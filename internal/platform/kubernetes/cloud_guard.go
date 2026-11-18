// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// RecoveryEligibility fails closed when native-repair evidence is unavailable.
// A veto capability alone does not prove an effective veto; until the provider
// supplies that evidence its pool remains unavailable for mutation.
func (p *Platform) RecoveryEligibility(ctx context.Context, pin cloud.NodeIdentity) (cloud.RecoveryEligibility, error) {
	v := cloud.RecoveryEligibility{Identity: pin, Mode: cloud.RepairUnknown}
	if err := pin.Validate(); err != nil {
		v.Reason = err.Error()
		return v, err
	}
	provider, ok := p.recycler.(cloud.RepairAwareProvider)
	if !ok {
		v.Reason = "provider has no native-repair contract"
		return v, nil
	}
	ref, err := p.CloudNode(ctx, pin.Node)
	if err != nil {
		v.Reason = "live node identity unavailable"
		return v, err
	}
	if err := pin.Match(ref.Identity); err != nil {
		v.Reason = err.Error()
		return v, err
	}
	policy, err := provider.RepairPolicy(ctx, ref)
	if err != nil {
		v.Reason = "native-repair policy unavailable"
		return v, err
	}
	v.Mode, v.Reason = policy.Mode, policy.Reason
	if policy.Mode != cloud.RepairOff {
		return v, nil
	}
	repair, err := provider.OpenRepair(ctx, ref)
	if err != nil {
		v.Reason = "open-repair query failed"
		return v, err
	}
	if !repair.Known || repair.Active || repair.Replaced {
		v.Reason = "cloud repair is active, changed the target, or cannot be excluded: " + repair.Reason
		return v, nil
	}
	if provider.Capabilities().Replace {
		if err := provider.CheckReplacePermission(ctx, pin.InstanceID); err != nil {
			v.Reason = "replacement permission unconfirmed"
			return v, err
		}
		v.ReplaceAllowed = true
	}
	if provider.Capabilities().ReinitializeInPlace {
		err = provider.CheckRecycle(ctx, pin.InstanceID)
		if err != nil && !errors.Is(err, cloud.ErrRecycleNotViable) {
			v.ReplaceAllowed = false
			v.Reason = "recycle viability query failed"
			return v, err
		}
		v.RecycleAllowed = err == nil
	}
	// Provider queries may take time: verify again before returning eligibility.
	current, err := p.CloudNode(ctx, pin.Node)
	if err == nil {
		err = pin.Match(current.Identity)
	}
	if err != nil {
		v.ReplaceAllowed, v.RecycleAllowed = false, false
		v.Reason = "node identity changed during eligibility check"
		return v, err
	}
	return v, nil
}

func (p *Platform) ReplacePinnedNode(ctx context.Context, pin cloud.NodeIdentity) error {
	v, err := p.RecoveryEligibility(ctx, pin)
	if err != nil {
		return err
	}
	if !v.ReplaceAllowed {
		return fmt.Errorf("cloud replacement unavailable: %s", v.Reason)
	}
	// Never substitute an instance discovered by a later name lookup.
	return p.recycler.Replace(ctx, pin.InstanceID)
}

func (p *Platform) RecyclePinnedNode(ctx context.Context, pin cloud.NodeIdentity) error {
	v, err := p.RecoveryEligibility(ctx, pin)
	if err != nil {
		return err
	}
	if !v.RecycleAllowed {
		return fmt.Errorf("cloud recycle unavailable: %s", v.Reason)
	}
	return p.recycler.Recycle(ctx, pin.InstanceID)
}

// RecyclePinnedNodeVerified keeps post-operation observation inside the guarded
// operation. A successful provider call alone does not prove a new kubelet boot.
func (p *Platform) RecyclePinnedNodeVerified(ctx context.Context, pin cloud.NodeIdentity) (cloud.RecycleResult, error) {
	result := cloud.RecycleResult{Before: pin}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Minute)
		defer cancel()
	}
	if err := p.RecyclePinnedNode(ctx, pin); err != nil {
		return result, err
	}
	for {
		ref, err := p.CloudNode(ctx, pin.Node)
		if err == nil {
			sameBoot := ref.Identity
			sameBoot.BootID = pin.BootID
			if err := pin.Match(sameBoot); err != nil {
				return result, err
			}
			if ref.Ready && ref.Identity.BootID != pin.BootID {
				result.After, result.CompletedAt = ref.Identity, time.Now()
				return result, result.Validate(pin)
			}
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, fmt.Errorf("recycled instance has no verified new Ready boot: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
