// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// A planned remove-first replacement deletes the node's instance and then
// sets its pool back to the size it had, on a pool whose size does not come
// back on its own. The plan is the incident's: it is recorded before the
// cloud is asked for anything, before the delete is sent, after every step,
// and when it ends. The step drives it until the instance is gone; from
// there, and after a restart, the reconcile pass finishes it from the record
// alone. One plan is open per pool at a time.

// removeFirstDeadline is how long an open plan may go on past the delete,
// with errors or without progress, before it is left for an operator with
// the command that sets the size back.
var removeFirstDeadline = time.Hour

// errPoolPlanOpen is another incident's plan still open on the same pool.
var errPoolPlanOpen = errors.New("another remove-first replacement is open on the node pool")

// replaceRemoveFirst replaces the pinned node through a plan when the
// provider needs one. handled is false when it does not: the caller replaces
// by deleting only.
func (c *Controller) replaceRemoveFirst(ctx context.Context, inc *types.Incident, plat platform.PlannedRemoveFirstRecycler, pin cloud.NodeIdentity) (res *types.ActionResult, handled bool, err error) {
	op := successorOperation(inc)
	if rec := inc.RemoveFirst; rec.Open() && rec.Operation != op {
		return nil, true, fmt.Errorf("platform.replace_node: remove-first operation %s of an earlier attempt is still setting node pool %s back", rec.Operation, rec.Pool)
	}
	if rec := inc.RemoveFirst; rec != nil && !rec.Open() && rec.Operation == op && rec.Phase != cloud.RemoveFirstPlanned {
		// This attempt's plan already ended past the delete: it is not
		// planned, or deleted, again.
		if rec.Outcome == types.RemoveFirstRestored {
			return okResult(inc, fmt.Sprintf("the instance behind %s was deleted and node pool %s set back to its size (operation %s)", pin.Node, rec.Pool, rec.Operation)), true, nil
		}
		return nil, true, fmt.Errorf("platform.replace_node: remove-first operation %s ended %s: %s", rec.Operation, rec.Outcome, rec.Reason)
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultRecycleWaitTimeout)
		defer cancel()
	}
	for !inc.RemoveFirst.Open() {
		plan, err := plat.PlanRemoveFirst(ctx, pin, op)
		if errors.Is(err, cloud.ErrRemoveFirstUnplanned) {
			return nil, false, nil
		}
		if err != nil {
			return nil, true, fmt.Errorf("platform.replace_node: %w", err)
		}
		// A plan of another incident on the pool is waited for; the plan is
		// made again once it ends, since the pool's size may have changed.
		if err := c.claimRemoveFirst(ctx, inc, plan); err != nil && (!errors.Is(err, errPoolPlanOpen) || !c.pause(ctx)) {
			return nil, true, fmt.Errorf("platform.replace_node: %w", err)
		}
	}
	for {
		rec := inc.RemoveFirst
		if err := c.continueRemoveFirst(ctx, inc, plat, pin); err != nil {
			// Once the delete is committed the node is not given back: a
			// transient error is retried here, then by the reconcile pass.
			if !inc.RemoveFirst.Open() || inc.RemoveFirst.Phase == cloud.RemoveFirstPlanned || !c.pause(ctx) {
				return nil, true, fmt.Errorf("platform.replace_node: %w", err)
			}
			continue
		}
		switch {
		case !inc.RemoveFirst.Open():
			return okResult(inc, fmt.Sprintf("deleted the instance behind %s and set node pool %s back to its size (operation %s)", pin.Node, rec.Pool, rec.Operation)), true, nil
		case inc.RemoveFirst.Phase == cloud.RemoveFirstResizing:
			// The node is gone; the reconcile pass sees the size set back.
			return okResult(inc, fmt.Sprintf("deleted the instance behind %s; node pool %s is being set back to its size (operation %s)", pin.Node, rec.Pool, rec.Operation)), true, nil
		}
		if !c.pause(ctx) {
			return nil, true, fmt.Errorf("platform.replace_node: remove-first operation %s still in phase %s: %w", rec.Operation, inc.RemoveFirst.Phase, ctx.Err())
		}
	}
}

// pause waits one poll interval; false when ctx ended first.
func (c *Controller) pause(ctx context.Context) bool {
	timer := time.NewTimer(c.successorPollInterval())
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// claimRemoveFirst records a new plan on the incident, unless another
// incident has a plan open on the same pool: two plans would each restore a
// size the other changed. The check is made in the transaction that writes
// the plan; only the leader writes plans, and the lock keeps its own
// replace steps from checking at the same time.
func (c *Controller) claimRemoveFirst(ctx context.Context, inc *types.Incident, plan cloud.RemoveFirstPlan) error {
	c.removeFirstPools.Lock()
	defer c.removeFirstPools.Unlock()
	rec := &types.RemoveFirstRecord{RemoveFirstPlan: plan}
	return c.saveRemoveFirst(ctx, inc, rec, "remove-first-planned",
		fmt.Sprintf("planned operation %s: delete the instance, then set node pool %s back to its size", plan.Operation, plan.Pool))
}

// poolPlanOpen is another incident's open plan on the pool of rec, read in tx.
func poolPlanOpen(ctx context.Context, tx store.Tx, inc *types.Incident, rec *types.RemoveFirstRecord) error {
	open, err := tx.ListIncidents(ctx, store.IncidentFilter{OpenRemoveFirst: true})
	if err != nil {
		return err
	}
	for _, other := range open {
		if other.ID != inc.ID && other.RemoveFirst.Pool == rec.Pool && providerOf(other) == providerOf(inc) {
			return fmt.Errorf("%w: incident %s is still setting node pool %s back to its size (operation %s); waiting", errPoolPlanOpen, other.ID, rec.Pool, other.RemoveFirst.Operation)
		}
	}
	return nil
}

// continueRemoveFirst moves the incident's open plan on by one step and
// records what came of it. A pool that refuses the plan before the delete
// ends it with nothing changed; one that cannot be set back safely, or a plan
// past its deadline, ends it for an operator. Any other error leaves the plan
// open, to be continued.
func (c *Controller) continueRemoveFirst(ctx context.Context, inc *types.Incident, plat platform.PlannedRemoveFirstRecycler, pin cloud.NodeIdentity) error {
	rec := inc.RemoveFirst.Clone()
	next, err := plat.ContinueRemoveFirst(ctx, pin, rec.RemoveFirstPlan)
	if next.Phase != rec.Phase || !maps.Equal(next.State, rec.State) {
		rec.RemoveFirstPlan = next
		if serr := c.saveRemoveFirst(ctx, inc, rec, "remove-first-"+string(next.Phase),
			fmt.Sprintf("operation %s: %s", rec.Operation, next.Phase)); serr != nil {
			return errors.Join(err, serr)
		}
	}
	switch {
	case errors.Is(err, cloud.ErrRemoveFirstUnavailable) && rec.Phase == cloud.RemoveFirstPlanned:
		if eerr := c.endRemoveFirst(ctx, inc, types.RemoveFirstNotStarted, err.Error()); eerr != nil {
			return errors.Join(err, eerr)
		}
		return err
	case errors.Is(err, cloud.ErrNeedsOperator):
		if eerr := c.endRemoveFirst(ctx, inc, types.RemoveFirstNeedsOperator, err.Error()); eerr != nil {
			return errors.Join(err, eerr)
		}
		return err
	case err != nil:
		return errors.Join(err, c.removeFirstOverdue(ctx, inc, inc.RemoveFirst, err.Error()))
	case next.Phase == cloud.RemoveFirstDone:
		return c.endRemoveFirst(ctx, inc, types.RemoveFirstRestored, "")
	}
	return c.removeFirstOverdue(ctx, inc, inc.RemoveFirst, "no progress")
}

// removeFirstOverdue ends a plan past the delete that is still open after
// removeFirstDeadline, for an operator. A plan at planned changed nothing
// and is left to its step.
func (c *Controller) removeFirstOverdue(ctx context.Context, inc *types.Incident, rec *types.RemoveFirstRecord, last string) error {
	if !rec.Open() || rec.Phase == cloud.RemoveFirstPlanned || time.Since(rec.PlannedAt) <= removeFirstDeadline {
		return nil
	}
	return c.endRemoveFirst(ctx, inc, types.RemoveFirstNeedsOperator,
		fmt.Sprintf("not finished within %s (last: %s, phase %s); to set the size back: %s", removeFirstDeadline, last, rec.Phase, rec.Restore))
}

// saveRemoveFirst persists rec on the incident with one audit row, and keeps
// inc in step with what was written.
func (c *Controller) saveRemoveFirst(ctx context.Context, inc *types.Incident, rec *types.RemoveFirstRecord, action, result string) error {
	now := time.Now()
	var saved *types.Incident
	err := c.store.WithTx(ctx, func(tx store.Tx) error {
		fresh, err := tx.GetIncident(ctx, inc.ID)
		if err != nil {
			return err
		}
		if fresh.RemoveFirst.Open() && fresh.RemoveFirst.Operation != rec.Operation {
			return store.ErrConflict
		}
		if rec.Open() && !fresh.RemoveFirst.Open() {
			if err := poolPlanOpen(ctx, tx, fresh, rec); err != nil {
				return err
			}
		}
		fresh.RemoveFirst = rec.Clone()
		touchUpdated(fresh, now)
		if err := tx.UpdateIncident(ctx, fresh); err != nil {
			return err
		}
		saved = fresh
		return tx.AppendAudit(ctx, &types.AuditEntry{
			IncidentID: inc.ID, Time: now, FromState: fresh.State, ToState: fresh.State,
			Actor: "system", Action: action, Result: result, DryRun: fresh.DryRun,
			Params: map[string]string{"operation": rec.Operation, "pool": rec.Pool, "phase": string(rec.Phase), "outcome": rec.Outcome},
		})
	})
	if err != nil {
		return err
	}
	inc.RemoveFirst = saved.RemoveFirst.Clone()
	inc.Version = saved.Version
	inc.UpdatedAt = saved.UpdatedAt
	return nil
}

// endRemoveFirst closes the plan with an outcome.
func (c *Controller) endRemoveFirst(ctx context.Context, inc *types.Incident, outcome, reason string) error {
	rec := inc.RemoveFirst.Clone()
	if !rec.Open() {
		return nil
	}
	now := time.Now().UTC()
	rec.Outcome, rec.Reason, rec.EndedAt = outcome, reason, &now
	result := fmt.Sprintf("operation %s ended %s", rec.Operation, outcome)
	if reason != "" {
		result += ": " + reason
	}
	if outcome == types.RemoveFirstNeedsOperator {
		c.log.Error("a remove-first replacement needs an operator", "incident", inc.ID, "pool", rec.Pool, "reason", reason)
	}
	return c.saveRemoveFirst(ctx, inc, rec, "remove-first-"+outcome, result)
}

// sweepRemoveFirst continues the open plans no step drives: after a restart
// or a change of leader, or once the step has returned with the size still
// being set back. A plan still at planned reached nothing in the cloud; with
// its step gone, it is ended.
func (c *Controller) sweepRemoveFirst(ctx context.Context) {
	plat, ok := c.platform.(platform.PlannedRemoveFirstRecycler)
	if !ok {
		return
	}
	open, err := c.store.ListIncidents(ctx, store.IncidentFilter{OpenRemoveFirst: true})
	if err != nil {
		c.log.Error("listing open remove-first plans failed", "err", err)
		return
	}
	for _, inc := range open {
		if c.isInFlight(inc.ID) {
			continue
		}
		if inc.RemoveFirst.Phase == cloud.RemoveFirstPlanned {
			if err := c.endRemoveFirst(ctx, inc, types.RemoveFirstNotStarted, "the step that planned it stopped before the delete; nothing was changed"); err != nil {
				c.log.Error("recording an unstarted remove-first plan failed", "incident", inc.ID, "err", err)
			}
			continue
		}
		if inc.CloudIdentity == nil {
			if err := c.endRemoveFirst(ctx, inc, types.RemoveFirstNeedsOperator,
				"the incident has no pinned cloud identity to continue the plan with; to set the size back: "+inc.RemoveFirst.Restore); err != nil {
				c.log.Error("recording a remove-first plan for an operator failed", "incident", inc.ID, "err", err)
			}
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, successorSweepTimeout)
		if err := c.continueRemoveFirst(sctx, inc, plat, *inc.CloudIdentity); err != nil {
			c.log.Warn("continuing a remove-first replacement failed; retrying", "incident", inc.ID, "operation", inc.RemoveFirst.Operation, "err", err)
		}
		cancel()
	}
}
