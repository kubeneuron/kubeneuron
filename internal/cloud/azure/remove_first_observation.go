// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"fmt"
	"strings"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// ObserveRemoveFirstDelete uses the pool and machine saved before deletion.
// A listed ARM path can be reused. The platform must independently check the
// original Kubernetes Node UID and providerID before permitting another delete.
func (p *Provider) ObserveRemoveFirstDelete(ctx context.Context, instanceID, poolName, machineName string) (removefirst.Observation, error) {
	if poolName == "" || machineName == "" {
		return removefirst.Unknown, nil
	}
	target, members, err := p.resolve(ctx, instanceID)
	if err != nil {
		return removefirst.Unknown, err
	}
	if !strings.EqualFold(target.pool, poolName) {
		return removefirst.IdentityChanged, nil
	}
	result := removefirst.Deleted
	for _, member := range members {
		name := strings.EqualFold(member.Name, machineName)
		path := strings.EqualFold(member.ResourceID, instanceID)
		if name != path {
			return removefirst.IdentityChanged, nil
		}
		if name {
			result = removefirst.Listed
		}
	}
	return result, nil
}

// DeleteListedMachine rechecks membership immediately before sending the
// name-based delete. Its caller must first check the live Node identity.
func (p *Provider) DeleteListedMachine(ctx context.Context, instanceID, poolName, machineName string) error {
	status, err := p.ObserveRemoveFirstDelete(ctx, instanceID, poolName, machineName)
	if err != nil {
		return err
	}
	if status != removefirst.Listed {
		return cloud.ErrIdentityChanged
	}
	return p.api.DeleteMachines(ctx, poolName, []string{machineName})
}

// ObserveRemoveFirstGrow reads a fixed-size pool whose grow by one was
// recorded before it was sent. It never writes. The marker tag is written in
// the same request as the new size, so the tag, the absolute target and as
// many listed machines together are the grow; no tag at the size before is a
// grow that has not reached the pool, or not yet. Anything else is a change
// this provider did not make and must not repeat.
func (p *Provider) ObserveRemoveFirstGrow(ctx context.Context, g removefirst.Grow) (removefirst.GrowObservation, error) {
	pl, err := p.api.Pool(ctx, g.Pool)
	if err != nil {
		return removefirst.GrowObservation{}, err
	}
	if !strings.EqualFold(pl.ProvisioningState, "Succeeded") {
		if strings.EqualFold(pl.ProvisioningState, "Failed") {
			return removefirst.GrowObservation{State: removefirst.GrowChanged,
				Detail: "its last update failed"}, nil
		}
		return removefirst.GrowObservation{}, nil
	}
	tag, tagged := pl.Tags[replacingTag]
	obs := removefirst.GrowObservation{Size: pl.Count}
	switch {
	case pl.Autoscaling:
		obs.State, obs.Detail = removefirst.GrowChanged, "it is now autoscaled"
	case !tagged && pl.Count == g.Before:
		obs.State = removefirst.GrowNotApplied
	case tagged && tag == g.Marker && pl.Count == g.Target:
		machines, err := p.api.Machines(ctx, g.Pool)
		if err != nil {
			return removefirst.GrowObservation{}, err
		}
		if int32(len(machines)) != g.Target {
			// The size is set; the new machine is not listed yet.
			return removefirst.GrowObservation{}, nil
		}
		obs.State = removefirst.GrowApplied
	case tagged && tag != g.Marker:
		obs.State, obs.Detail = removefirst.GrowChanged, "it is marked for another replacement"
	default:
		obs.State, obs.Detail = removefirst.GrowChanged, fmt.Sprintf("it has %d nodes", pl.Count)
	}
	return obs, nil
}

// SettleGrownPool finishes a fixed-size pool grown for machine once the
// machine is gone: the replacing tag is dropped when the pool is back to the
// size the tag recorded. A pool without this machine's tag needs nothing. A
// pool still one node larger is never shrunk here, and is reported.
func (p *Provider) SettleGrownPool(ctx context.Context, poolName, machineName string) (removefirst.SettleResult, error) {
	pl, err := p.api.Pool(ctx, poolName)
	if err != nil {
		return removefirst.SettleResult{}, err
	}
	marked, before, err := parseMarker(pl.Tags)
	if err != nil {
		return removefirst.SettleResult{}, fmt.Errorf("azure: agent pool %s: %w", poolName, err)
	}
	if marked == "" || !strings.EqualFold(marked, machineName) {
		return removefirst.SettleResult{}, nil
	}
	if !strings.EqualFold(pl.ProvisioningState, "Succeeded") {
		return removefirst.SettleResult{}, fmt.Errorf("azure: agent pool %s is busy (state %q); retry", poolName, pl.ProvisioningState)
	}
	res := removefirst.SettleResult{Size: pl.Count, Before: before, Extra: !pl.Autoscaling && pl.Count == before+1}
	if pl.Count != before {
		return res, fmt.Errorf("%w: machine %s is deleted but agent pool %s has %d nodes, not %d", cloud.ErrNeedsOperator, machineName, poolName, pl.Count, before)
	}
	if err := p.finish(ctx, poolName, pl, machineName, before); err != nil {
		return res, err
	}
	res.Untagged = true
	return res, nil
}
