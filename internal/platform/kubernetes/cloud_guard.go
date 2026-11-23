// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/cloud/resizediff"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// RecoveryEligibility fails closed when native-repair evidence is unavailable.
// A veto capability alone does not prove an effective veto; until the provider
// supplies that evidence its pool remains unavailable for mutation.
func (p *Platform) RecoveryEligibility(ctx context.Context, pin cloud.NodeIdentity) (cloud.RecoveryEligibility, error) {
	v, _, err := p.PoolRecoveryEligibility(ctx, pin)
	return v, err
}

// PoolRecoveryEligibility is RecoveryEligibility with, when the cloud's own
// repair is on, that policy's hold with the shorter texts a pool row shows.
func (p *Platform) PoolRecoveryEligibility(ctx context.Context, pin cloud.NodeIdentity) (cloud.RecoveryEligibility, *cloud.RecoveryHold, error) {
	v := cloud.RecoveryEligibility{Identity: pin, Mode: cloud.RepairUnknown}
	if err := pin.Validate(); err != nil {
		v.Reason = err.Error()
		return v, nil, err
	}
	provider, ok := p.recycler.(cloud.RepairAwareProvider)
	if !ok {
		v.Reason = "provider has no native-repair contract"
		return v, nil, nil
	}
	ref, err := p.CloudNode(ctx, pin.Node)
	if err != nil {
		v.Reason = "live node identity unavailable"
		return v, nil, err
	}
	if err := pin.Match(ref.Identity); err != nil {
		v.Reason = err.Error()
		return v, nil, err
	}
	policy, err := provider.RepairPolicy(ctx, ref)
	if err != nil {
		v.Reason = holdMessage(err, "native-repair policy unavailable")
		return v, nil, err
	}
	v.Mode, v.Reason = policy.Mode, policy.Message()
	switch policy.Mode {
	case cloud.RepairOff:
	case cloud.RepairNotReadyOnly:
		// The cloud's own repair may act on a NotReady node at any time.
		if !ref.Ready {
			v.Reason = "node is NotReady; the cloud's own repair may act on it"
			return v, nil, nil
		}
	case cloud.RepairOn:
		hold := policy.Pool
		if hold == nil {
			hold = &cloud.RecoveryHold{Code: cloud.HoldNativeRepair, Reason: policy.Reason, Action: policy.Action}
		}
		return v, hold, nil
	default:
		return v, nil, nil
	}
	repair, err := provider.OpenRepair(ctx, ref)
	if err != nil {
		v.Reason = holdMessage(err, "open-repair query failed")
		return v, nil, err
	}
	if !repair.Known || repair.Active || repair.Replaced {
		v.Reason = "cloud repair is active, changed the target, or cannot be excluded: " + repair.Reason
		return v, nil, nil
	}
	if provider.Capabilities().Replace {
		if err := provider.CheckReplacePermission(ctx, pin.InstanceID); err != nil {
			v.Reason = holdMessage(err, err.Error())
			return v, nil, err
		}
		v.ReplaceAllowed = true
	}
	if provider.Capabilities().ReinitializeInPlace {
		err = provider.CheckRecycle(ctx, pin.InstanceID)
		if err != nil && !errors.Is(err, cloud.ErrRecycleNotViable) {
			v.ReplaceAllowed = false
			v.Reason = "recycle viability query failed"
			return v, nil, err
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
		return v, nil, err
	}
	if policy.Mode == cloud.RepairNotReadyOnly && !current.Ready {
		v.ReplaceAllowed, v.RecycleAllowed = false, false
		v.Reason = "node became NotReady during eligibility check; the cloud's own repair may act on it"
	}
	return v, nil, nil
}

// holdMessage is a provider's operator-facing text for err, if it gave one.
func holdMessage(err error, fallback string) string {
	var hold *cloud.RecoveryHold
	if errors.As(err, &hold) {
		return hold.Message()
	}
	return fallback
}

func (p *Platform) ReplacePinnedNode(ctx context.Context, pin cloud.NodeIdentity) error {
	_, err := p.ReplacePinnedNodeNamed(ctx, pin)
	return err
}

func (p *Platform) ReplacePinnedNodeNamed(ctx context.Context, pin cloud.NodeIdentity) (string, error) {
	return p.ReplacePinnedNodeWithIntent(ctx, pin, nil)
}

// ReplacePinnedNodeWithIntent runs the provider's pre-delete callback only
// after the normal identity and cloud eligibility checks have passed.
func (p *Platform) ReplacePinnedNodeWithIntent(ctx context.Context, pin cloud.NodeIdentity, record func(pool, machine, successor string) error) (string, error) {
	if err := p.replaceEligible(ctx, pin); err != nil {
		return "", err
	}
	if record != nil {
		provider, ok := p.recycler.(interface {
			ReplaceNamedWithIntent(context.Context, string, func(string, string, string) error) (string, error)
		})
		if !ok {
			return "", cloud.ErrRemoveFirstUnplanned
		}
		return provider.ReplaceNamedWithIntent(ctx, pin.InstanceID, record)
	}
	// Never substitute an instance discovered by a later name lookup.
	if named, ok := p.recycler.(resizediff.Replacer); ok {
		return named.ReplaceNamed(ctx, pin.InstanceID)
	}
	return "", p.recycler.Replace(ctx, pin.InstanceID)
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
