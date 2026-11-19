// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// successorTag marks a fixed-size pool grown by one for an add-first
// operation: "<operation token>:<count before>". The token is a hash of the
// operation, which names the incident and so the node: the tag is visible
// to anyone who can read the pool. It is written in the same request
// as the new size, so the pool shows whether the request reached AKS, and a
// retry after a lost answer finds it instead of growing the pool again.
const successorTag = "kubeneuron-successor"

// State keys of an Azure successor operation, recorded at the plan.
const (
	stateMachine = "machine"
	stateCount   = "count"
	stateMembers = "members"
)

var _ cloud.SuccessorProvider = (*Provider)(nil)

// CheckAddFirst refuses a pool under the cluster autoscaler: its size is the
// autoscaler's, so a successor cannot be added and later taken back.
func (p *Provider) CheckAddFirst(ctx context.Context, instanceID string) error {
	t, _, pl, err := p.successorPool(ctx, instanceID)
	if err != nil {
		return err
	}
	if t.machine == "" {
		return &cloud.PoolRefusal{Code: cloud.PoolInstanceMissing, Reason: fmt.Sprintf("instance %s is not a machine of agent pool %s", instanceID, t.pool)}
	}
	return addFirstRefusal(t.pool, pl)
}

func addFirstRefusal(name string, pl pool) error {
	if pl.Autoscaling {
		return &cloud.PoolRefusal{Code: cloud.PoolAutoscaled, Reason: fmt.Sprintf("agent pool %s is under the cluster autoscaler, which owns its size", name)}
	}
	if pl.ZoneCount > 1 {
		return &cloud.PoolRefusal{Code: cloud.PoolMultiZone, Reason: "AKS chooses the zone of a new node in this pool"}
	}
	return nil
}

func (p *Provider) successorPool(ctx context.Context, instanceID string) (target, []machine, pool, error) {
	t, machines, err := p.resolve(ctx, instanceID)
	if err != nil {
		return t, nil, pool{}, err
	}
	pl, err := p.api.Pool(ctx, t.pool)
	if err != nil {
		return t, nil, pool{}, fmt.Errorf("azure: reading agent pool %s: %w", t.pool, err)
	}
	return t, machines, pl, nil
}

// PrepareSuccessor moves an add-first operation on by one phase; see
// cloud.SuccessorProvider.
func (p *Provider) PrepareSuccessor(ctx context.Context, s cloud.Successor) (cloud.Successor, error) {
	if s.UnrequestedPlan() {
		return s, fmt.Errorf("%w: planned successor has no RequestedAt", cloud.ErrNeedsOperator)
	}
	t, machines, pl, err := p.successorPool(ctx, s.Instance)
	if err != nil {
		return s, err
	}
	op, before, err := parseSuccessorTag(pl.Tags)
	if err != nil {
		return s, fmt.Errorf("azure: agent pool %s: %w: %w", t.pool, cloud.ErrNeedsOperator, err)
	}
	switch s.Phase {
	case "":
		return p.planSuccessor(s, t, machines, pl)
	case cloud.SuccessorPlanned:
		if op == operationToken(s.Operation) {
			// The request reached AKS; its answer was lost.
			s.Phase = cloud.SuccessorRequested
			return s, nil
		}
		return p.requestSuccessor(ctx, s, t, machines, pl, op)
	case cloud.SuccessorRequested:
		if op != operationToken(s.Operation) {
			return s, fmt.Errorf("azure: agent pool %s no longer carries the %s tag of operation %s: %w", t.pool, successorTag, s.Operation, cloud.ErrNeedsOperator)
		}
		return settleSuccessor(s, t, machines, pl, before)
	case cloud.SuccessorCreated:
		return s, nil
	}
	return s, fmt.Errorf("azure: unknown successor phase %q", s.Phase)
}

func (p *Provider) planSuccessor(s cloud.Successor, t target, machines []machine, pl pool) (cloud.Successor, error) {
	if t.machine == "" {
		return s, fmt.Errorf("%w: instance %s is not a machine of agent pool %s", cloud.ErrAddFirstUnavailable, s.Instance, t.pool)
	}
	if err := addFirstRefusal(t.pool, pl); err != nil {
		return s, err
	}
	if !strings.EqualFold(pl.ProvisioningState, "Succeeded") {
		return s, fmt.Errorf("azure: agent pool %s is busy (state %q); retry", t.pool, pl.ProvisioningState)
	}
	names := machineNames(machines)
	s.Pool, s.Phase = t.pool, cloud.SuccessorPlanned
	s.State = map[string]string{
		stateMachine: t.machine,
		stateCount:   strconv.Itoa(int(pl.Count)),
		stateMembers: strings.Join(names, ","),
	}
	return s, nil
}

// requestSuccessor grows the pool by one, with the operation's tag, if it is
// still as planned. One replacement runs per pool at a time.
func (p *Provider) requestSuccessor(ctx context.Context, s cloud.Successor, t target, machines []machine, pl pool, other string) (cloud.Successor, error) {
	if other != "" {
		return s, fmt.Errorf("azure: agent pool %s is preparing a successor for another operation (tag %s); waiting, one replacement per pool at a time", t.pool, other)
	}
	if marked, _, _ := parseMarker(pl.Tags); marked != "" {
		return s, fmt.Errorf("azure: agent pool %s is replacing %s; waiting, one replacement per pool at a time", t.pool, marked)
	}
	if err := addFirstRefusal(t.pool, pl); err != nil {
		return s, err
	}
	if !strings.EqualFold(pl.ProvisioningState, "Succeeded") {
		return s, fmt.Errorf("azure: agent pool %s is busy (state %q); retry", t.pool, pl.ProvisioningState)
	}
	before, members, err := plannedPool(s)
	if err != nil {
		return s, err
	}
	if pl.Count != before || !sameMembers(machineNames(machines), members) {
		return s, fmt.Errorf("%w: agent pool %s changed since the successor was planned (%d machines at size %d, planned %d at %d)",
			cloud.ErrAddFirstUnavailable, t.pool, len(machines), pl.Count, len(members), before)
	}
	tags := maps.Clone(pl.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	tags[successorTag] = operationToken(s.Operation) + ":" + strconv.Itoa(int(before))
	if err := p.api.WritePool(ctx, t.pool, pl, before+1, tags); err != nil {
		if capacityError(err) {
			return s, fmt.Errorf("azure: growing agent pool %s for a successor of %s: %w: %w", t.pool, t.machine, cloud.ErrReplacementCapacity, err)
		}
		return s, fmt.Errorf("azure: growing agent pool %s for a successor of %s: %w", t.pool, t.machine, err)
	}
	s.Phase = cloud.SuccessorRequested
	return s, nil
}

// settleSuccessor names the successor once the grown pool has settled with
// exactly one machine that was not there at the plan.
func settleSuccessor(s cloud.Successor, t target, machines []machine, pl pool, before int32) (cloud.Successor, error) {
	switch {
	case !strings.EqualFold(pl.ProvisioningState, "Succeeded") && !strings.EqualFold(pl.ProvisioningState, "Failed"):
		return s, nil // still scaling
	}
	added, err := addedMachines(s, machines)
	if err != nil {
		return s, err
	}
	if strings.EqualFold(pl.ProvisioningState, "Failed") {
		if len(added) == 0 {
			// No machine came: the release can put the pool back.
			return s, fmt.Errorf("azure: agent pool %s failed to add a node for operation %s: %w", t.pool, s.Operation, cloud.ErrReplacementCapacity)
		}
		return s, fmt.Errorf("azure: agent pool %s failed while grown for operation %s (state Failed): %w", t.pool, s.Operation, cloud.ErrNeedsOperator)
	}
	if pl.Count != before+1 || len(added) != 1 {
		return s, fmt.Errorf("azure: agent pool %s settled at size %d with %d new machines for operation %s, not one at %d: %w",
			t.pool, pl.Count, len(added), s.Operation, before+1, cloud.ErrNeedsOperator)
	}
	s.Phase, s.ProviderID = cloud.SuccessorCreated, "azure://"+added[0].ResourceID
	return s, nil
}

// ReleaseSuccessor undoes an abandoned operation; see cloud.SuccessorProvider.
func (p *Provider) ReleaseSuccessor(ctx context.Context, s cloud.Successor) error {
	if s.UnrequestedPlan() {
		return nil
	}
	t, machines, pl, err := p.successorPool(ctx, s.Instance)
	if err != nil {
		return err
	}
	op, before, err := parseSuccessorTag(pl.Tags)
	if err != nil {
		return fmt.Errorf("azure: agent pool %s: %w: %w", t.pool, cloud.ErrNeedsOperator, err)
	}
	if op != operationToken(s.Operation) {
		if s.Phase == cloud.SuccessorPlanned {
			return nil // the tag goes with the new size: nothing reached AKS
		}
		if untouched, err := asPlanned(s, machines, pl); err != nil || !untouched {
			return fmt.Errorf("azure: agent pool %s no longer carries the %s tag of operation %s: %w", t.pool, successorTag, s.Operation, cloud.ErrNeedsOperator)
		}
		return nil // released before: the pool is as planned
	}
	if !strings.EqualFold(pl.ProvisioningState, "Succeeded") && !strings.EqualFold(pl.ProvisioningState, "Failed") {
		return fmt.Errorf("azure: agent pool %s is still scaling for operation %s (state %q): %w", t.pool, s.Operation, pl.ProvisioningState, cloud.ErrNeedsOperator)
	}
	added, err := addedMachines(s, machines)
	if err != nil {
		return err
	}
	if len(added) == 0 && pl.Count == before && strings.EqualFold(pl.ProvisioningState, "Succeeded") {
		// The successor is gone and the pool is at its size: only the tag of
		// an earlier release is left.
		return p.closeSuccessor(ctx, t.pool, s.Operation, before)
	}
	if s.Phase == cloud.SuccessorCreated {
		successor, ok := machineByProviderID(machines, s.ProviderID)
		if !ok || len(added) != 1 || added[0].Name != successor.Name {
			return fmt.Errorf("azure: agent pool %s does not hold just the successor %s of operation %s: %w", t.pool, s.ProviderID, s.Operation, cloud.ErrNeedsOperator)
		}
		if err := p.api.DeleteMachines(ctx, t.pool, []string{successor.Name}); err != nil {
			return fmt.Errorf("azure: deleting successor %s from agent pool %s: the result is unknown: %w: %w", successor.Name, t.pool, cloud.ErrNeedsOperator, err)
		}
		return p.closeSuccessor(ctx, t.pool, s.Operation, before)
	}
	if len(added) != 0 {
		return fmt.Errorf("azure: agent pool %s has %d machines that were not there at the plan of operation %s: %w", t.pool, len(added), s.Operation, cloud.ErrNeedsOperator)
	}
	// No machine came: restore the size and drop the tag in one write.
	tags := maps.Clone(pl.Tags)
	delete(tags, successorTag)
	if err := p.api.WritePool(ctx, t.pool, pl, before, tags); err != nil {
		return fmt.Errorf("azure: restoring agent pool %s to size %d: %w", t.pool, before, err)
	}
	return nil
}

// ReplaceWithSuccessor deletes the pinned machine once its successor of the
// same operation is in the pool, and leaves the pool at its size before the
// request. A delete whose result is unknown needs an operator: retrying it
// blindly, or releasing the successor, could leave the pool with neither.
func (p *Provider) ReplaceWithSuccessor(ctx context.Context, s cloud.Successor) error {
	t, machines, pl, err := p.successorPool(ctx, s.Instance)
	if err != nil {
		return err
	}
	op, before, err := parseSuccessorTag(pl.Tags)
	if err != nil {
		return fmt.Errorf("azure: agent pool %s: %w: %w", t.pool, cloud.ErrNeedsOperator, err)
	}
	if op != operationToken(s.Operation) {
		if t.machine == "" {
			return nil // replaced and closed on an earlier call
		}
		return fmt.Errorf("azure: agent pool %s no longer carries the %s tag of operation %s: %w", t.pool, successorTag, s.Operation, cloud.ErrNeedsOperator)
	}
	if _, ok := machineByProviderID(machines, s.ProviderID); !ok {
		return fmt.Errorf("azure: successor %s of operation %s is not in agent pool %s: %w", s.ProviderID, s.Operation, t.pool, cloud.ErrNeedsOperator)
	}
	if t.machine != "" {
		if t.machine != s.State[stateMachine] {
			return fmt.Errorf("azure: instance %s is machine %s, not the planned %s: %w", s.Instance, t.machine, s.State[stateMachine], cloud.ErrNeedsOperator)
		}
		if err := p.api.DeleteMachines(ctx, t.pool, []string{t.machine}); err != nil {
			return fmt.Errorf("azure: deleting machine %s from agent pool %s: the result is unknown: %w: %w", t.machine, t.pool, cloud.ErrNeedsOperator, err)
		}
	}
	return p.closeSuccessor(ctx, t.pool, s.Operation, before)
}

// closeSuccessor drops the operation's tag once the pool is back to its size.
// A pool still a node larger is never shrunk here: AKS would choose which
// machine goes.
func (p *Provider) closeSuccessor(ctx context.Context, name, operation string, before int32) error {
	pl, err := p.api.Pool(ctx, name)
	if err != nil {
		return fmt.Errorf("azure: reading agent pool %s: %w", name, err)
	}
	if pl.Count != before {
		return fmt.Errorf("azure: agent pool %s has %d nodes after operation %s, not %d; it was not shrunk: %w", name, pl.Count, operation, before, cloud.ErrNeedsOperator)
	}
	tags := maps.Clone(pl.Tags)
	delete(tags, successorTag)
	if err := p.api.WritePool(ctx, name, pl, pl.Count, tags); err != nil {
		return fmt.Errorf("azure: operation %s is done; clearing the %s tag of agent pool %s failed: %w", operation, successorTag, name, err)
	}
	return nil
}

// asPlanned reports whether the pool has settled exactly as it was planned:
// its size and machines.
func asPlanned(s cloud.Successor, machines []machine, pl pool) (bool, error) {
	before, members, err := plannedPool(s)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(pl.ProvisioningState, "Succeeded") && pl.Count == before && sameMembers(machineNames(machines), members), nil
}

func plannedPool(s cloud.Successor) (int32, []string, error) {
	n, err := strconv.ParseInt(s.State[stateCount], 10, 32)
	if err != nil || s.State[stateMembers] == "" {
		return 0, nil, fmt.Errorf("azure: successor operation %s has no recorded plan", s.Operation)
	}
	return int32(n), strings.Split(s.State[stateMembers], ","), nil
}

func addedMachines(s cloud.Successor, machines []machine) ([]machine, error) {
	_, members, err := plannedPool(s)
	if err != nil {
		return nil, err
	}
	var added []machine
	for _, m := range machines {
		if !slices.ContainsFunc(members, func(n string) bool { return strings.EqualFold(n, m.Name) }) {
			added = append(added, m)
		}
	}
	return added, nil
}

// machineNames lists machine names, lowercased and sorted.
func machineNames(machines []machine) []string {
	names := make([]string, 0, len(machines))
	for _, m := range machines {
		names = append(names, strings.ToLower(m.Name))
	}
	slices.Sort(names)
	return names
}

func sameMembers(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func machineByProviderID(machines []machine, providerID string) (machine, bool) {
	id := strings.TrimPrefix(providerID, "azure://")
	for _, m := range machines {
		if strings.EqualFold(m.ResourceID, id) {
			return m, true
		}
	}
	return machine{}, false
}

// operationToken is how an operation appears in the pool's tag.
func operationToken(operation string) string {
	sum := sha256.Sum256([]byte(operation))
	return "kn-" + hex.EncodeToString(sum[:8])
}

// parseSuccessorTag reads the successor tag: the operation token and the pool's
// size before it was grown. No tag is no operation under way.
func parseSuccessorTag(tags map[string]string) (string, int32, error) {
	v, ok := tags[successorTag]
	if !ok {
		return "", 0, nil
	}
	i := strings.LastIndex(v, ":")
	if i <= 0 {
		return "", 0, fmt.Errorf("malformed %s tag %q", successorTag, v)
	}
	n, err := strconv.ParseInt(v[i+1:], 10, 32)
	if err != nil || n < 0 {
		return "", 0, fmt.Errorf("malformed %s tag %q", successorTag, v)
	}
	return v[:i], int32(n), nil
}
