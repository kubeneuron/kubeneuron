// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// What holds a repeat a sweep would send for a step that no longer runs.
const (
	holdOther     = "other"
	holdPause     = "pause"
	holdNodePause = "node-pause"
	holdConfig    = "config"
	holdDryRun    = "dry-run"
	holdApproval  = "approval"
)

// repeatHold is why a repeat may not be sent now; kind is "" when it may.
type repeatHold struct {
	kind   string
	policy string
	// config is the text of a published configuration not yet loaded.
	config string
	// why is the check that held a repeat of kind holdOther, for the log.
	why string
}

// repeatHold decides a repeat of a remove-first mutation as a new one: the
// step that was allowed to make it no longer runs, so its approval alone
// does not allow it again. It is sent only from one of states. The caller
// holds runtimeMu for reading until the repeat is sent.
func (c *Controller) repeatHold(ctx context.Context, inc *types.Incident, states ...types.IncidentState) (repeatHold, error) {
	rec := inc.RemoveFirst
	config := c.runtimeConfig(ctx)
	if config != c.runtime.Load() || config.Engine == nil {
		return repeatHold{kind: holdOther, why: "the runtime configuration is being replaced"}, nil
	}
	// What holds the incident's step holds the repeat too, in any state.
	if c.gate.Paused() {
		return repeatHold{kind: holdPause}, nil
	}
	if c.pauseHolds(ctx, inc) {
		return repeatHold{kind: holdNodePause}, nil
	}
	if reason, _ := c.unloadedConfig(ctx); reason != "" {
		return repeatHold{kind: holdConfig, config: reason}, nil
	}
	pol, ok := config.Engine.PolicyFor(inc.Class, inc.Vendor)
	if ok && pol.Mode.Effective() == recoverymode.DryRun {
		return repeatHold{kind: holdDryRun, policy: pol.Name}, nil
	}
	pinned := inc.CloudIdentity != nil && (rec.DeleteTarget == nil && rec.Instance == inc.CloudIdentity.InstanceID ||
		rec.DeleteTarget != nil && *inc.CloudIdentity == *rec.DeleteTarget)
	if !pinned {
		return repeatHold{kind: holdOther, why: "the record does not pin the incident's cloud node"}, nil
	}
	if rec.Operation != successorOperation(inc) {
		return repeatHold{kind: holdOther, why: "the record belongs to another attempt"}, nil
	}
	if ok && pol.Mode.Effective() == recoverymode.Approval {
		// The repeat needs an approval no step can ask for any more: the
		// step's node is gone.
		return repeatHold{kind: holdApproval, policy: pol.Name}, nil
	}
	if !slices.Contains(states, inc.State) {
		return repeatHold{kind: holdOther, why: "the incident is " + string(inc.State)}, nil
	}
	if c.effectiveDryRun(ctx, inc) {
		return repeatHold{kind: holdOther, why: "the incident runs in dry-run"}, nil
	}
	step, done, err := config.Engine.NextStep(inc)
	if err != nil || done || step == nil || step.Action != "platform.replace_node" {
		return repeatHold{kind: holdOther, why: "the incident's next step is not the node replacement"}, err
	}
	pol, ok = c.automaticPolicyFor(ctx, inc)
	if !ok || pol.ReplacementStrategy.Effective() != recoverymode.RemoveFirst {
		return repeatHold{kind: holdOther, why: "no automatic remove-first policy applies"}, nil
	}
	if verdict, reason := c.authorizeByPolicy(ctx, inc, step, pol); verdict != policyRun {
		return repeatHold{kind: holdOther, why: "policy " + pol.Name + " does not run the step now: " + reason}, nil
	}
	if reason, result := c.destructiveStepConfinement(ctx, inc, step); result != confinementAllowed {
		return repeatHold{kind: holdOther, why: "the step is not confined: " + reason}, nil
	}
	if hold, skip, _ := c.policyRecoveryPreflight(ctx, inc, step); hold != "" || skip != "" {
		return repeatHold{kind: holdOther, why: "the recovery preflight holds the step: " + hold + skip}, nil
	}
	return repeatHold{}, nil
}

// logHeld writes one controller log line for each new reason a repeat of
// what (a delete or a resize) is not sent for.
func (c *Controller) logHeld(inc *types.Incident, what string, hold repeatHold) {
	why := hold.why
	if why == "" {
		why = hold.kind
	}
	if c.logOnce.first("remove-first-held/" + inc.ID + "/" + what + "/" + why) {
		c.log.Info("remove-first repeat not sent", "incident", inc.ID, "operation", inc.RemoveFirst.Operation, "repeat", what, "reason", why)
	}
}

// heldFor is what a held resize waits for, as status and pools print it,
// and the incident's sentences after the size; "" for a hold with nothing
// the operator is asked to do.
func (h repeatHold) heldFor(inc *types.Incident) (waitingFor, text string) {
	node := inc.Target.Node
	switch h.kind {
	case holdPause:
		return "remediation to be resumed", "Setting it back is waiting for remediation to be resumed. To restore capacity: run kubeneuronctl resume."
	case holdNodePause:
		return fmt.Sprintf("the pause on node %s to be lifted", node),
			fmt.Sprintf("Setting it back is waiting for the pause on node %s to be lifted. To restore capacity: remove paused: true from the GPUNodeConfig of node %s.", node, node)
	case holdConfig:
		return "the published configuration to be loaded", "held: " + h.config
	case holdDryRun:
		return fmt.Sprintf("policy %s to leave DryRun", h.policy),
			fmt.Sprintf("Setting it back is waiting for policy %s to leave DryRun. To restore capacity: set the mode of policy %s to Approval or Automatic.", h.policy, h.policy)
	}
	return "", ""
}

func (c *Controller) observePoolSize(ctx context.Context, inc *types.Incident) (removefirst.PoolSize, bool) {
	observer, ok := c.platform.(interface {
		ObservePinnedPoolSize(context.Context, cloud.RemoveFirstPlan) (removefirst.PoolSize, error)
	})
	if !ok {
		return removefirst.PoolSize{}, false
	}
	readCtx, cancel := context.WithTimeout(ctx, successorSweepTimeout)
	defer cancel()
	size, err := observer.ObservePinnedPoolSize(readCtx, inc.RemoveFirst.RemoveFirstPlan)
	if err != nil {
		c.log.Warn("reading the size of a pool with a held resize failed", "incident", inc.ID, "pool", inc.RemoveFirst.Pool, "err", err)
		return removefirst.PoolSize{}, false
	}
	return size, true
}

// sweepResize continues a plan whose resize may be sent next. A resize the
// sweep sends is a new mutation: it is held unless the policy would start
// the step now. A held resize is not sent; the pool's size is read, and the
// record notes what it waits for. A pool set back by someone else is read
// as such and the plan ends, with nothing sent.
func (c *Controller) sweepResize(ctx context.Context, inc *types.Incident, plat platform.PlannedRemoveFirstRecycler) error {
	c.runtimeMu.RLock()
	defer c.runtimeMu.RUnlock()
	// The step's node is gone, so the step is not decided again after a
	// restart: only the sweep sets the size back.
	hold, err := c.repeatHold(ctx, inc, types.StateExecuting, types.StateEvaluating)
	if err != nil {
		return err
	}
	rec := inc.RemoveFirst
	if hold.kind != "" {
		c.logHeld(inc, "resize", hold)
	}
	if hold.kind == "" {
		if rec.State[types.RemoveFirstHeldFor] != "" {
			next := rec.Clone()
			for _, k := range []string{types.RemoveFirstHeldFor, types.RemoveFirstHeldCurrent, types.RemoveFirstHeldPlanned} {
				delete(next.State, k)
			}
			if err := c.saveRemoveFirst(ctx, inc, next, "remove-first-hold-lifted",
				fmt.Sprintf("operation %s: setting node pool %s back to its size", rec.Operation, rec.Pool)); err != nil {
				return err
			}
		}
		return c.continueRemoveFirst(ctx, inc, plat, *inc.CloudIdentity)
	}
	size, read := c.observePoolSize(ctx, inc)
	if read && size.Current >= size.Planned {
		// No zone is short: continuing ends the plan. It reads the pool
		// again, and must not send the held resize if a zone is short by
		// then.
		return c.continueRemoveFirst(removefirst.WithoutSend(ctx), inc, plat, *inc.CloudIdentity)
	}
	if hold.kind == holdApproval {
		return c.parkUnapprovableResize(ctx, inc, hold, size, read)
	}
	if time.Since(rec.PlannedAt) > removeFirstDeadline {
		if !read {
			return c.removeFirstOverdue(ctx, inc, rec)
		}
		return c.parkRemoveFirst(ctx, inc, fmt.Sprintf("Pool %s has had %d of %d nodes for more than an hour: node %s was removed and the pool size was not set back.",
			rec.Pool, size.Current, size.Planned, inc.Target.Node))
	}
	waitingFor, text := hold.heldFor(inc)
	if !read || waitingFor == "" {
		return nil
	}
	current, planned := strconv.Itoa(int(size.Current)), strconv.Itoa(int(size.Planned))
	if rec.State[types.RemoveFirstHeldFor] == waitingFor && rec.State[types.RemoveFirstHeldCurrent] == current && rec.State[types.RemoveFirstHeldPlanned] == planned {
		return nil
	}
	next := rec.Clone()
	next.State[types.RemoveFirstHeldFor] = waitingFor
	next.State[types.RemoveFirstHeldCurrent] = current
	next.State[types.RemoveFirstHeldPlanned] = planned
	return c.saveRemoveFirst(ctx, inc, next, "remove-first-held",
		fmt.Sprintf("Pool %s has %d of %d nodes: node %s was removed and the pool size has not been set back yet. %s", rec.Pool, size.Current, size.Planned, inc.Target.Node, text))
}

// parkUnapprovableResize hands a pool to an operator at once when its resize
// needs an approval that can no longer be requested.
func (c *Controller) parkUnapprovableResize(ctx context.Context, inc *types.Incident, hold repeatHold, size removefirst.PoolSize, read bool) error {
	rec := inc.RemoveFirst
	reason := fmt.Sprintf("Node %s was removed from pool %s and the pool size was not set back.", inc.Target.Node, rec.Pool)
	planned := "its size before the replacement"
	if read {
		reason = fmt.Sprintf("Pool %s has %d of %d nodes: node %s was removed and the pool size was not set back.", rec.Pool, size.Current, size.Planned, inc.Target.Node)
		planned = fmt.Sprintf("%d nodes", size.Planned)
	}
	h := handover{
		What:    reason + fmt.Sprintf(" Policy %s requires approval for this step, and the approval can no longer be requested because the step was interrupted after the node was removed.", hold.policy),
		Check:   fmt.Sprintf("Set the size of pool %s back to %s, then run", rec.Pool, planned),
		Command: settlementCommand(inc.ID),
	}
	reason = h.message()
	if inc.State != types.StateNeedsHuman {
		if err := c.quarantineAfterStep(ctx, inc, h, false); err != nil {
			return err
		}
	}
	if read {
		// status and pools show the pool's size with the operator's part.
		next := inc.RemoveFirst.Clone()
		next.State[types.RemoveFirstHeldFor] = types.RemoveFirstHeldForOperator
		next.State[types.RemoveFirstHeldCurrent] = strconv.Itoa(int(size.Current))
		next.State[types.RemoveFirstHeldPlanned] = strconv.Itoa(int(size.Planned))
		inc.RemoveFirst = next
	}
	return c.endRemoveFirst(ctx, inc, types.RemoveFirstNeedsOperator, reason)
}

// heldResizeNotes are the notes of held resizes by pool, as the last sweep
// read them from the incident records.
type heldResizeNotes struct {
	mu    sync.Mutex
	pools map[string][]string
}

// noteHeldResizes keeps the notes of the open records the sweep went over,
// as it left them.
func (c *Controller) noteHeldResizes(open []*types.Incident) {
	pools := map[string][]string{}
	seen := map[string]bool{}
	// The last read of an incident wins.
	for _, inc := range slices.Backward(open) {
		if seen[inc.ID] {
			continue
		}
		seen[inc.ID] = true
		if note := types.RemoveFirstHeldNote(inc); note != "" {
			pools[inc.RemoveFirst.Pool] = append(pools[inc.RemoveFirst.Pool], note)
		}
	}
	c.held.mu.Lock()
	defer c.held.mu.Unlock()
	c.held.pools = pools
}

func (c *Controller) heldResizes() map[string][]string {
	c.held.mu.Lock()
	defer c.held.mu.Unlock()
	return c.held.pools
}
