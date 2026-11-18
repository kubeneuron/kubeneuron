// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// State keys of a remove-first plan, besides stateGroup, stateSize and
// stateOperation: every zonal group of the pool at the plan. stateSize is the
// node count each zone had, the same in all of them.
const stateGroups = "groups"

var _ cloud.PlannedRemoveFirstProvider = (*Provider)(nil)

// zoneSizes are the target sizes of every zonal group of a pool, and whether
// they all have settled.
func (p *Provider) zoneSizes(ctx context.Context, pl nodePool) ([]int32, bool, error) {
	sizes := make([]int32, 0, len(pl.Groups))
	stable := true
	for _, g := range pl.Groups {
		gr, err := p.api.Group(ctx, g)
		if err != nil {
			return nil, false, fmt.Errorf("gcp: reading instance group %s: %w", g.name, err)
		}
		sizes = append(sizes, gr.TargetSize)
		stable = stable && gr.Stable
	}
	return sizes, stable, nil
}

// unequalZones refuses a fixed-size pool whose zones differ in size: GKE sets
// one node count for every zone, so restoring one zone would change another.
func (p *Provider) unequalZones(ctx context.Context, pl nodePool) error {
	sizes, _, err := p.zoneSizes(ctx, pl)
	if err != nil {
		return err
	}
	for _, n := range sizes {
		if n != sizes[0] {
			return fmt.Errorf("%w: node pool %s has zones of different sizes (%s); GKE sets one node count for every zone, so its size could not be set back",
				cloud.ErrRemoveFirstUnavailable, pl.Name, joinSizes(sizes))
		}
	}
	return nil
}

func joinSizes(sizes []int32) string {
	out := make([]string, 0, len(sizes))
	for _, n := range sizes {
		out = append(out, strconv.Itoa(int(n)))
	}
	return strings.Join(out, ", ")
}

// restoreCommand is how a person sets the pool back to its size before.
func (p *Provider) restoreCommand(pool string, size int32) string {
	return fmt.Sprintf("gcloud container clusters resize %s --node-pool %s --num-nodes %d --location %s --project %s",
		p.cluster, pool, size, p.location, p.project)
}

// PlanRemoveFirst records the size of every zone of a fixed-size pool before
// one of its instances is deleted; see cloud.PlannedRemoveFirstProvider. An
// autoscaled pool needs no plan: Replace only deletes, and the autoscaler
// owns the size.
func (p *Provider) PlanRemoveFirst(ctx context.Context, instanceID, operation string) (cloud.RemoveFirstPlan, error) {
	plan := cloud.RemoveFirstPlan{Operation: operation, Instance: instanceID, PlannedAt: p.now()}
	t, c, err := p.resolve(ctx, instanceID)
	if errors.Is(err, errGone) {
		return plan, fmt.Errorf("%w: instance %s is already gone", cloud.ErrRemoveFirstUnplanned, instanceID)
	}
	if err != nil {
		return plan, err
	}
	switch {
	case c.Autopilot:
		return plan, fmt.Errorf("%w: GKE Autopilot manages its nodes; KubeNeuron only observes them", cloud.ErrRemoveFirstUnavailable)
	case t.pool.Autoscaling:
		return plan, fmt.Errorf("%w: node pool %s is under the cluster autoscaler", cloud.ErrRemoveFirstUnplanned, t.pool.Name)
	}
	ops, err := p.api.Operations(ctx)
	if err != nil {
		return plan, fmt.Errorf("gcp: listing GKE operations: %w", err)
	}
	if op, ok := p.running(ops, t.pool.Name); ok {
		return plan, fmt.Errorf("gcp: node pool %s has GKE operation %s running (%s); waiting", t.pool.Name, op.Name, op.Type)
	}
	if err := p.unequalZones(ctx, t.pool); err != nil {
		return plan, err
	}
	sizes, stable, err := p.zoneSizes(ctx, t.pool)
	if err != nil {
		return plan, err
	}
	if !stable {
		return plan, fmt.Errorf("gcp: node pool %s is still changing; waiting", t.pool.Name)
	}
	groups := make([]string, 0, len(t.pool.Groups))
	for _, g := range t.pool.Groups {
		groups = append(groups, g.url())
	}
	plan.Pool, plan.Phase = t.pool.Name, cloud.RemoveFirstPlanned
	plan.Restore = p.restoreCommand(t.pool.Name, sizes[0])
	plan.State = map[string]string{
		stateGroup:  t.group.url(),
		stateGroups: strings.Join(groups, ","),
		stateSize:   strconv.Itoa(int(sizes[0])),
	}
	return plan, nil
}

// ContinueRemoveFirst moves a plan on by one step; see
// cloud.PlannedRemoveFirstProvider. From planned it only checks the pool and
// returns deleting, which the caller records before the delete is sent: a
// plan recorded as planned never reached Google Cloud. The instance is
// deleted by name with a
// request ID of the plan, so a resent delete is the same delete. The size is
// set back through GKE as an absolute node count, and only while the
// instance's zone is below it and every other zone is at it: a resent resize
// is the same resize, and no zone is ever lowered.
func (p *Provider) ContinueRemoveFirst(ctx context.Context, plan cloud.RemoveFirstPlan) (cloud.RemoveFirstPlan, error) {
	if plan.Phase == cloud.RemoveFirstDone {
		return plan, nil
	}
	started := plan.Phase != cloud.RemoveFirstPlanned
	// refuse is what a pool that no longer fits the plan returns: before the
	// delete nothing was changed; after it the size may be short.
	refuse := func(format string, args ...any) error {
		if !started {
			return fmt.Errorf("%w: %s", cloud.ErrRemoveFirstUnavailable, fmt.Sprintf(format, args...))
		}
		return fmt.Errorf("gcp: %s; to set the size back: %s: %w", fmt.Sprintf(format, args...), plan.Restore, cloud.ErrNeedsOperator)
	}
	g, err := parseGroup(plan.State[stateGroup])
	if err != nil {
		return plan, fmt.Errorf("gcp: remove-first operation %s has no recorded plan: %w", plan.Operation, err)
	}
	before, err := strconv.ParseInt(plan.State[stateSize], 10, 32)
	if err != nil {
		return plan, fmt.Errorf("gcp: remove-first operation %s has no recorded size", plan.Operation)
	}
	old, err := parseInstanceID(plan.Instance)
	if err != nil {
		return plan, err
	}
	c, err := p.api.Cluster(ctx)
	if err != nil {
		return plan, fmt.Errorf("gcp: reading GKE cluster %s: %w", p.cluster, err)
	}
	pl, ok := c.pool(plan.Pool)
	if !ok {
		return plan, refuse("node pool %s is gone", plan.Pool)
	}
	var groups []string
	for _, gr := range pl.Groups {
		groups = append(groups, gr.url())
	}
	if strings.Join(groups, ",") != plan.State[stateGroups] {
		return plan, refuse("the zones of node pool %s changed since the plan", pl.Name)
	}
	if pl.Autoscaling || c.Autopilot {
		return plan, refuse("node pool %s is no longer fixed-size", pl.Name)
	}
	ops, err := p.api.Operations(ctx)
	if err != nil {
		return plan, fmt.Errorf("gcp: listing GKE operations: %w", err)
	}
	if _, ok := p.running(ops, pl.Name); ok {
		return plan, nil // another operation, or the plan's own resize, runs
	}
	members, err := p.api.Members(ctx, g)
	if err != nil {
		return plan, fmt.Errorf("gcp: listing instance group %s: %w", g.name, err)
	}
	sizes, _, err := p.zoneSizes(ctx, pl)
	if err != nil {
		return plan, err
	}
	zone := slices.Index(pl.Groups, g)
	m, present := memberNamed(members, old.name)
	switch {
	case !started:
		// Nothing was sent: only see that the pool is as planned, so the
		// caller can record that the delete is about to be.
		if !present || m.Action != "NONE" && m.Action != "" {
			return plan, refuse("instance %s is no longer a running member of node pool %s", old.name, pl.Name)
		}
		for _, n := range sizes {
			if n != int32(before) {
				return plan, refuse("node pool %s changed since the plan (zones at %s, planned %d each)", pl.Name, joinSizes(sizes), before)
			}
		}
		plan.Phase = cloud.RemoveFirstDeleting
		return plan, nil
	case present && m.Action == "DELETING":
		return plan, nil // the delete runs
	case present && plan.Phase == cloud.RemoveFirstResizing:
		return plan, refuse("instance %s is back in node pool %s", old.name, pl.Name)
	case present:
		for _, n := range sizes {
			if n != int32(before) {
				return plan, refuse("node pool %s changed before the delete (zones at %s, planned %d each)", pl.Name, joinSizes(sizes), before)
			}
		}
		// A delete resent after a lost answer has the same request ID, so
		// Compute Engine runs it once.
		if err := p.api.DeleteInstances(ctx, g, []string{old.name}, requestID("remove-first", plan.Operation, old.name)); err != nil {
			return plan, fmt.Errorf("gcp: deleting instance %s from node pool %s: %w", old.name, pl.Name, err)
		}
		return plan, nil
	}
	// The instance is gone: set the size back, if its zone is short.
	if sizes[zone] >= int32(before) {
		for _, n := range sizes {
			if n != sizes[zone] {
				return plan, refuse("node pool %s has zones of different sizes (%s) after the delete", pl.Name, joinSizes(sizes))
			}
		}
		plan.Phase = cloud.RemoveFirstDone
		return plan, nil
	}
	for i, n := range sizes {
		if i != zone && n != int32(before) {
			return plan, refuse("node pool %s was resized since the plan (zones at %s, planned %d each)", pl.Name, joinSizes(sizes), before)
		}
	}
	plan.Phase = cloud.RemoveFirstResizing
	name, err := p.api.SetPoolSize(ctx, pl.Name, int32(before))
	if err != nil {
		return plan, fmt.Errorf("gcp: setting node pool %s back to %d nodes per zone: %w", pl.Name, before, err)
	}
	plan.State = withState(plan.State, stateOperation, name)
	return plan, nil
}
