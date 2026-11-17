// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// cloudRecoveryEligibility is the shared pre-authorization query. A negative
// verdict is execution unavailability, not a request for human authorization.
// It does not change incident state or dispatch a side effect. Callers retain
// the reason for status, and must not turn an error into a positive verdict.
// GuardedNodeRecycler repeats these checks at the cloud mutation boundary.
func (c *Controller) cloudRecoveryEligibility(ctx context.Context, inc *types.Incident) (cloud.RecoveryEligibility, error) {
	v := cloud.RecoveryEligibility{Mode: cloud.RepairUnknown}
	if inc.CloudIdentity == nil {
		v.Reason = "incident has no pinned cloud identity; recovery unavailable"
		return v, nil
	}
	v.Identity = *inc.CloudIdentity
	if inc.Target.Node != v.Identity.Node {
		v.Reason = "incident target does not match pinned cloud identity"
		return v, fmt.Errorf("%s", v.Reason)
	}
	guard, ok := c.platform.(platform.GuardedNodeRecycler)
	if !ok {
		v.Reason = "platform has no guarded cloud recovery contract"
		return v, nil
	}
	return guard.RecoveryEligibility(ctx, v.Identity)
}

// A refusal at the final boundary must hold the same step, never escalate.
var errCloudRecoveryUnavailable = errors.New("cloud recovery unavailable")

func (c *Controller) requireCloudRecovery(ctx context.Context, inc *types.Incident, step *playbook.Step) error {
	switch step.Action {
	case "platform.cordon", "platform.drain", "platform.evict_gpu_workload", "platform.recycle_node", "platform.replace_node":
	default:
		return nil // Observation and compensating cleanup must remain possible.
	}
	direct := step.Action == "platform.recycle_node" || step.Action == "platform.replace_node"
	if !direct && inc.CloudIdentity == nil && !c.cloudRecoveryLadder(ctx, inc.Playbook) {
		return nil
	}
	v, err := c.cloudRecoveryEligibility(ctx, inc)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errCloudRecoveryUnavailable, v.Reason, err)
	}
	if !v.ReplaceAllowed || (step.Action == "platform.recycle_node" && !v.RecycleAllowed) {
		return fmt.Errorf("%w: %s", errCloudRecoveryUnavailable, v.Reason)
	}
	return nil
}

// supersedeCloudTarget closes only a positively identified changed target. It
// deliberately does not uncordon: a same-UID reboot retains the existing hold.
func (c *Controller) supersedeCloudTarget(ctx context.Context, inc *types.Incident, err error) error {
	if !errors.Is(err, cloud.ErrIdentityChanged) {
		return err
	}
	return c.transition(ctx, inc, types.StateSuperseded, "system", "cloud-target-changed",
		"target identity changed; recovery unverified; any existing cordon is retained for a same-UID node", nil)
}
