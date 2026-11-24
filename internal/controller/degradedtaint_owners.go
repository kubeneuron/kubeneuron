// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
)

// The degraded taint comes off a node only when this store knows the incidents
// that placed it. A controller with a new store, or one restored from an older
// backup, has no open incident for any node; removing every taint on that
// basis returned nodes with a real fault to the scheduler. Such a taint is
// kept and reported once, like the cordon.

// taintVerdict is what the janitor may do with a taint no open incident holds.
type taintVerdict int

const (
	// taintRemovable: the store knows the incidents behind the taint.
	taintRemovable taintVerdict = iota
	// taintUnknown: the store has no record of one of them. Keep and report.
	taintUnknown
	// taintKept: already kept for a human. Leave it alone, silently.
	taintKept
)

func (c *Controller) taintOwnership() platform.DegradedTaintOwnership {
	ownership, ok := c.platform.(platform.DegradedTaintOwnership)
	if !ok {
		return nil
	}
	return ownership
}

// closedTaintVerdict decides whether the taint on a node may come off. It
// returns the incident the store has no record of, when the record names one.
func (c *Controller) closedTaintVerdict(ctx context.Context, node string) (taintVerdict, string, error) {
	ownership := c.taintOwnership()
	if ownership == nil {
		return taintRemovable, "", nil
	}
	owners, _, unknown, err := ownership.DegradedTaintOwners(ctx, node)
	if err != nil {
		return 0, "", err
	}
	if unknown {
		return taintKept, "", nil
	}
	if len(owners) > 0 {
		for _, id := range owners {
			if _, err := c.store.GetIncident(ctx, id); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return taintUnknown, id, nil
				}
				return 0, "", err
			}
		}
		return taintRemovable, "", nil
	}
	// A taint placed before owners were recorded. Any record of an incident
	// on the node means this store placed it and that incident has closed.
	known, err := c.storeKnowsNode(ctx, node)
	if err != nil {
		return 0, "", err
	}
	if known {
		return taintRemovable, "", nil
	}
	return taintUnknown, "", nil
}

func (c *Controller) storeKnowsNode(ctx context.Context, node string) (bool, error) {
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{Node: node, Limit: 1})
	if err != nil {
		return false, err
	}
	return len(incidents) > 0, nil
}

// releaseClosedTaint removes the taint from a node no open incident holds, or
// keeps it when the store does not know what placed it.
func (c *Controller) releaseClosedTaint(ctx context.Context, tainter platform.NodeTainter, node, why string) {
	verdict, incidentID, err := c.closedTaintVerdict(ctx, node)
	if err != nil {
		c.log.Warn("cannot tell whether the store knows the incidents behind a degraded taint; leaving it",
			"node", node, "err", err)
		return
	}
	switch verdict {
	case taintRemovable:
		c.removeDegradedTaint(ctx, tainter, node, why)
	case taintUnknown:
		c.keepUnknownTaint(ctx, node, incidentID)
	}
}

// keepUnknownTaint marks the taint kept and notifies once. Once is enforced on
// the node, as for the cordon.
func (c *Controller) keepUnknownTaint(ctx context.Context, node, incidentID string) {
	marked, err := c.taintOwnership().MarkDegradedTaintUnknown(ctx, node)
	if err != nil {
		c.log.Warn("recording a degraded taint kept for an unknown incident failed, will retry", "node", node, "err", err)
		return
	}
	if !marked {
		return
	}
	c.log.Warn("keeping a degraded taint whose incident this store has no record of", "node", node, "incident", incidentID)
	if c.notifier == nil {
		return
	}
	if err := c.notifier.Notify(ctx, notify.NotifyEvent{
		Kind: notify.EventNodeKept, Node: node, IncidentID: incidentID, Message: unknownTaintMessage(node),
	}); err != nil {
		// As for the cordon: a notice not sent must not leave the record that
		// stops the next one.
		c.log.Warn("reporting a degraded taint kept for an unknown incident failed, will retry", "node", node, "err", err)
		c.forgetUnknownTaint(ctx, node)
	}
}

func (c *Controller) forgetUnknownTaint(ctx context.Context, node string) {
	if err := c.taintOwnership().ForgetDegradedTaintUnknown(ctx, node); err != nil {
		c.log.Warn("clearing the record of an unreported kept taint failed", "node", node, "err", err)
	}
}

// keepTaintWithCordon marks the node's taint kept along with a cordon kept for
// the same unknown incident, so the one cordon notice covers both. It reports
// whether it did.
func (c *Controller) keepTaintWithCordon(ctx context.Context, node, incidentID string) bool {
	ownership := c.taintOwnership()
	if ownership == nil {
		return false
	}
	owners, _, unknown, err := ownership.DegradedTaintOwners(ctx, node)
	if err != nil || unknown {
		return false
	}
	if len(owners) > 0 {
		if !slices.Contains(owners, incidentID) {
			return false
		}
	} else if known, err := c.storeKnowsNode(ctx, node); err != nil || known {
		return false
	}
	marked, err := ownership.MarkDegradedTaintUnknown(ctx, node)
	if err != nil {
		c.log.Warn("recording a degraded taint kept for an unknown incident failed, will retry", "node", node, "err", err)
	}
	return marked
}

// recordTaintOwner adds an incident to the owners of the taint it placed.
func (c *Controller) recordTaintOwner(ctx context.Context, node, incidentID string) {
	ownership := c.taintOwnership()
	if ownership == nil {
		return
	}
	if err := ownership.UpdateDegradedTaintOwners(ctx, node, []string{incidentID}, nil); err != nil {
		c.log.Warn("recording the incident behind a degraded taint failed, will retry",
			"node", node, "incident", incidentID, "err", err)
	}
}

// syncTaintOwners makes the recorded owners of a tainted node under an open
// incident match the store: every open incident on the node is added, which
// also backfills a taint placed before owners were recorded, and every owner
// the store knows has halted is dropped. An owner the store has no record of
// stays, so the taint is kept for a human once the open incidents close.
//
// Dropping matters for retention: a halted owner left in the record would be
// pruned while another incident keeps the taint, and then read as unknown.
func (c *Controller) syncTaintOwners(ctx context.Context, node string) {
	ownership := c.taintOwnership()
	if ownership == nil {
		return
	}
	owners, _, _, err := ownership.DegradedTaintOwners(ctx, node)
	if err != nil {
		c.log.Warn("reading the incidents behind a degraded taint failed, will retry", "node", node, "err", err)
		return
	}
	open, err := c.store.ListIncidents(ctx, store.IncidentFilter{Node: node, States: activeIncidentStates()})
	if err != nil {
		c.log.Warn("reading open incidents failed, will retry", "node", node, "err", err)
		return
	}
	var add, drop []string
	for _, inc := range open {
		if !inc.DryRun {
			add = append(add, inc.ID)
		}
	}
	for _, id := range owners {
		if slices.Contains(add, id) {
			continue
		}
		inc, err := c.store.GetIncident(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			c.log.Warn("reading an incident behind a degraded taint failed, will retry", "node", node, "incident", id, "err", err)
			return
		}
		if !isActiveIncidentState(inc.State) {
			drop = append(drop, id)
		}
	}
	// The read above comes from the node cache; the write re-reads live, so
	// it is made only when the record would change.
	if !slices.ContainsFunc(add, func(id string) bool { return !slices.Contains(owners, id) }) && len(drop) == 0 {
		return
	}
	if err := ownership.UpdateDegradedTaintOwners(ctx, node, add, drop); err != nil {
		c.log.Warn("recording the incidents behind a degraded taint failed, will retry", "node", node, "err", err)
	}
}

// clearDegradedTaintRecords removes the records from nodes whose taint a
// human removed.
func (c *Controller) clearDegradedTaintRecords(ctx context.Context) {
	ownership := c.taintOwnership()
	if ownership == nil {
		return
	}
	cleared, err := ownership.ClearDegradedTaintRecords(ctx)
	for _, node := range cleared {
		c.log.Info("cleared the record of a degraded taint removed by hand", "node", node)
	}
	if err != nil {
		c.log.Warn("clearing the records of degraded taints removed by hand failed, will retry", "err", err)
	}
}

func unknownTaintMessage(node string) string {
	return "Node " + node + " carries the KubeNeuron taint kubeneuron.io/degraded, but this controller's store has no " +
		"record of an incident for it (for example after a reinstall or after restoring an older backup). " +
		"KubeNeuron keeps the taint and will not remove it. Check the node; when it can return to service, " +
		"run kubectl taint nodes " + node + " kubeneuron.io/degraded-"
}
