// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The policy's recovery settings (replacement strategy, recycle.enabled) are
// checked at the same point as the provider's eligibility, before any
// authorization and before the first disruptive step, in every mode. Two
// outcomes besides "go on":
//
//   - hold: nothing on the incident's cloud recovery path may run under the
//     policy, so the node is not cordoned or drained for a recovery that
//     cannot follow. No approval is asked; a person's approval cannot make
//     the step available.
//   - skip: a recycle the policy or the provider does not allow is not
//     applicable. It is not attempted and not counted as failed; the ladder
//     continues with its next step, or with its escalation playbook.

// recoveryPath summarises the cloud recovery steps reachable from a playbook
// through its escalation chain.
type recoveryPath struct {
	recycle, replace bool
}

func (c *Controller) cloudRecoveryPath(ctx context.Context, name string) recoveryPath {
	var path recoveryPath
	engine := c.runtimeConfig(ctx).Engine
	if engine == nil {
		return path
	}
	seen := map[string]bool{}
	for name != "" && !seen[name] {
		seen[name] = true
		book, ok := engine.Playbook(name)
		if !ok {
			return path
		}
		for _, step := range book.Steps {
			switch step.Action {
			case "platform.recycle_node":
				path.recycle = true
			case "platform.replace_node":
				path.replace = true
			}
		}
		name = book.OnFailure.EscalateTo
	}
	return path
}

func (c *Controller) incidentPolicy(ctx context.Context, inc *types.Incident) playbook.Policy {
	if engine := c.runtimeConfig(ctx).Engine; engine != nil {
		pol, _ := engine.PolicyFor(inc.Class, inc.Vendor)
		return pol
	}
	return playbook.Policy{}
}

const (
	addFirstUnavailable = "add-first replacement is not available from the cloud provider yet, and the policy does not name remove-first"
	recycleNotEnabled   = "the policy does not enable the restart through the cloud API (recycle.enabled)"
)

// policyRecoveryPreflight returns a hold reason or a skip reason for the
// step, or neither.
func (c *Controller) policyRecoveryPreflight(ctx context.Context, inc *types.Incident, step *playbook.Step) (hold, skip string) {
	pol := c.incidentPolicy(ctx, inc)
	addFirst := pol.ReplacementStrategy.Effective() == recoverymode.AddFirst
	switch step.Action {
	case "platform.replace_node":
		if addFirst {
			return "recovery unavailable: " + addFirstUnavailable, ""
		}
	case "platform.recycle_node":
		if !pol.RecycleEnabled {
			return "", recycleNotEnabled
		}
	case "platform.cordon", "platform.drain", "platform.evict_gpu_workload":
		path := c.cloudRecoveryPath(ctx, inc.Playbook)
		switch {
		case path.replace && addFirst:
			// Even with a recycle enabled before it: the recycle may fail and
			// the replace it falls back to could not run, so nothing on the
			// path may disrupt the node until the policy names remove-first.
			return "recovery unavailable: " + addFirstUnavailable + "; the node is not cordoned or drained", ""
		case path.recycle && !path.replace && !pol.RecycleEnabled:
			return "recovery unavailable: " + recycleNotEnabled + ", and the path has no replace step; the node is not cordoned or drained", ""
		}
		if !pol.RecycleEnabled && c.rungRecycle(ctx, inc) != nil {
			return "", recycleNotEnabled
		}
	}
	return "", ""
}

// holdOrSkipByPolicy applies the preflight at the authorization boundary.
// It reports whether it handled the step.
func (c *Controller) holdOrSkipByPolicy(ctx context.Context, inc *types.Incident, step *playbook.Step) (bool, error) {
	hold, skip := c.policyRecoveryPreflight(ctx, inc, step)
	if hold != "" {
		c.deferStep(inc, step, metrics.DeferRecoveryUnavailable)
		c.logOnceStepUnavailable(inc, step, hold)
		return true, nil
	}
	if err := c.recordRecoveryAvailable(ctx, inc); err != nil {
		return true, err
	}
	if skip != "" {
		return true, c.skipRecycle(ctx, inc, step, skip)
	}
	return false, nil
}

// recycleOnly reports whether a playbook recovers only by recycling: it has a
// recycle step and no replace step.
func recycleOnly(book *playbook.Playbook) bool {
	recycle := false
	for _, step := range book.Steps {
		switch step.Action {
		case "platform.replace_node":
			return false
		case "platform.recycle_node":
			recycle = true
		}
	}
	return recycle
}

// replaceAfter reports whether the playbook has a replace step after index.
func replaceAfter(book *playbook.Playbook, index int) bool {
	for _, step := range book.Steps[index+1:] {
		if step.Action == "platform.replace_node" {
			return true
		}
	}
	return false
}

// skipRecycle moves past a recycle that is not applicable. It is not a
// failure and not an escalation: Attempt is unchanged, no escalation is
// counted and nothing is spent against the cap. Two places allow it:
//
//   - Before a rung starts (step 0, nothing of it has run) when the rung
//     recovers only by recycling: the incident moves to the first rung on
//     its escalation path that does not. Nothing at this Attempt has run, so
//     the new rung's action IDs (incident, step, attempt) are fresh. The path
//     is walked once with a visited set; a cycle, or a path that ends, holds.
//   - Within a rung, to the next step, when a replace follows in the same
//     rung. Step indexes only grow, so the IDs stay fresh.
//
// A recycle that becomes inapplicable after its rung has started, with no
// replace after it in the rung, means the rung cannot complete: that is an
// escalation like any failed step. A skip alone never leads to VERIFYING.
func (c *Controller) skipRecycle(ctx context.Context, inc *types.Incident, step *playbook.Step, reason string) error {
	if inc.State != types.StateEvaluating {
		// A parked step keeps its approval round; it is held until the round
		// ends. Moving past it from AWAITING_APPROVAL is not a legal edge.
		return c.holdRecycle(inc, step, reason)
	}
	engine := c.runtimeConfig(ctx).Engine
	book, ok := engine.Playbook(inc.Playbook)
	if !ok {
		return c.holdRecycle(inc, step, reason)
	}
	if inc.StepIndex == 0 && recycleOnly(book) {
		// StepIndex 0 is not proof that nothing of the rung ran at this
		// Attempt (a failed step 0, a resume after a restart). The rung is
		// skipped only when the record shows it did not start.
		started, err := c.rungStarted(ctx, inc, book)
		if err != nil {
			c.log.Warn("cannot tell whether the rung started; holding the recycle skip",
				"incident", inc.ID, "err", err)
			return c.holdRecycle(inc, step, reason+"; whether the rung already started cannot be read")
		}
		if started {
			return c.escalate(ctx, inc, "recycle not applicable after the rung started: "+reason)
		}
		seen := map[string]bool{inc.Playbook: true}
		name := inc.Playbook
		for {
			next, ok := engine.Escalation(name)
			if !ok {
				return c.holdRecycle(inc, step, reason+"; no rung on the escalation path can run")
			}
			if seen[next.Name] {
				return c.holdRecycle(inc, step, reason+"; the escalation path returns to "+next.Name+" without a rung that can run")
			}
			seen[next.Name] = true
			name = next.Name
			if !recycleOnly(next) {
				break
			}
		}
		previous := inc.Playbook
		inc.Playbook = name
		return c.transition(ctx, inc, types.StateEvaluating, "system", "recycle-skipped",
			fmt.Sprintf("recycle skipped: %s; continuing with %s (from %s)", reason, name, previous), nil)
	}
	if step.Action == "platform.recycle_node" && replaceAfter(book, inc.StepIndex) {
		inc.StepIndex++
		return c.transition(ctx, inc, types.StateEvaluating, "system", "recycle-skipped",
			fmt.Sprintf("recycle skipped: %s; continuing with step %s of %s", reason, book.Steps[inc.StepIndex].Name, inc.Playbook), nil)
	}
	return c.escalate(ctx, inc, "recycle not applicable after the rung started: "+reason)
}

// rungStarted reports whether any step of the incident's current rung has
// started or has a result at the current Attempt: an action stored under
// one of the rung's action IDs, or a step start or outcome in the audit
// trail since the row that bound the rung (opening, escalation or skip).
func (c *Controller) rungStarted(ctx context.Context, inc *types.Incident, book *playbook.Playbook) (bool, error) {
	for i := range book.Steps {
		probe := types.Incident{ID: inc.ID, StepIndex: i, Attempt: inc.Attempt}
		_, err := c.store.GetAction(ctx, actionID(&probe))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
	}
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil {
		return false, err
	}
	for i := len(trail) - 1; i >= 0; i-- {
		e := trail[i]
		switch e.Action {
		case "open", "escalate", "recycle-skipped":
			return false, nil
		}
		if e.ToState == types.StateExecuting || e.FromState == types.StateExecuting || e.ToState == types.StateVerifying {
			return true, nil
		}
	}
	return false, nil
}

func (c *Controller) holdRecycle(inc *types.Incident, step *playbook.Step, reason string) error {
	c.deferStep(inc, step, metrics.DeferRecoveryUnavailable)
	c.logOnceStepUnavailable(inc, step, "recovery unavailable: "+reason)
	return nil
}

// rungRecycle returns the recycle step to assess before a recycle-only rung
// starts, or nil: the provider's answer for the recycle decides whether the
// rung runs at all, before its cordon.
func (c *Controller) rungRecycle(ctx context.Context, inc *types.Incident) *playbook.Step {
	if inc.StepIndex != 0 || inc.State != types.StateEvaluating {
		return nil
	}
	engine := c.runtimeConfig(ctx).Engine
	if engine == nil {
		return nil
	}
	book, ok := engine.Playbook(inc.Playbook)
	if !ok || !recycleOnly(book) {
		return nil
	}
	for i := range book.Steps {
		if book.Steps[i].Action == "platform.recycle_node" {
			return &book.Steps[i]
		}
	}
	return nil
}

// lastPolicyAssessment returns the last policy-scope assessment of the
// incident: its denial text, or "" when there is none or it was lifted. The
// trail is read once per incident per process. Callers hold the mutex.
func (c *Controller) lastPolicyAssessment(ctx context.Context, id string) (string, error) {
	// A cache of the trail, so dropping it only costs a re-read; bounded
	// because incident IDs are not.
	if c.policyAssessed == nil || len(c.policyAssessed) > 4096 {
		c.policyAssessed = map[string]string{}
	}
	if last, ok := c.policyAssessed[id]; ok {
		return last, nil
	}
	trail, err := c.store.AuditTrail(ctx, id)
	if err != nil {
		return "", err
	}
	last := ""
	for i := len(trail) - 1; i >= 0; i-- {
		if trail[i].Action == "recovery-unavailable" {
			last = trail[i].Result
			break
		}
		if trail[i].Action == "recovery-available" {
			break
		}
	}
	c.policyAssessed[id] = last
	return last, nil
}

// recordRecoveryDenied writes a policy-scope "recovery-unavailable" row
// unless the same denial is already the last assessment.
func (c *Controller) recordRecoveryDenied(ctx context.Context, inc *types.Incident, result string) error {
	c.policyAssessedMu.Lock()
	defer c.policyAssessedMu.Unlock()
	last, err := c.lastPolicyAssessment(ctx, inc.ID)
	if err != nil || last == result {
		return err
	}
	if err := c.appendAudit(ctx, inc, "system", "recovery-unavailable", result); err != nil {
		return err
	}
	c.policyAssessed[inc.ID] = result
	return nil
}

// recordRecoveryAvailable writes the policy-scope "recovery-available" row
// once the policy no longer holds a step it held before, so the panel and
// CLI stop showing the old denial.
func (c *Controller) recordRecoveryAvailable(ctx context.Context, inc *types.Incident) error {
	c.policyAssessedMu.Lock()
	defer c.policyAssessedMu.Unlock()
	last, err := c.lastPolicyAssessment(ctx, inc.ID)
	if err != nil || last == "" {
		return err
	}
	if err := c.appendAudit(ctx, inc, "system", "recovery-available", "the policy's recovery settings allow the step; step authorization still required"); err != nil {
		return err
	}
	c.policyAssessed[inc.ID] = ""
	return nil
}
