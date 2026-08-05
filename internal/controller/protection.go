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

// recordIdleRefusal counts an idle guard that refused. It is called on the
// step-failure path because that is what an idle guard's refusal looks like
// from the executor: agent.idle_check returns an error, the ladder escalates,
// and the reset the guard stood in front of never happens. Charging that to
// "steps failed" alone loses the fact that the failure WAS the protection.
func (c *Controller) recordIdleRefusal(inc *types.Incident, step *playbook.Step) {
	if inc.DryRun || !stepIsIdleGuard(step) {
		return
	}
	metrics.DestructiveStepsDeferred.WithLabelValues(metrics.DeferNotIdle).Inc()
}
