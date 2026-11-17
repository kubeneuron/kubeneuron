// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// holdUnavailableCloudRecovery runs before either human or policy authorization.
// An approval cannot supply missing provider capabilities or repair evidence.
func (c *Controller) holdUnavailableCloudRecovery(ctx context.Context, inc *types.Incident, step *playbook.Step) (bool, error) {
	if !c.cloudRecoveryLadder(ctx, inc.Playbook) && !cloudRecoveryAction(step.Action) {
		return false, nil
	}
	verdict, err := c.cloudRecoveryEligibility(ctx, inc)
	if errors.Is(err, cloud.ErrIdentityChanged) {
		return true, c.supersedeCloudTarget(ctx, inc, err)
	}
	if err == nil && inc.CloudIdentity != nil {
		if mismatch := inc.CloudIdentity.Match(verdict.Identity); errors.Is(mismatch, cloud.ErrIdentityChanged) {
			return true, c.supersedeCloudTarget(ctx, inc, mismatch)
		}
	}
	allowed := err == nil && verdict.ReplaceAllowed && (verdict.Mode == cloud.RepairOff || verdict.Mode == cloud.RepairVeto)
	if step.Action == "platform.recycle_node" {
		allowed = allowed && verdict.RecycleAllowed
	}
	reason := verdict.Reason
	if allowed && (inc.CloudIdentity == nil || inc.CloudIdentity.Match(verdict.Identity) != nil) {
		allowed = false
		reason = "provider eligibility does not match pinned cloud identity"
	}
	if reason == "" {
		reason = "provider did not establish recovery eligibility"
	}
	// Do not copy raw provider errors into a persisted, user-visible message.
	trail, readErr := c.store.AuditTrail(ctx, inc.ID)
	if readErr != nil {
		return true, readErr
	}
	var last *types.AuditEntry
	for i := len(trail) - 1; i >= 0; i-- {
		if trail[i].Action == "provider-recovery-unavailable" || trail[i].Action == "provider-recovery-available" {
			last = trail[i]
			break
		}
	}
	if allowed {
		if last != nil && last.Action == "provider-recovery-unavailable" {
			if err := c.appendAudit(ctx, inc, "system", "provider-recovery-available", "provider eligibility restored for the pinned identity; step authorization still required"); err != nil {
				return true, err
			}
		}
		return false, nil
	}
	if last != nil && last.Action == "provider-recovery-unavailable" && last.Result == reason {
		return true, nil
	}
	return true, c.appendAudit(ctx, inc, "system", "provider-recovery-unavailable", reason)
}
