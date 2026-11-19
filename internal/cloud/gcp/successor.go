// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// State keys of a GCP successor operation. The plan records the pool's group,
// size and members, and the resize operations it already had; the request
// adds the name of the GKE operation that grew it. Nothing is written into
// Google Cloud to mark the operation.
const (
	stateGroup     = "group"
	stateSize      = "size"
	stateMembers   = "members"
	stateSeen      = "seen"
	stateOperation = "operation"
)

var _ cloud.SuccessorProvider = (*Provider)(nil)

// CheckAddFirst allows a fixed-size node pool in one zone of GKE Standard: a
// size set through GKE applies to every zone, and an autoscaled pool's size
// is the autoscaler's.
func (p *Provider) CheckAddFirst(ctx context.Context, instanceID string) error {
	t, c, err := p.resolve(ctx, instanceID)
	if err != nil {
		return err
	}
	return addFirstRefusal(t.pool, c)
}

func addFirstRefusal(pl nodePool, c cluster) error {
	switch {
	case c.Autopilot:
		return &cloud.PoolRefusal{Code: cloud.PoolAutopilot, Reason: "GKE Autopilot manages its nodes; KubeNeuron only observes them"}
	case pl.Autoscaling:
		return &cloud.PoolRefusal{Code: cloud.PoolAutoscaled, Reason: fmt.Sprintf("node pool %s is under the cluster autoscaler, which owns its size", pl.Name)}
	case len(pl.Groups) != 1:
		return &cloud.PoolRefusal{Code: cloud.PoolMultiZone, Reason: fmt.Sprintf("node pool %s spans %d zones, and GKE grows every zone at once", pl.Name, len(pl.Groups))}
	}
	return nil
}

// PrepareSuccessor moves an add-first operation on by one phase; see
// cloud.SuccessorProvider.
func (p *Provider) PrepareSuccessor(ctx context.Context, s cloud.Successor) (cloud.Successor, error) {
	if s.UnrequestedPlan() {
		return s, fmt.Errorf("%w: planned successor has no RequestedAt", cloud.ErrNeedsOperator)
	}
	switch s.Phase {
	case "":
		return p.planSuccessor(ctx, s)
	case cloud.SuccessorPlanned:
		return p.requestSuccessor(ctx, s)
	case cloud.SuccessorRequested:
		return p.settleSuccessor(ctx, s)
	case cloud.SuccessorCreated:
		return s, nil
	}
	return s, fmt.Errorf("gcp: unknown successor phase %q", s.Phase)
}

func (p *Provider) planSuccessor(ctx context.Context, s cloud.Successor) (cloud.Successor, error) {
	t, c, err := p.resolve(ctx, s.Instance)
	if err != nil {
		return s, err
	}
	if err := addFirstRefusal(t.pool, c); err != nil {
		return s, err
	}
	ops, err := p.api.Operations(ctx)
	if err != nil {
		return s, fmt.Errorf("gcp: listing GKE operations: %w", err)
	}
	if op, ok := p.running(ops, t.pool.Name); ok {
		return s, fmt.Errorf("gcp: node pool %s has GKE operation %s running (%s); waiting", t.pool.Name, op.Name, op.Type)
	}
	g, err := p.api.Group(ctx, t.group)
	if err != nil {
		return s, fmt.Errorf("gcp: reading instance group %s: %w", t.group.name, err)
	}
	if !g.Stable {
		return s, fmt.Errorf("gcp: instance group %s of node pool %s is still changing; waiting", t.group.name, t.pool.Name)
	}
	s.Pool, s.Phase = t.pool.Name, cloud.SuccessorPlanned
	s.State = map[string]string{
		stateGroup:   t.group.url(),
		stateSize:    strconv.Itoa(int(g.TargetSize)),
		stateMembers: strings.Join(memberNames(t.members), ","),
		stateSeen:    strings.Join(p.resizes(ops, t.pool.Name), ","),
	}
	return s, nil
}

// requestSuccessor grows the pool by one through GKE, if it is still as
// planned. A retry after a lost answer finds the resize that request started
// — one the plan had not seen — and does not send another.
func (p *Provider) requestSuccessor(ctx context.Context, s cloud.Successor) (cloud.Successor, error) {
	pl, g, members, err := p.successorPool(ctx, s)
	if err != nil {
		return s, err
	}
	ops, err := p.api.Operations(ctx)
	if err != nil {
		return s, fmt.Errorf("gcp: listing GKE operations: %w", err)
	}
	gr, err := p.api.Group(ctx, g)
	if err != nil {
		return s, fmt.Errorf("gcp: reading instance group %s: %w", g.name, err)
	}
	if mine, err := p.ownResize(s, ops, gr.TargetSize); err != nil || mine != "" {
		if err != nil {
			return s, err
		}
		s.Phase, s.State = cloud.SuccessorRequested, withState(s.State, stateOperation, mine)
		return s, nil
	}
	if op, ok := p.running(ops, pl.Name); ok {
		return s, fmt.Errorf("gcp: node pool %s has GKE operation %s running (%s); waiting", pl.Name, op.Name, op.Type)
	}
	c, err := p.api.Cluster(ctx)
	if err != nil {
		return s, fmt.Errorf("gcp: reading GKE cluster %s: %w", p.cluster, err)
	}
	if err := addFirstRefusal(pl, c); err != nil {
		return s, err
	}
	before, planned, err := plannedPool(s)
	if err != nil {
		return s, err
	}
	if gr.TargetSize != before || !slices.Equal(memberNames(members), planned) {
		return s, fmt.Errorf("%w: node pool %s changed since the successor was planned (%d members at size %d, planned %d at %d)",
			cloud.ErrAddFirstUnavailable, pl.Name, len(members), gr.TargetSize, len(planned), before)
	}
	name, err := p.api.SetPoolSize(ctx, pl.Name, before+1)
	if err != nil {
		if capacityError(err) {
			return s, fmt.Errorf("gcp: growing node pool %s for a successor: %w: %w", pl.Name, cloud.ErrReplacementCapacity, err)
		}
		return s, fmt.Errorf("gcp: growing node pool %s for a successor: %w", pl.Name, err)
	}
	s.Phase, s.State = cloud.SuccessorRequested, withState(s.State, stateOperation, name)
	return s, nil
}

// settleSuccessor names the successor once the pool's group lists exactly one
// member that was not there at the plan, and it runs.
func (p *Provider) settleSuccessor(ctx context.Context, s cloud.Successor) (cloud.Successor, error) {
	pl, g, members, err := p.successorPool(ctx, s)
	if err != nil {
		return s, err
	}
	op, err := p.api.Operation(ctx, s.State[stateOperation])
	if err != nil {
		return s, fmt.Errorf("gcp: reading GKE operation %s: %w", s.State[stateOperation], err)
	}
	added, err := addedMembers(s, members)
	if err != nil {
		return s, err
	}
	if op.Status == opDone && op.Error != "" && len(added) == 0 {
		err := fmt.Errorf("gcp: growing node pool %s failed: %s", pl.Name, op.Error)
		if op.ErrorCode == codes.ResourceExhausted {
			err = fmt.Errorf("%w: %w", cloud.ErrReplacementCapacity, err)
		}
		return s, err
	}
	switch len(added) {
	case 0:
		return s, nil // still growing
	case 1:
	default:
		return s, fmt.Errorf("gcp: node pool %s has %d members that were not there at the plan of operation %s, not one: %w", pl.Name, len(added), s.Operation, cloud.ErrNeedsOperator)
	}
	m := added[0]
	if slices.ContainsFunc(m.Errors, capacityCode) {
		return s, fmt.Errorf("gcp: node pool %s could not create instance %s (%s): %w", pl.Name, m.Name, strings.Join(m.Errors, ", "), cloud.ErrReplacementCapacity)
	}
	if m.Action != "NONE" || m.Status != "RUNNING" {
		return s, nil // still creating
	}
	s.Phase, s.ProviderID = cloud.SuccessorCreated, g.instance(m.Name).providerID()
	return s, nil
}

// ReleaseSuccessor undoes an abandoned operation; see cloud.SuccessorProvider.
// The pool is never shrunk through GKE, which would choose the node: only
// the operation's own new member is deleted, by name.
func (p *Provider) ReleaseSuccessor(ctx context.Context, s cloud.Successor) error {
	if s.UnrequestedPlan() {
		return nil
	}
	pl, g, members, err := p.successorPool(ctx, s)
	if err != nil {
		return err
	}
	gr, err := p.api.Group(ctx, g)
	if err != nil {
		return fmt.Errorf("gcp: reading instance group %s: %w", g.name, err)
	}
	ops, err := p.api.Operations(ctx)
	if err != nil {
		return fmt.Errorf("gcp: listing GKE operations: %w", err)
	}
	mine, err := p.ownResize(s, ops, gr.TargetSize)
	if errors.Is(err, cloud.ErrAddFirstUnavailable) {
		return nil // the pool was resized, but not by this operation
	}
	if err != nil {
		return err
	}
	if mine == "" {
		return nil // nothing of this operation reached GKE
	}
	added, err := addedMembers(s, members)
	if err != nil {
		return err
	}
	switch len(added) {
	case 0:
	case 1:
		// The operation's one new member goes, whether it runs or is still
		// being created (a zone out of GPUs keeps retrying): deleting it by
		// name takes the pool back to its size.
		if s.Phase == cloud.SuccessorCreated && g.instance(added[0].Name).providerID() != s.ProviderID {
			return fmt.Errorf("gcp: node pool %s does not hold just the successor %s of operation %s: %w", pl.Name, s.ProviderID, s.Operation, cloud.ErrNeedsOperator)
		}
		if err := p.api.DeleteInstances(ctx, g, []string{added[0].Name}, requestID("release", s.Operation, added[0].Name)); err != nil {
			return fmt.Errorf("gcp: deleting successor %s from node pool %s: the result is unknown: %w: %w", added[0].Name, pl.Name, cloud.ErrNeedsOperator, err)
		}
		return nil
	default:
		return fmt.Errorf("gcp: node pool %s has %d members that were not there at the plan of operation %s: %w", pl.Name, len(added), s.Operation, cloud.ErrNeedsOperator)
	}
	// No new member: undone already, or the request may still bring one.
	if op, err := p.api.Operation(ctx, mine); err != nil || op.Status != opDone {
		return fmt.Errorf("gcp: GKE operation %s that grew node pool %s is not known to be done, and no new member is listed yet: %w", mine, pl.Name, cloud.ErrNeedsOperator)
	}
	if int(gr.TargetSize) != len(members) {
		return fmt.Errorf("gcp: instance group %s is at size %d with %d members; a member may still come: %w", g.name, gr.TargetSize, len(members), cloud.ErrNeedsOperator)
	}
	return nil // no member came, or an earlier release deleted it
}

// ReplaceWithSuccessor deletes the pinned instance once its successor of the
// same operation is in the pool; the delete leaves the pool at its size
// before the request. A delete whose result is unknown needs an operator:
// retrying it blindly, or releasing the successor, could leave the pool with
// neither.
func (p *Provider) ReplaceWithSuccessor(ctx context.Context, s cloud.Successor) error {
	pl, g, members, err := p.successorPool(ctx, s)
	if err != nil {
		return err
	}
	succ, err := parseInstance(s.ProviderID)
	if err != nil || succ.project != g.project || succ.zone != g.zone {
		return fmt.Errorf("gcp: successor %q of operation %s is not in instance group %s: %w", s.ProviderID, s.Operation, g.name, cloud.ErrNeedsOperator)
	}
	if _, ok := memberNamed(members, succ.name); !ok {
		return fmt.Errorf("gcp: successor %s of operation %s is not in node pool %s: %w", succ.name, s.Operation, pl.Name, cloud.ErrNeedsOperator)
	}
	old, err := parseInstanceID(s.Instance)
	if err != nil {
		return err
	}
	if _, ok := memberNamed(members, old.name); !ok {
		return nil // replaced on an earlier call
	}
	if err := p.api.DeleteInstances(ctx, g, []string{old.name}, requestID("replace", s.Operation, old.name)); err != nil {
		return fmt.Errorf("gcp: deleting instance %s from node pool %s: the result is unknown: %w: %w", old.name, pl.Name, cloud.ErrNeedsOperator, err)
	}
	return nil
}

// successorPool reads the planned group, confirmed to still be the pool's,
// and its members.
func (p *Provider) successorPool(ctx context.Context, s cloud.Successor) (nodePool, groupRef, []member, error) {
	g, err := parseGroup(s.State[stateGroup])
	if err != nil {
		return nodePool{}, g, nil, fmt.Errorf("gcp: successor operation %s has no recorded plan: %w", s.Operation, err)
	}
	c, err := p.api.Cluster(ctx)
	if err != nil {
		return nodePool{}, g, nil, fmt.Errorf("gcp: reading GKE cluster %s: %w", p.cluster, err)
	}
	pl, ok := c.pool(s.Pool)
	if !ok || !slices.Contains(pl.Groups, g) {
		return pl, g, nil, fmt.Errorf("gcp: instance group %s is no longer in node pool %s: %w", g.name, s.Pool, cloud.ErrNeedsOperator)
	}
	members, err := p.api.Members(ctx, g)
	if err != nil {
		return pl, g, nil, fmt.Errorf("gcp: listing instance group %s: %w", g.name, err)
	}
	return pl, g, members, nil
}

// resizes are the names of the pool's GKE resize operations, sorted.
func (p *Provider) resizes(ops []operation, pool string) []string {
	var names []string
	for _, op := range ops {
		if op.Type == opSetSize && p.onPool(op, pool) {
			names = append(names, op.Name)
		}
	}
	slices.Sort(names)
	return names
}

// ownResize finds the resize this operation's request started: one the plan
// had not seen. More than one cannot be told apart.
func (p *Provider) ownResize(s cloud.Successor, ops []operation, targetSize int32) (string, error) {
	if s.State[stateOperation] != "" {
		return s.State[stateOperation], nil
	}
	before, _, err := plannedPool(s)
	if err != nil {
		return "", err
	}
	seen := strings.Split(s.State[stateSeen], ",")
	var fresh []operation
	for _, op := range ops {
		if op.Type == opSetSize && p.onPool(op, s.Pool) && !slices.Contains(seen, op.Name) {
			fresh = append(fresh, op)
		}
	}
	if len(fresh) > 1 {
		return "", fmt.Errorf("gcp: node pool %s was resized %d times since the plan of operation %s: %w", s.Pool, len(fresh), s.Operation, cloud.ErrNeedsOperator)
	}
	if len(fresh) == 0 {
		return "", nil
	}
	op := fresh[0]
	// Ours only if it started after the request went out (the caller records
	// the time first; a minute for the clocks) and grew the pool by one. A
	// resize someone made while the step waited for approval is not.
	early := !s.RequestedAt.IsZero() && (op.Start.IsZero() || op.Start.Before(s.RequestedAt.Add(-clockSkew)))
	if early || targetSize != before+1 {
		return "", fmt.Errorf("%w: node pool %s was resized (GKE operation %s) since the successor was planned", cloud.ErrAddFirstUnavailable, s.Pool, op.Name)
	}
	return op.Name, nil
}

// clockSkew is how far the controller's clock may be from GKE's.
const clockSkew = time.Minute

func plannedPool(s cloud.Successor) (int32, []string, error) {
	n, err := strconv.ParseInt(s.State[stateSize], 10, 32)
	if err != nil || s.State[stateMembers] == "" {
		return 0, nil, fmt.Errorf("gcp: successor operation %s has no recorded plan", s.Operation)
	}
	return int32(n), strings.Split(s.State[stateMembers], ","), nil
}

func addedMembers(s cloud.Successor, members []member) ([]member, error) {
	_, planned, err := plannedPool(s)
	if err != nil {
		return nil, err
	}
	var added []member
	for _, m := range members {
		if !slices.Contains(planned, m.Name) {
			added = append(added, m)
		}
	}
	return added, nil
}

// memberNames lists member names, sorted.
func memberNames(members []member) []string {
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Name)
	}
	slices.Sort(names)
	return names
}

func withState(state map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(state)+1)
	for k, v := range state {
		out[k] = v
	}
	out[key] = value
	return out
}
