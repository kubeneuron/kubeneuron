// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// authorizeAndStartStep is the decision boundary shared by evaluation and
// approval resumption. Authorization never bypasses startStep's live safety
// checks. Policy authorization can extend this boundary without changing the
// recovery ladder or treating a parked state as proof of consent.
func (c *Controller) authorizeAndStartStep(ctx context.Context, inc *types.Incident, step *playbook.Step) error {
	// Provider first, so a changed target is superseded whatever the policy
	// says; then the policy's recovery settings. Both before any approval.
	// Before a recycle-only rung starts, the provider is asked about its
	// recycle: a refused recycle skips the rung before its cordon.
	assess := step
	if recycle := c.rungRecycle(ctx, inc); recycle != nil {
		assess = recycle
	}
	held, skip, err := c.holdUnavailableCloudRecovery(ctx, inc, assess)
	if held || err != nil {
		return err
	}
	if skip != "" {
		return c.skipRecycle(ctx, inc, step, skip)
	}
	// A parked round resumes under its own checks: startStep applies the
	// policy's recovery settings again before anything runs.
	if inc.State == types.StateAwaitingApproval {
		return c.resumeAwaitingApproval(ctx, inc)
	}
	if handled, err := c.holdOrSkipByPolicy(ctx, inc, step); handled || err != nil {
		return err
	}
	if step.NeedsApproval() {
		pol, automatic := c.automaticPolicyFor(ctx, inc)
		if !automatic {
			return c.parkForApproval(ctx, inc, step, "step requires human approval")
		}
		verdict, reason := c.authorizeByPolicy(ctx, inc, step, pol)
		switch verdict {
		case policyRun:
			if err := c.startStep(ctx, inc, step, policyActor(pol.Name)); err != nil {
				return err
			}
			if inc.State == types.StateExecuting {
				if err := c.appendAudit(ctx, inc, policyActor(pol.Name), "policy-authorised",
					policyAuthorisationNote(pol, time.Now())+": "+step.Name); err != nil {
					c.log.Error("policy authorisation audit append failed", "incident", inc.ID, "err", err)
				}
			}
			return nil
		case policyHold:
			c.deferStep(inc, step, metrics.DeferAutomaticLimit)
			c.logOnceStepHeld(inc, step, pol, reason)
			return nil
		default:
			return c.parkForApproval(ctx, inc, step,
				fmt.Sprintf("automatic policy %s did not authorise this step: %s", pol.Name, reason))
		}
	}
	return c.startStep(ctx, inc, step, "system")
}
