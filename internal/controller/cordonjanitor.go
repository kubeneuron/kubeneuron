// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// cordonReasonPrefix and the trailing "(incident-id)" are written by
// cordonReason below. The janitor parses the incident back out of it rather than
// carrying a second annotation, so the format is a contract between the two and
// is pinned by a test.
const cordonReasonPrefix = "kubeneuron: "

// cordonReason is the text recorded on a node when a playbook cordons it.
func cordonReason(inc *types.Incident) string {
	return fmt.Sprintf("%s%s (%s)", cordonReasonPrefix, inc.Class, inc.ID)
}

// incidentFromCordonOwner extracts the incident ID from one entry of a node's
// cordon owner set.
//
// An ordinary entry IS the incident ID: the executor writes inc.ID when it takes
// the node's cordon. A legacy entry stands in for a cordon placed before owner
// sets existed, which identified itself only by its reason, so that one is
// parsed the old way.
func incidentFromCordonOwner(owner string) (string, bool) {
	if reason, legacy := strings.CutPrefix(owner, platform.LegacyCordonOwnerPrefix); legacy {
		return incidentFromCordonReason(reason)
	}
	return owner, owner != ""
}

// incidentFromCordonReason extracts the incident ID from a recorded reason.
// Incident IDs contain no ")", so the last parenthesised group is unambiguous.
func incidentFromCordonReason(reason string) (string, bool) {
	if !strings.HasPrefix(reason, cordonReasonPrefix) {
		return "", false
	}
	open := strings.LastIndex(reason, "(")
	close := strings.LastIndex(reason, ")")
	if open < 0 || close < open+2 {
		return "", false
	}
	return reason[open+1 : close], true
}

// reconcileCordonedNodes clears cordons left behind by playbooks that finished,
// and surfaces the ones a human still owns.
//
// A node cordoned by a remediation that then died is invisible: nothing is
// running to finish the playbook, and the cluster has quietly lost capacity.
// This is the same reasoning as the automatic accelerator-stack restore, and it
// covers every failure path at once rather than each one remembering to clean up
// after itself.
//
// The split matters. A resolved incident means the remediation worked and only
// the final uncordon was missed, so the node goes back into service. An incident
// that ended in NEEDS_HUMAN or expired is *not* uncordoned: something decided
// the node was not fit, and putting it back silently would undo that judgement.
// Those are reported instead, once.
//
// It reports false when it could not list the cordoned nodes, so store
// retention does not take that pass as having seen them.
func (c *Controller) reconcileCordonedNodes(ctx context.Context) bool {
	janitor, ok := c.platform.(platform.CordonJanitor)
	if !ok {
		return true
	}
	c.clearUnknownCordonsReleasedByHand(ctx)
	nodes, err := janitor.CordonedNodes(ctx)
	if err != nil {
		c.log.Warn("listing cordoned nodes failed, will retry", "err", err)
		return false
	}
	for _, node := range nodes {
		// Every remediation holding this node, not just the one whose reason is
		// on it. On a multi-GPU node two incidents can hold one cordon, and the
		// reason annotation belongs to whichever cordoned LAST — so judging the
		// node by the reason alone both misses the abandoned hold whose reason
		// was overwritten and judges one incident's cordon by another's verdict.
		//
		// Holds() also covers the node cordoned by a build that predates owner
		// sets, which names its single holder by reason; asking it rather than
		// unfolding that rule here keeps this loop, the held-mark verdict and the
		// release path agreeing about the same node.
		for _, owner := range node.Holds() {
			c.reconcileCordonOwner(ctx, janitor, node, owner)
		}
	}
	return true
}

// reconcileCordonOwner decides what to do about ONE remediation's hold on a
// cordoned node: leave it alone, release it, or record that a human owns it.
func (c *Controller) reconcileCordonOwner(ctx context.Context, janitor platform.CordonJanitor, node platform.CordonedNode, owner string) {
	incidentID, parsed := incidentFromCordonOwner(owner)
	if !parsed {
		return // cordoned by something else wearing our annotation
	}
	inc, err := c.store.GetIncident(ctx, incidentID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		// A store outage is not proof that the remediation has gone away.
		// Releasing this cordon would put an unverified node back into service.
		c.log.Warn("reading a cordon incident failed, keeping the node cordoned", "node", node.Name, "incident", incidentID, "err", err)
		return
	}
	if errors.Is(err, store.ErrNotFound) || inc == nil {
		// The incident is gone but its cordon is not.
		//
		// Absence is not proof of abandonment. Retention prunes RESOLVED
		// and EXPIRED incidents alike, so an incident that timed out
		// awaiting approval — whose cordon this janitor deliberately
		// REFUSED to release while the row existed — was released the
		// moment that row was swept, inverting the rule stated at the top
		// of this file. A store restored from backup does the same for
		// every held cordon at once.
		//
		// The held mark lives on the node and outlives the row, so an
		// unreadable incident can no longer be mistaken for a resolved one.
		//
		// Asked about THIS hold, never about the node. A node-scoped answer says
		// "a human owns this" for every other remediation on the machine too, so
		// one halted remediation made every abandoned hold beside it permanent:
		// the owner set could never empty, and the mark blocking it is only
		// cleared when it does. The node deadlocks out of the fleet with no
		// incident row left to explain it.
		if node.HeldBy(owner) {
			c.log.Info("keeping a held cordon whose incident row is gone",
				"node", node.Name, "incident", incidentID)
			return
		}
		// Nor is it proof that the remediation finished. A store that never
		// held the row — a reinstall with a new store, a backup taken before the
		// incident opened — looks exactly like one that pruned it, and
		// releasing on that basis returned machines with real faults to the
		// scheduler. A live controller releases a resolved incident's cordon on
		// its next pass, long before retention prunes the row, so a missing row
		// with the cordon still in place is the unknown case: keep it and tell a
		// human.
		c.keepUnknownCordon(ctx, janitor, node, owner, incidentID)
		return
	}
	if isActiveIncidentState(inc.State) {
		return
	}
	if inc.State == types.StateResolved {
		c.uncordonAbandoned(ctx, node, owner, incidentID, "its incident resolved without reaching the uncordon step")
		return
	}
	// Halted but not resolved: a human owns this hold. Mark it, so the
	// decision survives the incident row that retention will prune.
	if !node.HeldBy(owner) {
		c.markCordonHeld(ctx, janitor, node, owner)
	}
	c.reportStuckCordon(ctx, node.Name, inc)
}

// keepUnknownCordon keeps a cordon whose incident this store has no record of,
// marks the hold held, and notifies once.
//
// Once is enforced on the node, not in memory: the platform reports a write
// only the first time it records this hold as unknown, so a restart does not
// repeat the notice.
func (c *Controller) keepUnknownCordon(ctx context.Context, janitor platform.CordonJanitor, node platform.CordonedNode, owner, incidentID string) {
	keeper, ok := c.platform.(platform.UnknownCordonKeeper)
	if !ok {
		// Fail closed all the same: the held mark alone stops every later pass
		// from releasing this hold.
		c.markCordonHeld(ctx, janitor, node, owner)
		return
	}
	marked, err := keeper.MarkCordonUnknownIfOwner(ctx, node.Name, owner)
	if err != nil {
		c.log.Warn("recording a cordon kept for an unknown incident failed, will retry",
			"node", node.Name, "incident", incidentID, "err", err)
		return
	}
	if !marked {
		return
	}
	withTaint := c.keepTaintWithCordon(ctx, node.Name, incidentID)
	c.log.Warn("keeping a cordon whose incident this store has no record of",
		"node", node.Name, "incident", incidentID)
	if c.notifier == nil {
		return
	}
	if err := c.notifier.Notify(ctx, notify.NotifyEvent{
		Kind: notify.EventNodeKept, Node: node.Name, IncidentID: incidentID,
		Message: unknownCordonMessage(node.Name, incidentID, withTaint),
	}); err != nil {
		// The record is what stops a second notice, so a notice that was not
		// sent must not leave it: forget it, and the next pass tries again.
		// The cordon stays either way.
		c.log.Warn("reporting a cordon kept for an unknown incident failed, will retry", "node", node.Name, "err", err)
		if err := keeper.ForgetUnknownCordon(ctx, node.Name, owner); err != nil {
			c.log.Warn("clearing the record of an unreported kept cordon failed", "node", node.Name, "err", err)
		}
		if withTaint {
			c.forgetUnknownTaint(ctx, node.Name)
		}
	}
}

// unknownCordonMessage is the notice for a kept cordon. withTaint covers a
// degraded taint kept for the same incident, so one notice names both.
func unknownCordonMessage(node, incidentID string, withTaint bool) string {
	release := "kubectl uncordon " + node
	if withTaint {
		release += " and kubectl taint nodes " + node + " kubeneuron.io/degraded-"
	}
	return fmt.Sprintf("Node %s carries a KubeNeuron cordon for incident %s, but this controller's store has no "+
		"record of that incident (for example after a reinstall or after restoring an older backup). "+
		"KubeNeuron keeps the cordon and will not remove it. Check the node; when it can return to service, "+
		"run %s", node, incidentID, release)
}

// clearUnknownCordonsReleasedByHand finishes what a human's `kubectl uncordon`
// started on a node whose cordon was kept for an unknown incident.
func (c *Controller) clearUnknownCordonsReleasedByHand(ctx context.Context) {
	keeper, ok := c.platform.(platform.UnknownCordonKeeper)
	if !ok {
		return
	}
	cleared, err := keeper.ClearUnknownCordonsReleasedByHand(ctx)
	for _, node := range cleared {
		c.log.Info("removed the cordon bookkeeping from a node a human uncordoned", "node", node)
	}
	if err != nil {
		c.log.Warn("clearing the cordon bookkeeping of uncordoned nodes failed, will retry", "err", err)
	}
}

// markCordonHeld records that a human owns this node's cordon, scoped to the
// hold the verdict was actually reached about.
//
// The scoping is the point, in both forms. The listing came from an informer
// cache, so a hold that has gone since then is not a missed one — it is a hold
// that has been RELEASED, and the held mark deliberately outlives the incident
// row, so a mark written from a stale verdict keeps a GPU node out of the fleet
// permanently once the row that explains it is pruned.
//
// On a shared cordon the reason cannot serve as that scope. It belongs to
// whichever incident cordoned LAST and stays on the node when any OTHER holder
// leaves, so it still matches long after the hold this verdict was reached about
// has gone — the write lands and nothing about the reason says it should not.
// The hold itself is the only thing that can be tested here.
func (c *Controller) markCordonHeld(ctx context.Context, janitor platform.CordonJanitor, node platform.CordonedNode, owner string) {
	var err error
	if node.Tracked() {
		_, err = janitor.MarkCordonHeldIfOwner(ctx, node.Name, owner)
	} else {
		_, err = janitor.MarkCordonHeldIfReason(ctx, node.Name, node.Reason)
	}
	if err != nil {
		c.log.Warn("recording a held cordon failed, will retry", "node", node.Name, "err", err)
	}
}

// resolveIncidentsOnVanishedNodes closes non-cloud incidents whose node no
// longer exists. Cloud recovery must instead verify the replacement: losing
// the old node alone says nothing about restored accelerator capacity.
func (c *Controller) resolveIncidentsOnVanishedNodes(ctx context.Context) {
	presence, ok := c.platform.(platform.NodePresence)
	if !ok {
		// A filtered inventory cannot establish deletion. Platforms without an
		// authoritative lookup leave incidents for a human rather than resolving
		// live hardware by mistake.
		return
	}
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{
		States: []types.IncidentState{
			types.StateOpen, types.StateObserving, types.StateEvaluating,
			types.StateAwaitingApproval, types.StateExecuting, types.StateVerifying,
			// NEEDS_HUMAN too: an incident parked for a human whose node has
			// since been deleted has nothing left to act on. Measured live — a
			// recycle on a managed node group left a NEEDS_HUMAN incident on a
			// node the ASG then replaced, and it sat there forever.
			types.StateNeedsHuman,
		},
	})
	if err != nil {
		return
	}
	for _, inc := range incidents {
		if inc.Target.Node == "" {
			continue
		}
		if c.isInFlight(inc.ID) {
			// A step goroutine still owns this incident. If the node is really
			// gone the step will fail against it and route the incident itself;
			// closing it from under a running step would only race that write.
			continue
		}
		exists, err := presence.NodeExists(ctx, inc.Target.Node)
		if err != nil {
			c.log.Warn("checking whether an incident node exists failed, keeping the incident open", "incident", inc.ID, "node", inc.Target.Node, "err", err)
			continue
		}
		if exists {
			continue
		}
		if pending, err := c.cloudRecoveryNeedsConfirmation(ctx, inc); err != nil || pending {
			// Deletion is not proof of a ready replacement. Leave the incident
			// to recovery verification, including when history cannot be read.
			c.log.Info("node disappeared; awaiting cloud recovery confirmation", "incident", inc.ID, "err", err)
			continue
		}
		message := fmt.Sprintf("node %s no longer exists; the incident cannot be remediated and is closed as replaced", inc.Target.Node)
		action := "node-replaced"
		if inc.RepairNodeUID == "" {
			// No inventory row named the node when the incident opened (a
			// mistyped or made-up name, or a node nothing had reported), so
			// nothing says it existed and was replaced.
			message = fmt.Sprintf("node %s is not in the cluster, and KubeNeuron had no record of it when the incident opened; the incident cannot be remediated and is closed", inc.Target.Node)
			action = "node-unknown"
		}
		c.log.Info("closing an incident whose node is not in the cluster", "incident", inc.ID, "node", inc.Target.Node)
		// EXECUTING has no direct edge to RESOLVED in the state machine, and an
		// orphaned EXECUTING incident (no running step goroutine, skipped above)
		// on a deleted node would otherwise fail this transition and re-log the
		// rejection on every reconcile tick. Route it back through EVALUATING —
		// a valid edge — first, so the closure completes on a legal path.
		if inc.State == types.StateExecuting {
			if err := c.transition(ctx, inc, types.StateEvaluating, "system", action,
				"node deleted while a step was orphaned mid-flight; abandoning it to close the incident", nil); err != nil {
				c.log.Warn("closing a replaced-node incident failed", "incident", inc.ID, "err", err)
				continue
			}
		}
		if err := c.transition(ctx, inc, types.StateResolved, "system", action, message, nil); err != nil {
			c.log.Warn("closing a replaced-node incident failed", "incident", inc.ID, "err", err)
			continue
		}
		if c.flap != nil {
			c.flap.RecordResolved(inc.Target, inc.Class)
		}
		if c.notifier != nil {
			_ = c.notifier.Notify(ctx, notify.NotifyEvent{
				Kind: notify.EventResolved, Incident: inc, Message: message,
			})
		}
	}
}

// uncordonAbandoned drops one finished remediation's hold on a node, and returns
// the node to service only if that was the last hold on it.
//
// The listing this decision came from is served from an informer cache. A
// stale entry is not a missed cordon — it is a cordon that has since been
// REPLACED: a node that resolved and immediately faulted again is cordoned by a
// NEW incident while the old reason is still cached, and releasing on that
// basis hands the scheduler a machine in the middle of its own drain. The
// sibling taint janitor already refuses to act on a two-read snapshot for
// exactly this reason and says so; this one had neither the re-read nor the
// conditional write.
//
// The same staleness argument is why the owner-set release is compare-and-swap
// rather than a read-modify-write, and why an owner that is no longer on the
// node is a silent no-op: this janitor runs on every reconcile tick and must be
// able to say the same thing twice without consequence.
func (c *Controller) uncordonAbandoned(ctx context.Context, node platform.CordonedNode, owner, incidentID, why string) {
	{
		// Counting holders is required of every platform now, so this asks for
		// nothing extra. It used to assert platform.CordonOwnership, which also
		// demands MarkCordonHeldIfOwner — so an adapter that implemented the
		// required pair but not the held mark fell through to the reason-only
		// release and lost owner counting entirely. The janitor would then
		// release one resolved hold and make a node schedulable while another
		// GPU remediation still owned it: the P0 this whole mechanism exists to
		// prevent, reachable through a capability check that no longer matched
		// what the code needs.
		if c.platform == nil {
			// Only reachable from a test fixture that builds a controller
			// without one. Stated explicitly because the type assertion this
			// replaced used to absorb it: an assertion on a nil interface
			// simply reports "not supported", so removing the assertion also
			// removed a guard nobody had written down.
			return
		}
		released, remaining, err := c.platform.ReleaseCordonOwners(ctx, node.Name, []string{owner})
		if err != nil {
			c.log.Warn("releasing an abandoned remediation's hold on a node failed, will retry",
				"node", node.Name, "incident", incidentID, "err", err)
			return
		}
		if !released {
			// Either the hold was already gone (a replaced cordon, or a release
			// that has since landed) or other remediations still hold this node.
			// Both mean the node stays cordoned, which is the fail-closed answer.
			c.log.Info("did not return a node to service: it is still held",
				"node", node.Name, "incident", incidentID, "remaining_holders", remaining)
			return
		}
		c.log.Info("uncordoned a node left behind by a finished playbook",
			"node", node.Name, "incident", incidentID, "reason", why)
		return
	}
}

// reportStuckCordon tells an operator once that a node is out of service.
//
// Repeating it on every reconcile tick would train people to ignore it, so the
// notice is remembered in memory. A controller restart repeats it, which is the
// right way round: better a duplicate than a node nobody knows about.
func (c *Controller) reportStuckCordon(ctx context.Context, node string, inc *types.Incident) {
	if c.notifier == nil || !c.markCordonReported(node, inc.ID) {
		return
	}
	message := fmt.Sprintf(
		"node %s is still cordoned: incident %s ended in %s and was not uncordoned. "+
			"Uncordon it once the node is fit, or resolve the incident.",
		node, inc.ID, inc.State)
	c.log.Warn("node left cordoned by an unresolved incident", "node", node, "incident", inc.ID, "state", inc.State)
	if err := c.notifier.Notify(ctx, notify.NotifyEvent{
		Kind: notify.EventNeedsHuman, Incident: inc, Message: message,
	}); err != nil {
		c.log.Warn("reporting a stuck cordon failed", "node", node, "err", err)
		c.forgetCordonReported(node, inc.ID)
	}
}

// cordonReportedKeys remembers which stuck cordons have already been reported.
type cordonReportedKeys struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (c *Controller) markCordonReported(node, incidentID string) bool {
	c.cordonReported.mu.Lock()
	defer c.cordonReported.mu.Unlock()
	if c.cordonReported.seen == nil {
		c.cordonReported.seen = map[string]bool{}
	}
	key := node + "/" + incidentID
	if c.cordonReported.seen[key] {
		return false
	}
	c.cordonReported.seen[key] = true
	return true
}

func (c *Controller) forgetCordonReported(node, incidentID string) {
	c.cordonReported.mu.Lock()
	defer c.cordonReported.mu.Unlock()
	delete(c.cordonReported.seen, node+"/"+incidentID)
}

// cloudRecoveryNeedsConfirmation prevents janitor cleanup from standing in for
// recovery verification. A durable start is enough to require confirmation:
// a controller crash may leave the cloud request's outcome unknown. Historical
// audit records keep this protection when a playbook is changed or removed.
func (c *Controller) cloudRecoveryNeedsConfirmation(ctx context.Context, inc *types.Incident) (bool, error) {
	if inc.Playbook != "" {
		engine := c.runtimeConfig(ctx).Engine
		if engine != nil {
			if book, ok := engine.Playbook(inc.Playbook); ok {
				for _, step := range book.Steps {
					if cloudRecoveryAction(step.Action) {
						return true, nil
					}
				}
			}
		}
	}
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil {
		return true, err
	}
	for _, entry := range trail {
		for _, wire := range []string{"platform.recycle_node", "platform.replace_node"} {
			if entry.Result == auditStepStartResult(wire) ||
				entry.Result == auditExecutingResult(wire) ||
				entry.Result == auditStepOutcomeResult(wire, false, true) ||
				entry.Result == auditStepOutcomeResult(wire, false, false) {
				return true, nil
			}
		}
	}
	return false, nil
}

func cloudRecoveryAction(wire string) bool {
	return wire == "platform.recycle_node" || wire == "platform.replace_node"
}
