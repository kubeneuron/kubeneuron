// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// A fixed-size AKS pool is grown by one before its machine is deleted. The
// grow is recorded first, in the same record the delete then moves on in:
// the size before, the absolute target and the pool tag the grow writes with
// the new size. After a restart the record is compared with the pool; the
// target is never worked out again from a size read later.

// State keys of a recorded grow.
const (
	stateCountBefore = "count_before"
	stateCountTarget = "count_target"
	stateMarker      = "marker"
	// stateGrowFailed records that the step saw the grow fail for lack of
	// capacity, so a pool left changed is reported as that failure.
	stateGrowFailed = "grow_failed"
)

const growIntentAction = "remove-first-grow-intent"

// growSettleDelay is how long a recorded grow that the pool does not show
// is waited for before it is taken as never sent: a request already on its
// way when the controller stopped can still reach the pool after the first
// read.
var growSettleDelay = 2 * time.Minute

// claimGrowIntent records the grow before the provider sends it.
func (c *Controller) claimGrowIntent(ctx context.Context, inc *types.Incident, pin cloud.NodeIdentity, g removefirst.Grow) error {
	if err := pin.Validate(); err != nil {
		return err
	}
	if g.Pool == "" || g.Machine == "" || g.Marker == "" || g.Target != g.Before+1 {
		return errors.New("remove-first grow requires a known pool, machine, marker and a target one above the size")
	}
	rec := &types.RemoveFirstRecord{
		Version: 1, DeleteActionID: actionID(inc), Provider: providerOf(inc), DeleteTarget: &pin, MachineName: g.Machine,
		RemoveFirstPlan: cloud.RemoveFirstPlan{Operation: successorOperation(inc), Pool: g.Pool, Instance: pin.InstanceID,
			Phase: removefirst.PhaseGrowing, PlannedAt: time.Now().UTC(),
			State: map[string]string{
				stateCountBefore: strconv.Itoa(int(g.Before)),
				stateCountTarget: strconv.Itoa(int(g.Target)),
				stateMarker:      g.Marker,
			}},
	}
	c.removeFirstPools.Lock()
	defer c.removeFirstPools.Unlock()
	return c.saveRemoveFirst(ctx, inc, rec, growIntentAction,
		fmt.Sprintf("recorded pool %s at %d nodes and the target %d before growing it", g.Pool, g.Before, g.Target))
}

// growOf reads the recorded grow back.
func growOf(rec *types.RemoveFirstRecord) (removefirst.Grow, error) {
	before, err1 := strconv.ParseInt(rec.State[stateCountBefore], 10, 32)
	target, err2 := strconv.ParseInt(rec.State[stateCountTarget], 10, 32)
	if err := errors.Join(err1, err2); err != nil || rec.State[stateMarker] == "" || target != before+1 {
		return removefirst.Grow{}, fmt.Errorf("remove-first operation %s has no complete grow record", rec.Operation)
	}
	return removefirst.Grow{Pool: rec.Pool, Machine: rec.MachineName, Before: int32(before), Target: int32(target), Marker: rec.State[stateMarker]}, nil
}

func (c *Controller) observeGrow(ctx context.Context, g removefirst.Grow) (removefirst.GrowObservation, error) {
	observer, ok := c.platform.(interface {
		ObservePinnedGrow(context.Context, removefirst.Grow) (removefirst.GrowObservation, error)
	})
	if !ok {
		return removefirst.GrowObservation{}, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, successorSweepTimeout)
	defer cancel()
	return observer.ObservePinnedGrow(readCtx, g)
}

// settleRefusedGrow closes a grow the cloud refused outright, once a read
// shows the pool as it was: nothing changed, and the pool is free at once.
// Any other read leaves the record to the sweep.
func (c *Controller) settleRefusedGrow(ctx context.Context, inc *types.Incident) error {
	g, err := growOf(inc.RemoveFirst)
	if err != nil {
		return nil
	}
	obs, err := c.observeGrow(ctx, g)
	if err != nil {
		return c.parkRemoveFirst(ctx, inc, fmt.Sprintf("The cloud provider refused to resize pool %s to replace machine %s. Check its current size.", g.Pool, g.Machine))
	}
	if obs.State != removefirst.GrowNotApplied {
		return nil
	}
	return c.endRemoveFirst(ctx, inc, types.RemoveFirstNotStarted,
		fmt.Sprintf("The cloud provider refused to resize pool %s to replace machine %s. Nothing was changed.", g.Pool, g.Machine))
}

// noCapacity is a grow's capacity failure in our words: the class also
// holds the quota codes.
const noCapacity = "no capacity or quota for this machine type"

// settleCapacityGrow handles a grow the step saw fail for lack of capacity.
// A pool read as it was is freed at once, and the step fails as any
// capacity shortage does. Otherwise the failure is recorded for the sweep,
// which reports a changed pool as that failure.
func (c *Controller) settleCapacityGrow(ctx context.Context, inc *types.Incident) error {
	g, err := growOf(inc.RemoveFirst)
	if err != nil {
		return nil
	}
	if obs, err := c.observeGrow(ctx, g); err == nil && obs.State == removefirst.GrowNotApplied {
		return c.endRemoveFirst(ctx, inc, types.RemoveFirstNotStarted,
			fmt.Sprintf("The resize of pool %s to replace machine %s failed in the cloud (%s). Nothing was changed.", g.Pool, g.Machine, noCapacity))
	}
	next := inc.RemoveFirst.Clone()
	next.State[stateGrowFailed] = "capacity"
	return c.saveRemoveFirst(ctx, inc, next, "remove-first-grow-failed",
		fmt.Sprintf("the resize of pool %s to replace machine %s failed for lack of capacity", g.Pool, g.Machine))
}

// sweepGrow resumes a recorded grow without a step: the grow is never sent
// again from here. A pool that shows it moves on to the delete, which the
// next pass checks and repeats only as a new, fully authorized mutation.
func (c *Controller) sweepGrow(ctx context.Context, inc *types.Incident) error {
	rec := inc.RemoveFirst
	g, gerr := growOf(rec)
	var obs removefirst.GrowObservation
	var err error
	if gerr == nil {
		obs, err = c.observeGrow(ctx, g)
	}
	switch {
	case gerr != nil:
		return c.parkRemoveFirst(ctx, inc, fmt.Sprintf("The resize of pool %s to replace machine %s was started, but its record is incomplete.", rec.Pool, rec.MachineName))
	case err == nil && obs.State == removefirst.GrowApplied:
		next := rec.Clone()
		next.Phase = cloud.RemoveFirstDeleting
		return c.saveRemoveFirst(ctx, inc, next, "remove-first-"+string(cloud.RemoveFirstDeleting),
			fmt.Sprintf("pool %s shows the recorded grow to %d nodes; the delete of machine %s is next", g.Pool, g.Target, g.Machine))
	case err == nil && obs.State == removefirst.GrowNotApplied:
		if time.Since(rec.PlannedAt) < growSettleDelay {
			return nil
		}
		return c.endRemoveFirst(ctx, inc, types.RemoveFirstNotStarted,
			fmt.Sprintf("Pool %s was not resized and still has %d nodes. Nothing was changed.", g.Pool, g.Before))
	case err == nil && obs.State == removefirst.GrowChanged && rec.State[stateGrowFailed] != "":
		reason := fmt.Sprintf("The resize of pool %s to replace machine %s failed in the cloud (%s). Machine %s was not removed.", g.Pool, g.Machine, noCapacity, g.Machine)
		switch {
		case obs.Size == g.Before:
			reason += fmt.Sprintf(" Pool %s still has %d nodes.", g.Pool, obs.Size)
		case obs.Size > 0:
			reason += fmt.Sprintf(" Pool %s now has %d nodes; it had %d before the replacement.", g.Pool, obs.Size, g.Before)
		default:
			reason += " Check its current size."
		}
		return c.parkRemoveFirst(ctx, inc, reason)
	case err == nil && obs.State == removefirst.GrowChanged:
		return c.parkRemoveFirst(ctx, inc, fmt.Sprintf("The resize of pool %s to replace machine %s was started, but its outcome was not recorded, and the pool does not match it (%s), so it was not repeated.", g.Pool, g.Machine, obs.Detail)+
			unremovedSizeSentence(g, obs.Size))
	case time.Since(rec.PlannedAt) > removeFirstDeadline:
		return c.parkRemoveFirst(ctx, inc, fmt.Sprintf("The resize of pool %s to replace machine %s was started more than an hour ago, and KubeNeuron could not confirm that it completed.", g.Pool, g.Machine)+
			unremovedSizeSentence(g, obs.Size))
	}
	return err
}

// unremovedSizeSentence is the size of a pool whose grow was not confirmed,
// as read, with the machine the delete never reached; without a read, a
// request to check it.
func unremovedSizeSentence(g removefirst.Grow, read int32) string {
	if read > 0 {
		return fmt.Sprintf(" Pool %s now has %d nodes; it had %d before the replacement. Machine %s was not removed.", g.Pool, read, g.Before, g.Machine)
	}
	return " Check its current size."
}

// settleDeletedTarget ends a confirmed delete. A pool the provider grew for
// it is finished first: its replacement tag is dropped once the pool is back
// to its size, and a pool still one node larger is left for an operator.
func (c *Controller) settleDeletedTarget(ctx context.Context, inc *types.Incident) error {
	rec := inc.RemoveFirst
	untaggedReason := ""
	if settler, ok := c.platform.(interface {
		SettlePinnedPool(context.Context, string, string) (removefirst.SettleResult, error)
	}); ok {
		callCtx, cancel := context.WithTimeout(ctx, successorSweepTimeout)
		res, err := settler.SettlePinnedPool(callCtx, rec.Pool, rec.MachineName)
		cancel()
		if errors.Is(err, cloud.ErrNeedsOperator) {
			reason := fmt.Sprintf("Machine %s was removed from pool %s.", rec.MachineName, rec.Pool)
			if res.Extra {
				reason += fmt.Sprintf(" Pool %s was resized to %d before the removal and now has one node more than planned.", rec.Pool, res.Size)
			} else {
				reason += fmt.Sprintf(" Pool %s has %d nodes, not the %d it had before the replacement. Check its current size.", rec.Pool, res.Size, res.Before)
			}
			return c.parkRemoveFirst(ctx, inc, reason)
		}
		if res.Untagged {
			// Written to the pool even while paused: the audit says why.
			untaggedReason = fmt.Sprintf("The kubeneuron-replacing tag was removed from pool %s, which is back to %d nodes.", rec.Pool, res.Size)
		}
		if err != nil {
			if time.Since(rec.PlannedAt) > removeFirstDeadline {
				reason := fmt.Sprintf("Machine %s was removed from pool %s more than an hour ago, and KubeNeuron could not confirm that the pool is back to its size.", rec.MachineName, rec.Pool)
				if g, gerr := growOf(rec); gerr == nil {
					reason = fmt.Sprintf("Machine %s was removed from pool %s more than an hour ago, and KubeNeuron could not confirm that pool %s is back to %d nodes.", rec.MachineName, rec.Pool, rec.Pool, g.Before)
				}
				return c.parkRemoveFirst(ctx, inc, reason)
			}
			return err
		}
	}
	return c.endRemoveFirst(ctx, inc, types.RemoveFirstDeleteConfirmed, untaggedReason)
}

// parkRemoveFirst leaves the record and the incident for an operator, with
// what to check and the command that settles it.
func (c *Controller) parkRemoveFirst(ctx context.Context, inc *types.Incident, reason string) error {
	return c.parkRemoveFirstWithCheck(ctx, inc, reason+" Check the machine and the size of the pool, then run")
}

func (c *Controller) parkRemoveFirstWithCheck(ctx context.Context, inc *types.Incident, instruction string) error {
	reason := fmt.Sprintf(`%s kubeneuronctl resolve %s --cloud-operation-settled --reason "<what you found>"`, instruction, inc.ID)
	if inc.State != types.StateNeedsHuman && inc.State != types.StateResolved {
		if err := c.quarantineAfterStep(ctx, inc, reason, false); err != nil {
			return err
		}
	}
	return c.endRemoveFirst(ctx, inc, types.RemoveFirstNeedsOperator, reason)
}
