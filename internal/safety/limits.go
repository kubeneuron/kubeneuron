// Package safety enforces the cluster-wide guard rails every remediation
// must pass before executing: concurrency limits, per-target cooldowns, flap
// detection, and the global pause switch. All execution paths in the
// controller go through Gate.Allow — never around it.
package safety

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	actionreg "github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Limits are the operator-configured safety limits (configs/policies.yaml).
type Limits struct {
	// MaxConcurrentRemediations caps how many targets may be mid-remediation
	// at once. A target counts from its first admitted step until its incident
	// reaches a terminal state — not merely while a step is executing, or N
	// incidents could interleave their steps and far exceed the cap.
	MaxConcurrentRemediations int
	// MaxConcurrentReboots caps reboot-class actions specifically.
	MaxConcurrentReboots int
	// DryRun makes every action a logged no-op.
	DryRun bool
}

// Denial explains why an action was not allowed. It is surfaced in metrics,
// logs, and the incident audit trail.
type Denial struct {
	Reason string
}

func (d *Denial) Error() string { return "safety: " + d.Reason }

// Gate is the single admission point for remediation actions.
type Gate struct {
	mu     sync.Mutex
	limits Limits
	paused bool
	// active and reboots are refcounts per target key (node[/gpu-uuid]).
	// Several concurrent incidents may act on the same target; the target
	// counts once against the limit, and its slot is only released when the
	// last in-flight action for it finishes.
	active  map[string]int
	reboots map[string]int // subset of active running reboot-class actions
	// cooldownUntil[key] = time before which the (target, action) pair may
	// not run again. Key: target|action.
	cooldownUntil map[string]time.Time
	now           func() time.Time
	// store, when set via RestoreAndPersist, receives a write-through
	// snapshot of cooldownUntil after every change so restarts keep them.
	store    StateStore
	storeLog *slog.Logger
}

// NewGate builds a Gate with the given limits.
func NewGate(limits Limits) *Gate {
	return &Gate{
		limits:        limits,
		active:        map[string]int{},
		reboots:       map[string]int{},
		cooldownUntil: map[string]time.Time{},
		now:           time.Now,
	}
}

// Pause freezes all automated execution (the "big red button").
func (g *Gate) Pause() { g.mu.Lock(); g.paused = true; g.mu.Unlock() }

// Resume re-enables automated execution.
func (g *Gate) Resume() { g.mu.Lock(); g.paused = false; g.mu.Unlock() }

// Paused reports the pause state.
func (g *Gate) Paused() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.paused }

// DryRun reports whether the gate is in dry-run mode.
func (g *Gate) DryRun() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.limits.DryRun }

// SetDryRun toggles dry-run mode at runtime (admin API).
func (g *Gate) SetDryRun(v bool) { g.mu.Lock(); g.limits.DryRun = v; g.mu.Unlock() }

// Allow admits the FIRST step of a remediation on a target, reserving the
// target's remediation slot and the step's reboot-class slot. The remediation
// slot is held until ReleaseRemediation (the incident terminalized); the step's
// reboot slot until StepDone. Subsequent steps of the same remediation are
// admitted with AllowHeld. A non-nil error is always *Denial.
func (g *Gate) Allow(target types.Target, action types.ActionType) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	key := targetKey(target)
	if reason := g.stepDenialLocked(key, action); reason != "" {
		return &Denial{Reason: reason}
	}
	if g.active[key] == 0 && len(g.active) >= g.limits.MaxConcurrentRemediations {
		return &Denial{Reason: fmt.Sprintf("concurrency limit reached (%d targets in remediation)", len(g.active))}
	}

	g.active[key]++
	if isRebootClass(action) {
		g.reboots[key]++
	}
	return nil
}

// AllowHeld admits one more step for a target that already holds a remediation
// slot: it applies the per-step checks (pause, cooldown, the reboot cap) but
// not the remediation cap, and reserves only the step's reboot-class slot.
// Without this split, releasing and re-acquiring the target slot between steps
// let MaxConcurrentRemediations cap concurrent steps instead of concurrent
// remediations. A non-nil error is always *Denial.
func (g *Gate) AllowHeld(target types.Target, action types.ActionType) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if reason := g.stepDenialLocked(targetKey(target), action); reason != "" {
		return &Denial{Reason: reason}
	}
	if isRebootClass(action) {
		g.reboots[targetKey(target)]++
	}
	return nil
}

// stepDenialLocked evaluates the per-step admission checks shared by Allow and
// AllowHeld. Called with g.mu held; returns a denial reason or "".
func (g *Gate) stepDenialLocked(key string, action types.ActionType) string {
	if g.paused {
		return "system is paused"
	}
	if until, ok := g.cooldownUntil[key+"|"+string(action)]; ok && g.now().Before(until) {
		return fmt.Sprintf("cooldown until %s for %s on %s", until.Format(time.RFC3339), action, key)
	}
	if isRebootClass(action) && g.reboots[key] == 0 && len(g.reboots) >= g.limits.MaxConcurrentReboots {
		return fmt.Sprintf("reboot concurrency limit reached (%d in progress)", len(g.reboots))
	}
	return ""
}

// OccupyRemediation unconditionally reserves a target's remediation slot,
// bypassing every admission check (pause, cooldown, limits). It exists for one
// caller: rebuilding the gate's occupancy from durable state on leadership
// acquisition, so MaxConcurrentRemediations stays an invariant across a leader
// failover. A new leader that started with empty slots would admit
// remediations past the cap while agents were still running leased destructive
// actions the previous leader had admitted. Each occupied slot is released by
// ReleaseRemediation when its incident terminalizes, exactly like a slot
// reserved by Allow.
func (g *Gate) OccupyRemediation(target types.Target) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active[targetKey(target)]++
}

// OccupyStep unconditionally reserves the reboot-class slot for a recovered
// in-flight step (rebuild-on-leadership only; a no-op for non-reboot actions).
// Released by StepDone when the recovered incident leaves EXECUTING.
func (g *Gate) OccupyStep(target types.Target, action types.ActionType) {
	if !isRebootClass(action) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reboots[targetKey(target)]++
}

// StepDone releases the per-step reservation of a matching Allow/AllowHeld —
// the reboot-class slot — and records the action's cooldown. The target's
// remediation slot stays held until ReleaseRemediation: releasing it here,
// between steps, is what reduced MaxConcurrentRemediations to a per-step cap.
func (g *Gate) StepDone(target types.Target, action types.ActionType, cooldown time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := targetKey(target)
	if isRebootClass(action) && g.reboots[key] > 0 {
		if g.reboots[key]--; g.reboots[key] == 0 {
			delete(g.reboots, key)
		}
	}
	if cooldown > 0 {
		g.cooldownUntil[key+"|"+string(action)] = g.now().Add(cooldown)
	}
	g.pruneCooldownsLocked()
	if cooldown > 0 {
		g.persistLocked()
	}
}

// Done fully releases one admitted action: its per-step reservation and its
// target's remediation slot, in one call. It is StepDone followed by
// ReleaseRemediation, for callers whose remediation is a single admitted
// action. The controller's step lifecycle keeps the two apart: StepDone at
// step end, ReleaseRemediation when the incident terminalizes.
func (g *Gate) Done(target types.Target, action types.ActionType, cooldown time.Duration) {
	g.StepDone(target, action, cooldown)
	g.ReleaseRemediation(target)
}

// ReleaseRemediation releases the target's remediation slot reserved by Allow
// (or OccupyRemediation). Slots are refcounted: the target frees its slot only
// when every incident remediating it has released, so one incident finishing
// cannot release a slot another incident on the same target still holds.
func (g *Gate) ReleaseRemediation(target types.Target) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := targetKey(target)
	if g.active[key] > 0 {
		if g.active[key]--; g.active[key] == 0 {
			delete(g.active, key)
		}
	}
}

// RecordCooldown notes a cooldown for the (target, action) pair without
// touching concurrency slots. Use it when the cooldown is decided at a point
// that holds no reservation (for example when an incident resolves after its
// steps already released their slots).
func (g *Gate) RecordCooldown(target types.Target, action types.ActionType, cooldown time.Duration) {
	if cooldown <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cooldownUntil[targetKey(target)+"|"+string(action)] = g.now().Add(cooldown)
	g.pruneCooldownsLocked()
	g.persistLocked()
}

// CooldownRemaining reports how long the (target, action) pair remains in
// cooldown; zero means it may run now.
func (g *Gate) CooldownRemaining(target types.Target, action types.ActionType) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.cooldownUntil[targetKey(target)+"|"+string(action)]
	if !ok {
		return 0
	}
	if remaining := until.Sub(g.now()); remaining > 0 {
		return remaining
	}
	return 0
}

// pruneCooldownsLocked drops expired cooldown entries so the map does not
// grow without bound over months of operation. Called with g.mu held.
func (g *Gate) pruneCooldownsLocked() {
	now := g.now()
	for key, until := range g.cooldownUntil {
		if !now.Before(until) {
			delete(g.cooldownUntil, key)
		}
	}
}

// isRebootClass delegates to the action registry so the reboot-class set lives
// in one place with the rest of each action's facts.
func isRebootClass(a types.ActionType) bool {
	return actionreg.IsRebootClass(a)
}

func targetKey(t types.Target) string {
	if t.IsGPU() {
		return t.Node + "/" + t.GPUUUID
	}
	return t.Node
}
