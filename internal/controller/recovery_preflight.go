// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
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

// skipRecycle moves past a recycle step that is not applicable: to the next
// step of the playbook, or to its escalation playbook. A skip is neither a
// failure nor an escalation (no escalation metric, no recovery attempt
// spent), and alone it never leads to VERIFYING: nothing was done to recover
// the node.
func (c *Controller) skipRecycle(ctx context.Context, inc *types.Incident, step *playbook.Step, reason string) error {
	if inc.State != types.StateEvaluating {
		// A parked step keeps its approval round; it is held until the round
		// ends. Moving past it from AWAITING_APPROVAL is not a legal edge.
		c.deferStep(inc, step, metrics.DeferRecoveryUnavailable)
		c.logOnceStepUnavailable(inc, step, "recovery unavailable: "+reason)
		return nil
	}
	engine := c.runtimeConfig(ctx).Engine
	book, ok := engine.Playbook(inc.Playbook)
	if ok && inc.StepIndex+1 < len(book.Steps) {
		inc.StepIndex++
		return c.transition(ctx, inc, types.StateEvaluating, "system", "recycle-skipped",
			fmt.Sprintf("recycle skipped: %s; continuing with step %s of %s", reason, book.Steps[inc.StepIndex].Name, inc.Playbook), nil)
	}
	next, ok := engine.Escalation(inc.Playbook)
	if !ok {
		c.deferStep(inc, step, metrics.DeferRecoveryUnavailable)
		c.logOnceStepUnavailable(inc, step, "recovery unavailable: "+reason+"; the playbook has no step or escalation after it")
		return nil
	}
previous := inc.Playbook
	inc.Playbook = next.Name
	inc.StepIndex = 0
	// Attempt is the action-ID nonce (see actionID): bumped so the new
	// rung's steps get fresh IDs and do not replay the previous rung's
	// results. It is not a failed attempt: escalate discounts skips from the
	// cap, and skipping cannot loop, because escalation chains are acyclic
	// (rejected at load) and each skip moves one playbook further along.
	inc.Attempt++
	return c.transition(ctx, inc, types.StateEvaluating, "system", "recycle-skipped",
		fmt.Sprintf("recycle skipped: %s; continuing with %s (from %s)", reason, next.Name, previous), map[string]string{"rung": "escalation"})
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

// recoveryAttempts is inc.Attempt less the bumps made by skipped recycles,
// which are not recovery attempts. It reads the trail, so callers use it only
// once Attempt reaches the cap.
func (c *Controller) recoveryAttempts(ctx context.Context, inc *types.Incident) (int, error) {
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil {
		return 0, err
	}
	attempts := inc.Attempt
	for _, e := range trail {
		if e.Action == "recycle-skipped" && e.Params["rung"] == "escalation" {
			attempts--
		}
	}
	return attempts, nil
}
