// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Automatic recovery is an approval given in advance: a named policy,
// enabled by a named person, authorises the steps a person would otherwise
// approve one by one. It is never the absence of an approval. Every step it
// authorises carries the actor "policy:<name>", and the audit trail says who
// enabled the policy and when.
//
// The policy is read live at every step: switching it back to approval
// mid-incident parks the next step, and a limit that is reached holds the
// step until it clears. Whatever the policy says, startStep's own checks
// (pause, maintenance window, blast radius, gates) still run.

const policyActorPrefix = "policy:"

func policyActor(name string) string { return policyActorPrefix + name }

type policyVerdict int

const (
	// policyPark hands the step to a person: the policy does not cover it.
	policyPark policyVerdict = iota
	// policyHold keeps the step waiting: the policy covers it, a limit is
	// reached for now.
	policyHold
	// policyRun authorises the step.
	policyRun
)

// automaticPolicyFor returns the live policy of the incident when it is in
// automatic mode.
func (c *Controller) automaticPolicyFor(ctx context.Context, inc *types.Incident) (playbook.Policy, bool) {
	engine := c.runtimeConfig(ctx).Engine
	if engine == nil {
		return playbook.Policy{}, false
	}
	pol, ok := engine.PolicyFor(inc.Class, inc.Vendor)
	if !ok || pol.Mode != recoverymode.Automatic || pol.Automatic == nil || pol.Name == "" {
		return playbook.Policy{}, false
	}
	return pol, true
}

// authorizeByPolicy decides whether an automatic policy authorises the next
// step of an incident now.
func (c *Controller) authorizeByPolicy(ctx context.Context, inc *types.Incident, step *playbook.Step, pol playbook.Policy) (policyVerdict, string) {
	auto := pol.Automatic
	if nodeLevelAction(step) {
		return policyPark, nodeLevelAlphaReason
	}
	labels, err := c.nodeLabelsForConfinement(ctx, inc.Target.Node)
	if err != nil {
		return policyPark, fmt.Sprintf("the node's labels cannot be read (%v)", err)
	}
	if !auto.SelectsNode(labels) {
		return policyPark, "the node is outside the policy's node pools"
	}
	// The provider's eligibility for the pinned node has already been
	// established at the boundary, before this is asked, but only on a cloud
	// recovery ladder. Anywhere else nothing confirms that the cloud's own
	// repair will not act on the same node, so a person decides.
	if !c.cloudRecoveryLadder(ctx, inc.Playbook) {
		return policyPark, "automatic policies authorise only cloud recovery ladders, whose eligibility the cloud provider confirms"
	}
	if tripped, err := c.breakerTripped(ctx, pol); err != nil {
		return policyPark, fmt.Sprintf("the circuit breaker state cannot be read (%v)", err)
	} else if tripped {
		return policyPark, "the circuit breaker tripped for this revision of the policy; change the policy to return it to automatic"
	}
	pool := auto.Pool(labels)

	open, err := c.openIncidents(ctx)
	if err != nil {
		return policyHold, fmt.Sprintf("open incidents cannot be read (%v)", err)
	}
	nodes, err := c.store.ListNodes(ctx)
	if err != nil {
		return policyHold, fmt.Sprintf("the node inventory cannot be read (%v)", err)
	}
	// Circuit breaker: too large a share of the pool is unhealthy at once.
	// That looks like a monitoring fault or a shared cause, not N broken
	// nodes, so a person decides.
	poolNodes := map[string]bool{}
	for _, n := range nodes {
		if auto.SelectsNode(n.Labels) && auto.Pool(n.Labels) == pool {
			poolNodes[n.Name] = true
		}
	}
	// Unhealthy: a node of the pool with an open incident, or one the
	// platform reports not Ready.
	unhealthy := map[string]bool{}
	for _, other := range open {
		if poolNodes[other.Target.Node] {
			unhealthy[other.Target.Node] = true
		}
	}
	if readiness, ok := c.platform.(nodeReadiness); ok {
		for name := range poolNodes {
			if unhealthy[name] {
				continue
			}
			ready, err := readiness.NodeReady(ctx, name)
			if err != nil {
				// An unreadable pool looks like a shared cause, which is
				// exactly what the breaker exists for: never read it as healthy.
				return policyHold, fmt.Sprintf("the readiness of pool node %s cannot be read (%v)", name, err)
			}
			if !ready {
				unhealthy[name] = true
			}
		}
	}
	if len(poolNodes) > 0 && len(unhealthy) >= auto.MinUnhealthy() && len(unhealthy)*100 > auto.MaxUnhealthyPercent*len(poolNodes) {
		reason := fmt.Sprintf("circuit breaker: %d of %d nodes in the pool are unhealthy, above %d%%",
			len(unhealthy), len(poolNodes), auto.MaxUnhealthyPercent)
		if err := c.tripBreaker(ctx, pol, inc, reason); err != nil {
			c.log.Error("recording the circuit breaker trip failed", "policy", pol.Name, "err", err)
		}
		return policyPark, reason
	}

	// Concurrency: nodes this policy is already recovering.
	actor := policyActor(pol.Name)
	total, inPool := 0, 0
	for _, other := range open {
		if other.ID == inc.ID || other.Target.Node == inc.Target.Node {
			continue
		}
		authorised, err := c.authorisedBy(ctx, other.ID, actor, time.Time{})
		if err != nil {
			return policyHold, fmt.Sprintf("audit trail cannot be read (%v)", err)
		}
		if !authorised {
			continue
		}
		total++
		if poolNodes[other.Target.Node] {
			inPool++
		}
	}
	if total >= auto.MaxConcurrentNodes {
		return policyHold, fmt.Sprintf("the policy is already recovering %d node(s), its limit", total)
	}
	if auto.MaxConcurrentNodesPerPool > 0 && inPool >= auto.MaxConcurrentNodesPerPool {
		return policyHold, fmt.Sprintf("the policy is already recovering %d node(s) in this pool, its limit", inPool)
	}

	// Cool-down: this node was recovered automatically too recently.
	since := time.Now().Add(-auto.NodeCooldown)
	recent, err := c.store.ListIncidents(ctx, store.IncidentFilter{Node: inc.Target.Node, ActiveSince: since})
	if err != nil {
		return policyHold, fmt.Sprintf("the node's recent incidents cannot be read (%v)", err)
	}
	for _, other := range recent {
		if other.ID == inc.ID {
			continue
		}
		authorised, err := c.authorisedBy(ctx, other.ID, actor, since)
		if err != nil {
			return policyHold, fmt.Sprintf("audit trail cannot be read (%v)", err)
		}
		if authorised {
			return policyHold, fmt.Sprintf("the node was recovered by this policy less than %s ago", auto.NodeCooldown)
		}
	}
	return policyRun, ""
}

// openIncidents lists the incidents automation is still working on.
func (c *Controller) openIncidents(ctx context.Context) ([]*types.Incident, error) {
	return c.store.ListIncidents(ctx, store.IncidentFilter{States: []types.IncidentState{
		types.StateOpen, types.StateObserving, types.StateEvaluating,
		types.StateAwaitingApproval, types.StateExecuting, types.StateVerifying,
	}})
}

// authorisedBy reports whether a step of the incident was started under actor
// at or after since: an entry into EXECUTING by that actor. Other rows the
// policy writes (a held step, the authorisation note) do not count, or a held
// incident would itself use up the limit that holds it.
func (c *Controller) authorisedBy(ctx context.Context, incidentID, actor string, since time.Time) (bool, error) {
	trail, err := c.store.AuditTrail(ctx, incidentID)
	if err != nil {
		return false, err
	}
	for _, e := range trail {
		if e.Actor == actor && e.ToState == types.StateExecuting && e.FromState != types.StateExecuting && !e.Time.Before(since) {
			return true, nil
		}
	}
	return false, nil
}

// policyAuthorisationRefusal is the defence in depth behind authorizeAndStartStep:
// a step that needs approval may start only under a person's decision or under
// the live automatic policy named by its actor. It returns "" when the step
// may start.
func (c *Controller) policyAuthorisationRefusal(ctx context.Context, inc *types.Incident, step *playbook.Step, actor string) string {
	if !step.NeedsApproval() {
		return ""
	}
	if actor == "" || actor == "system" {
		return "this step needs a person's approval or an automatic policy; none authorised it"
	}
	name, isPolicy := strings.CutPrefix(actor, policyActorPrefix)
	if !isPolicy {
		return "" // a person's recorded decision
	}
	if nodeLevelAction(step) {
		return nodeLevelAlphaReason
	}
	pol, ok := c.automaticPolicyFor(ctx, inc)
	if !ok || pol.Name != name {
		return fmt.Sprintf("policy %q is not the incident's automatic policy any more", name)
	}
	return ""
}

// policyAuthorisationNote is the audit text of an automatic step. EnabledBy
// and EnabledAt are what the policy declares; they are recorded as declared.
func policyAuthorisationNote(pol playbook.Policy, now time.Time) string {
	return fmt.Sprintf("authorised by policy %s: automatic mode declared enabled by %s at %s in generation %d, observed at %s",
		pol.Name, pol.Automatic.EnabledBy, pol.Automatic.EnabledAt.UTC().Format(time.RFC3339),
		pol.Automatic.Generation, now.UTC().Format(time.RFC3339))
}

// nodeReadiness is the optional platform capability the breaker reads.
type nodeReadiness interface {
	NodeReady(ctx context.Context, node string) (bool, error)
}

const breakerTripAction = "circuit-breaker-tripped"

// tripBreaker records, durably, that the policy's breaker tripped for its
// current revision. It stays tripped until the policy changes.
func (c *Controller) tripBreaker(ctx context.Context, pol playbook.Policy, inc *types.Incident, reason string) error {
	ops, ok := c.store.(store.OperationalStore)
	if !ok {
		return fmt.Errorf("the store keeps no operational audit")
	}
	return ops.AppendOperationalAudit(ctx, &types.OperationalAuditEvent{
		Kind: types.ResourceRecoveryPolicy, ResourceID: pol.Name, Time: time.Now().UTC(),
		Actor: "system", Action: breakerTripAction, Result: reason,
		Params: map[string]string{"revision": pol.Automatic.TripKey(), "incident": inc.ID},
	})
}

// breakerTripped reports whether the breaker tripped for this policy revision.
// A store without an operational audit cannot remember a trip, so it answers
// tripped: the policy then asks a person, which is the safe direction.
func (c *Controller) breakerTripped(ctx context.Context, pol playbook.Policy) (bool, error) {
	ops, ok := c.store.(store.OperationalStore)
	if !ok {
		return true, nil
	}
	events, err := ops.ListOperationalAudit(ctx, types.ResourceRecoveryPolicy, pol.Name, 0)
	if err != nil {
		return false, err
	}
	for _, e := range events {
		if e.Action == breakerTripAction && e.Params["revision"] == pol.Automatic.TripKey() {
			return true, nil
		}
	}
	return false, nil
}

// logOnceStepUnavailable records why a step is held as unavailable: one log
// line per incident, step and reason, and an audit row whenever the denial
// differs from the last policy-scope assessment, not one per tick.
func (c *Controller) logOnceStepUnavailable(inc *types.Incident, step *playbook.Step, reason string) {
	if c.logOnce.first("step-unavailable/" + inc.ID + "/" + step.Name + "/" + reason) {
		c.log.Warn("step held: recovery unavailable", "incident", inc.ID, "step", step.Name, "reason", reason)
	}
	if err := c.recordRecoveryDenied(context.Background(), inc, step.Name+": "+reason); err != nil {
		c.log.Error("recovery-unavailable audit append failed", "incident", inc.ID, "err", err)
	}
}

// nodeLevelAlphaReason: commands run on the node itself (GPU reset, reboot,
// driver reload or reinstall, scripts) are alpha, not validated on real
// hardware. While they are, no automatic policy may authorise them; only a
// person's approval can.
const nodeLevelAlphaReason = "node-level actions are alpha (not validated on real hardware) and need a person's approval; automatic policies cannot authorise them"

// nodeLevelAction reports whether the step runs a destructive command on the
// node through the agent.
func nodeLevelAction(step *playbook.Step) bool {
	def, ok := action.ByWire(step.Action)
	return ok && def.Kind == action.KindAgent && def.AgentDestructive
}

// logOnceStepHeld records why an automatic policy holds a step (a limit, the
// cool-down, a pool that cannot be read): one log line and one audit row per
// incident, step and reason, not one per tick.
func (c *Controller) logOnceStepHeld(inc *types.Incident, step *playbook.Step, pol playbook.Policy, reason string) {
	if !c.logOnce.first("step-held/" + inc.ID + "/" + step.Name + "/" + reason) {
		return
	}
	c.log.Info("automatic policy holds the step", "incident", inc.ID, "policy", pol.Name, "step", step.Name, "reason", reason)
	if err := c.appendAudit(context.Background(), inc, "system", "recovery-held", step.Name+" held by policy "+pol.Name+": "+reason); err != nil {
		c.log.Error("recovery-held audit append failed", "incident", inc.ID, "err", err)
	}
}
