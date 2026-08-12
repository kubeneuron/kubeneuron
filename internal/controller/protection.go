package controller

import (
	"context"

	"github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// This file is the protection seam: the counting of what the control plane
// chose NOT to do. Every refusal, hold, and early escalation elsewhere in the
// controller routes its accounting through here, so the deferral label set has
// one owner and a new guard cannot quietly ship uncounted.

// deferStep records that a destructive step did not run because of one named
// guard, when the step is known.
//
// Two filters keep the series honest rather than merely large. A dry-run
// incident executes nothing, so holding it protected nothing. And a step that
// disrupts nothing — a notify rung, a verify rung, an uncordon — was never a
// threat to a workload; counting a paused node "protecting" a Slack message
// would turn the protection story into noise. Both exclusions are deliberate
// and are why the metric can be read as capacity actually spared.
func (c *Controller) deferStep(inc *types.Incident, step *playbook.Step, reason string) {
	if inc.DryRun || !stepDisrupts(step) {
		return
	}
	metrics.DestructiveStepsDeferred.WithLabelValues(reason).Inc()
}

// deferPlaybook records a deferral taken BEFORE the next step is resolved —
// the pause, window, and cooldown holds at the top of the evaluating walk. The
// disruption question is then asked of the whole bound playbook: if no rung of
// it can touch a workload, holding it protected nothing.
func (c *Controller) deferPlaybook(ctx context.Context, inc *types.Incident, reason string) {
	if inc.DryRun {
		return
	}
	book, ok := c.runtimeConfig(ctx).Engine.Playbook(inc.Playbook)
	if !ok || !playbookDisrupts(book) {
		return
	}
	metrics.DestructiveStepsDeferred.WithLabelValues(reason).Inc()
}

// deferForGateDenial translates a safety-gate refusal into its protection
// label. The classification is the gate's own (safety.DenialKind), never a
// parse of its human-facing reason string. An error that is not a *Denial
// cannot happen through Allow/AllowHeld, but if one ever did it must not be
// silently miscounted as a concurrency cap — it counts as nothing.
func (c *Controller) deferForGateDenial(inc *types.Incident, step *playbook.Step, err error) {
	kind, ok := safety.DenialKindOf(err)
	if !ok {
		return
	}
	switch kind {
	case safety.DenialGlobalPause:
		c.deferStep(inc, step, metrics.DeferGlobalPause)
	case safety.DenialCooldown:
		// The gate's cooldown map is keyed by (target, action) and is the same
		// map a completed playbook's cooldown lands in, so both wear the
		// playbook_cooldown label rather than inventing a near-duplicate.
		c.deferStep(inc, step, metrics.DeferPlaybookCooldown)
	case safety.DenialConcurrency:
		c.deferStep(inc, step, metrics.DeferConcurrencyCap)
	}
}

// stepDisrupts reports whether a step can actually take a workload away. It
// spans both destructive axes on purpose: the controller-path blast-radius
// flag (cordon, drain, evict, recycle/replace) and the agent-path one (reboot,
// gpu_reset, driver reload), because a workload does not care which executor
// ended it. A registry fact, so a new action is covered by its own row.
func stepDisrupts(step *playbook.Step) bool {
	if step == nil {
		return false
	}
	def, ok := action.ByWire(step.Action)
	return ok && (def.Destructive || def.AgentDestructive)
}

// playbookDisrupts reports whether ANY rung of the playbook can disrupt a
// workload — the playbook-scope companion of stepDisrupts, used where a
// deferral happens before the next step is known.
func playbookDisrupts(book *playbook.Playbook) bool {
	if book == nil {
		return false
	}
	for i := range book.Steps {
		if stepDisrupts(&book.Steps[i]) {
			return true
		}
	}
	return false
}

// stepIsIdleGuard reports whether the step is an idle guard, whose failure
// means the device was busy and the rung it protects did not run.
func stepIsIdleGuard(step *playbook.Step) bool {
	if step == nil {
		return false
	}
	def, ok := action.ByWire(step.Action)
	return ok && def.IdleGuard
}

// idleGuardStopped reports whether a failed step was an idle guard at all —
// the CONTROL decision, deliberately separate from the metric below.
//
// Any failure of a guard means the guard did not clear the device. Busy,
// missing nvidia-smi, wedged driver, timeout, a transport error with no result
// at all: in every one of them we do NOT know the device is idle, and the rung
// the guard stands in front of is by construction more destructive than the
// guard. So the ladder stops, always.
//
// Basing this on the refusal CODE instead was a fail-open across the ordinary
// upgrade window: docs/upgrade.md mandates controller-first, so every
// not-yet-upgraded agent sends no code, and a new controller read that absence
// as "not a refusal" and escalated — reaching for a bigger hammer on a device
// a guard had just failed to clear. Absence of evidence must not choose the
// destructive branch.
func idleGuardStopped(step *playbook.Step) bool {
	return stepIsIdleGuard(step)
}

// idleGuardRefused reports whether a failed idle-guard step failed because the
// device was BUSY, as opposed to because the probe could not run.
//
// The distinction is the whole value of the metric. nvidia-smi missing, a
// wedged driver, and a timed-out probe all fail the same step and all fail
// closed, but none of them is evidence that a workload was spared. The agent
// says which happened in ActionResult.Refusal; the prose in Error is never
// parsed, and an absent field (an agent older than the field, a platform step,
// a transport error with no result at all) counts as no evidence rather than
// as a refusal. Under-counting protection is the honest direction: this metric
// is a claim about value delivered.
func idleGuardRefused(step *playbook.Step, result *types.ActionResult) bool {
	return stepIsIdleGuard(step) && result != nil && result.Refusal == types.RefusalNotIdle
}

// recordIdleRefusal counts an idle guard that refused because the device was
// still working. Charging that to "steps failed" alone loses the fact that the
// failure WAS the protection.
func (c *Controller) recordIdleRefusal(inc *types.Incident, step *playbook.Step, result *types.ActionResult) {
	if inc.DryRun || !idleGuardRefused(step, result) {
		return
	}
	metrics.DestructiveStepsDeferred.WithLabelValues(metrics.DeferNotIdle).Inc()
}
