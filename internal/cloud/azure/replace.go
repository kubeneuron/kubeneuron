// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/kubeneuron/kubeneuron/internal/cloud/resizediff"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// replacingTag marks a fixed-size pool this provider has grown by one to
// replace a machine: "<machine>:<count before>". It is the replacement's
// identity, so a retry neither grows the pool twice nor shrinks it; it is a
// user tag of the agent pool, never a system label or annotation.
const replacingTag = "kubeneuron-replacing"

// capacityCodes are the ARM error codes AKS reports, outer or inner, when the
// new machine cannot be had.
var capacityCodes = []string{
	"QuotaExceeded", "InsufficientVCPUQuota", "ResourceGroupQuotaExceeded",
	"AllocationFailed", "ZonalAllocationFailed", "SkuNotAvailable",
	"OverconstrainedAllocationRequest", "OverconstrainedZonalAllocationRequest",
	"AKSCapacityError", "AksCapacityHeavyUsage", "SubnetIsFull", "InsufficientSubnetSize",
	"PublicIPCountLimitReached",
}

func capacityError(err error) bool {
	for _, code := range errorCodes(err) {
		if slices.ContainsFunc(capacityCodes, func(c string) bool { return strings.EqualFold(c, code) }) {
			return true
		}
	}
	return false
}

// replacement is one machine's replacement as the pool shows it now.
type replacement struct {
	target
	machines []machine
	state    pool
	started  bool  // the pool carries this machine's replacing tag
	before   int32 // the pool's size before it was grown
}

// Replace deletes the node's machine from its AKS agent pool. The controller
// has already cordoned and drained the node: AKS does neither.
//
// Two steps. The first makes sure a successor exists: a fixed-size pool is
// grown by one and the new machine must be listed before the old one goes, so
// no quota or capacity fails after a delete. A pool under the cluster
// autoscaler keeps its size, and the autoscaler decides whether another
// machine comes. The second deletes the machine and, on a grown pool, checks
// it came back to its size.
//
// It is idempotent: a machine already gone is a finished replacement, and the
// pool tag carries a started one across retries.
func (p *Provider) Replace(ctx context.Context, instanceID string) error {
	_, err := p.ReplaceNamed(ctx, instanceID)
	return err
}

var _ resizediff.Replacer = (*Provider)(nil)

// ReplaceNamed is Replace, and returns the providerID of the machine that
// joined a fixed-size pool for the replacement: the machines are listed
// before the pool is grown and again once it lists its new size, and exactly
// one is new. It returns "" when this call did not grow the pool (a retry, or
// an autoscaled pool), when the pool has more than resizediff.MaxMembers
// machines, or when not exactly one machine is new.
func (p *Provider) ReplaceNamed(ctx context.Context, instanceID string) (string, error) {
	return p.ReplaceNamedWithIntent(ctx, instanceID, nil)
}

// ReplaceNamedWithIntent lets the controller persist the target and the
// observed resize diff after capacity is ready and before sending the delete.
func (p *Provider) ReplaceNamedWithIntent(ctx context.Context, instanceID string, record func(pool, machine, successor string) error) (string, error) {
	r, err := p.replacement(ctx, instanceID)
	if err != nil || r == nil {
		return "", err
	}
	before, grows := r.machines, !r.started && !r.state.Autoscaling
	if err := p.ensureSuccessor(ctx, r); err != nil {
		return "", err
	}
	var named string
	if grows && len(before) <= resizediff.MaxMembers {
		added := resizediff.Added(names(before), names(r.machines), r.machine)
		for _, m := range r.machines {
			if added != "" && m.Name == added && m.ResourceID != "" {
				named = m.providerID()
			}
		}
	}
	if record != nil {
		if err := record(r.pool, r.machine, named); err != nil {
			return "", err
		}
	}
	if err := p.deleteMachine(ctx, r); err != nil {
		return "", err
	}
	return named, nil
}

// names lists the machines' names.
func names(machines []machine) []string {
	out := make([]string, 0, len(machines))
	for _, m := range machines {
		out = append(out, m.Name)
	}
	return out
}

// replacement reads the target, its pool and any replacement under way. A nil
// replacement with no error is a machine already replaced.
func (p *Provider) replacement(ctx context.Context, instanceID string) (*replacement, error) {
	t, machines, err := p.resolve(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	pl, err := p.api.Pool(ctx, t.pool)
	if err != nil {
		return nil, fmt.Errorf("azure: reading agent pool %s: %w", t.pool, err)
	}
	r := &replacement{target: t, machines: machines, state: pl}
	marked, before, err := parseMarker(pl.Tags)
	if err != nil {
		return nil, fmt.Errorf("azure: agent pool %s: %w", t.pool, err)
	}
	if !strings.EqualFold(pl.ProvisioningState, "Succeeded") {
		if marked != "" && strings.EqualFold(pl.ProvisioningState, "Failed") {
			return nil, fmt.Errorf("azure: agent pool %s failed while grown for the replacement of %s (state Failed); the pool needs an operator", t.pool, marked)
		}
		return nil, fmt.Errorf("azure: agent pool %s is busy (state %q); retry", t.pool, pl.ProvisioningState)
	}
	if t.machine == "" {
		// Gone: finished, unless this provider grew the pool for it and the
		// pool is not back to its size.
		if marked == "" || slices.ContainsFunc(machines, func(m machine) bool { return strings.EqualFold(m.Name, marked) }) {
			return nil, nil
		}
		return nil, p.finish(ctx, t.pool, pl, marked, before)
	}
	if op, _, _ := parseSuccessorTag(pl.Tags); op != "" {
		return nil, fmt.Errorf("azure: agent pool %s is preparing a successor for another operation (tag %s); waiting, one replacement per pool at a time", t.pool, op)
	}
	if marked != "" && !strings.EqualFold(marked, t.machine) {
		return nil, fmt.Errorf("azure: agent pool %s is replacing %s; waiting, one replacement per pool at a time", t.pool, marked)
	}
	r.started, r.before = marked != "", before
	if pl.Autoscaling && r.started {
		return nil, fmt.Errorf("azure: agent pool %s was grown for a replacement and is now autoscaled; it needs an operator", t.pool)
	}
	return r, nil
}

// ensureSuccessor grows a fixed-size pool by one, once, and requires the new
// machine to be listed. Nothing is deleted here, so any failure leaves the old
// machine in place.
func (p *Provider) ensureSuccessor(ctx context.Context, r *replacement) error {
	if r.state.Autoscaling {
		return nil
	}
	if !r.started {
		r.before = r.state.Count
		tags := maps.Clone(r.state.Tags)
		if tags == nil {
			tags = map[string]string{}
		}
		tags[replacingTag] = r.machine + ":" + strconv.Itoa(int(r.before))
		if err := p.api.WritePool(ctx, r.pool, r.state, r.before+1, tags); err != nil {
			if capacityError(err) {
				return fmt.Errorf("azure: growing agent pool %s to replace %s: %w: %w", r.pool, r.machine, cloud.ErrReplacementCapacity, err)
			}
			return fmt.Errorf("azure: growing agent pool %s to replace %s: %w; nothing was deleted", r.pool, r.machine, err)
		}
		r.started = true
		var err error
		if r.state, err = p.api.Pool(ctx, r.pool); err != nil {
			return fmt.Errorf("azure: reading agent pool %s: %w", r.pool, err)
		}
		if r.machines, err = p.api.Machines(ctx, r.pool); err != nil {
			return fmt.Errorf("azure: listing machines of agent pool %s: %w", r.pool, err)
		}
	}
	want := r.before + 1
	if !strings.EqualFold(r.state.ProvisioningState, "Succeeded") || r.state.Count != want || int32(len(r.machines)) != want {
		return fmt.Errorf("azure: agent pool %s lists %d machines at size %d (state %q), not the %d it was grown to: %w",
			r.pool, len(r.machines), r.state.Count, r.state.ProvisioningState, want, cloud.ErrReplacementCapacity)
	}
	return nil
}

// deleteMachine deletes the machine and, on a grown pool, checks the pool is
// back to its size before dropping the tag.
func (p *Provider) deleteMachine(ctx context.Context, r *replacement) error {
	if err := p.api.DeleteMachines(ctx, r.pool, []string{r.machine}); err != nil {
		return fmt.Errorf("azure: deleting machine %s from agent pool %s: %w", r.machine, r.pool, err)
	}
	if r.state.Autoscaling {
		return nil
	}
	pl, err := p.api.Pool(ctx, r.pool)
	if err != nil {
		return fmt.Errorf("azure: reading agent pool %s: %w", r.pool, err)
	}
	return p.finish(ctx, r.pool, pl, r.machine, r.before)
}

// finish drops the replacement tag once the pool is back to its size. A pool
// still a node larger is never shrunk here: that would delete a machine AKS
// chooses, so it is left for an operator.
func (p *Provider) finish(ctx context.Context, pool string, pl pool, machine string, before int32) error {
	if pl.Count != before {
		return fmt.Errorf("azure: machine %s is deleted but agent pool %s has %d nodes, not %d; it was not shrunk and needs an operator", machine, pool, pl.Count, before)
	}
	tags := maps.Clone(pl.Tags)
	delete(tags, replacingTag)
	if err := p.api.WritePool(ctx, pool, pl, pl.Count, tags); err != nil {
		return fmt.Errorf("azure: machine %s is replaced; clearing the replacement tag of agent pool %s failed: %w", machine, pool, err)
	}
	return nil
}

// parseMarker reads the replacing tag: the machine being replaced and the
// pool's size before it was grown. No tag is no replacement under way.
func parseMarker(tags map[string]string) (string, int32, error) {
	marker, ok := tags[replacingTag]
	if !ok {
		return "", 0, nil
	}
	name, count, ok := strings.Cut(marker, ":")
	n, err := strconv.ParseInt(count, 10, 32)
	if !ok || name == "" || err != nil || n < 0 {
		return "", 0, fmt.Errorf("malformed %s tag %q", replacingTag, marker)
	}
	return name, int32(n), nil
}
