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
func (c *Controller) reconcileCordonedNodes(ctx context.Context) {
	janitor, ok := c.platform.(platform.CordonJanitor)
	if !ok {
		return
	}
	nodes, err := janitor.CordonedNodes(ctx)
	if err != nil {
		c.log.Warn("listing cordoned nodes failed, will retry", "err", err)
		return
	}
	for _, node := range nodes {
		incidentID, parsed := incidentFromCordonReason(node.Reason)
		if !parsed {
			continue // cordoned by something else wearing our annotation
		}
		inc, err := c.store.GetIncident(ctx, incidentID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			// A store outage is not proof that the remediation has gone away.
			// Releasing this cordon would put an unverified node back into service.
			c.log.Warn("reading a cordon incident failed, keeping the node cordoned", "node", node.Name, "incident", incidentID, "err", err)
			continue
		}
		if errors.Is(err, store.ErrNotFound) || inc == nil {
			// The incident is gone but its cordon is not. Nothing will ever
			// come back for it, so release the node.
			c.uncordonAbandoned(ctx, node.Name, incidentID, "its incident no longer exists")
			continue
		}
		if isActiveIncidentState(inc.State) {
			continue
		}
		if inc.State == types.StateResolved {
			c.uncordonAbandoned(ctx, node.Name, incidentID, "its incident resolved without reaching the uncordon step")
			continue
		}
		c.reportStuckCordon(ctx, node.Name, inc)
	}
}

// resolveIncidentsOnVanishedNodes closes incidents whose node no longer exists.
//
// A node autoscaler can replace a node mid-playbook. The cordon now carries
// karpenter.sh/do-not-disrupt so that is far less likely, but a node can still
// go away — a spot reclaim, a manual delete, a node group roll — and the
// incident left behind targets hardware that no longer exists. It can never
// progress: its agent is gone, and its evidence is bound to a node UID nothing
// will ever match again. Closing it with that stated is the honest end;
// leaving it to age out as an approval timeout says nothing true.
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
		message := fmt.Sprintf("node %s no longer exists; the incident cannot be remediated and is closed as replaced", inc.Target.Node)
		c.log.Info("closing an incident whose node was replaced", "incident", inc.ID, "node", inc.Target.Node)
		// EXECUTING has no direct edge to RESOLVED in the state machine, and an
		// orphaned EXECUTING incident (no running step goroutine, skipped above)
		// on a deleted node would otherwise fail this transition and re-log the
		// rejection on every reconcile tick. Route it back through EVALUATING —
		// a valid edge — first, so the closure completes on a legal path.
		if inc.State == types.StateExecuting {
			if err := c.transition(ctx, inc, types.StateEvaluating, "system", "node-replaced",
				"node deleted while a step was orphaned mid-flight; abandoning it to close the incident", nil); err != nil {
				c.log.Warn("closing a replaced-node incident failed", "incident", inc.ID, "err", err)
				continue
			}
		}
		if err := c.transition(ctx, inc, types.StateResolved, "system", "node-replaced", message, nil); err != nil {
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

func (c *Controller) uncordonAbandoned(ctx context.Context, node, incidentID, why string) {
	if err := c.platform.Uncordon(ctx, node); err != nil {
		c.log.Warn("uncordoning an abandoned node failed, will retry",
			"node", node, "incident", incidentID, "err", err)
		return
	}
	c.log.Info("uncordoned a node left behind by a finished playbook",
		"node", node, "incident", incidentID, "reason", why)
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
