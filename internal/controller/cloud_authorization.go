// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"time"

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
	// This cache only paces waiting assessments. requireCloudRecovery always
	// queries the provider again immediately before a disruptive action.
	c.cloudAssessmentMu.Lock()
	defer c.cloudAssessmentMu.Unlock()
	now := time.Now()
	if c.cloudAssessments == nil {
		c.cloudAssessments = make(map[string]*cloudAssessment)
	}
	for id, old := range c.cloudAssessments {
		if now.Sub(old.at) > 10*time.Minute {
			delete(c.cloudAssessments, id)
		}
	}
	key := cloudAssessmentKey{state: inc.State, step: stepContentHash(inc.Playbook, step), runtime: c.runtimeConfig(ctx)}
	if inc.CloudIdentity != nil {
		key.pin = *inc.CloudIdentity
	}
	entry := c.cloudAssessments[inc.ID]
	waiting := inc.State == types.StateEvaluating || inc.State == types.StateAwaitingApproval
	if entry != nil && entry.key == key && waiting && now.Sub(entry.at) < time.Minute {
		return entry.held, nil
	}
	if entry == nil {
		entry = &cloudAssessment{}
	}
	held, err := c.assessUnavailableCloudRecovery(ctx, inc, step, entry)
	if err == nil && waiting && inc.State == key.state {
		entry.key, entry.at, entry.held = key, now, held
		c.cloudAssessments[inc.ID] = entry
	} else {
		delete(c.cloudAssessments, inc.ID)
	}
	return held, err
}

type cloudAssessmentKey struct {
	state   types.IncidentState
	step    string
	pin     cloud.NodeIdentity
	runtime *RuntimeConfig
}
type cloudAssessment struct {
	key         cloudAssessmentKey
	at          time.Time
	held        bool
	auditLoaded bool
	last        *types.AuditEntry
}

func (c *Controller) assessUnavailableCloudRecovery(ctx context.Context, inc *types.Incident, step *playbook.Step, entry *cloudAssessment) (bool, error) {
	verdict, err := c.cloudRecoveryEligibility(ctx, inc)
	if errors.Is(err, cloud.ErrIdentityChanged) {
		return true, c.supersedeCloudTarget(ctx, inc, err)
	}
	if err == nil && inc.CloudIdentity != nil {
		if mismatch := inc.CloudIdentity.Match(verdict.Identity); errors.Is(mismatch, cloud.ErrIdentityChanged) {
			return true, c.supersedeCloudTarget(ctx, inc, mismatch)
		}
	}
	allowed := err == nil && cloudRecoveryAllowed(verdict, step.Action)

	reason := verdict.Reason
	if allowed && (inc.CloudIdentity == nil || inc.CloudIdentity.Match(verdict.Identity) != nil) {
		allowed = false
		reason = "provider eligibility does not match pinned cloud identity"
	}
	if reason == "" {
		reason = "provider did not establish recovery eligibility"
	}
	// Do not copy raw provider errors into a persisted, user-visible message.
	if !entry.auditLoaded {
		trail, readErr := c.store.AuditTrail(ctx, inc.ID)
		if readErr != nil {
			return true, readErr
		}
		for i := len(trail) - 1; i >= 0; i-- {
			if trail[i].Action == "provider-recovery-unavailable" || trail[i].Action == "provider-recovery-available" {
				entry.last = trail[i]
				break
			}
		}
		entry.auditLoaded = true
	}
	last := entry.last
	if allowed {
		if last != nil && last.Action == "provider-recovery-unavailable" {
			if err := c.appendAudit(ctx, inc, "system", "provider-recovery-available", "provider eligibility restored for the pinned identity; step authorization still required"); err != nil {
				return true, err
			}
			entry.last = &types.AuditEntry{Action: "provider-recovery-available"}
		}
		return false, nil
	}
	if last != nil && last.Action == "provider-recovery-unavailable" && last.Result == reason {
		return true, nil
	}
	if err := c.appendAudit(ctx, inc, "system", "provider-recovery-unavailable", reason); err != nil {
		return true, err
	}
	entry.last = &types.AuditEntry{Action: "provider-recovery-unavailable", Result: reason}
	return true, nil
}
