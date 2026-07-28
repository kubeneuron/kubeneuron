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

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Limits are the operator-configured safety limits (configs/policies.yaml).
type Limits struct {
	// MaxConcurrentRemediations caps how many targets may be in
	// EXECUTING/VERIFYING at once.
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

// Allow admits an action on a target, reserving a concurrency slot. Callers
// MUST call Done when the action (and its verification) finishes. A non-nil
// error is always *Denial.
func (g *Gate) Allow(target types.Target, action types.ActionType) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.paused {
		return &Denial{Reason: "system is paused"}
	}
	key := targetKey(target)
	if until, ok := g.cooldownUntil[key+"|"+string(action)]; ok && g.now().Before(until) {
		return &Denial{Reason: fmt.Sprintf("cooldown until %s for %s on %s", until.Format(time.RFC3339), action, key)}
	}
	if g.active[key] == 0 && len(g.active) >= g.limits.MaxConcurrentRemediations {
		return &Denial{Reason: fmt.Sprintf("concurrency limit reached (%d targets in remediation)", len(g.active))}
	}
	if isRebootClass(action) && g.reboots[key] == 0 && len(g.reboots) >= g.limits.MaxConcurrentReboots {
		return &Denial{Reason: fmt.Sprintf("reboot concurrency limit reached (%d in progress)", len(g.reboots))}
	}

	g.active[key]++
	if isRebootClass(action) {
		g.reboots[key]++
	}
	return nil
}

// Done releases the concurrency slot reserved by a matching Allow and records
// the action's cooldown. Slots are refcounted: the target frees its
// concurrency (and reboot) slot only when every admitted action on it has
// called Done, so one incident finishing cannot release a slot another
// incident on the same target still holds.
func (g *Gate) Done(target types.Target, action types.ActionType, cooldown time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := targetKey(target)
	if g.active[key] > 0 {
		if g.active[key]--; g.active[key] == 0 {
			delete(g.active, key)
		}
	}
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

func isRebootClass(a types.ActionType) bool {
	return a == types.ActionReboot || a == types.ActionPowerCycle || a == types.ActionDriverReinstall
}

func targetKey(t types.Target) string {
	if t.IsGPU() {
		return t.Node + "/" + t.GPUUUID
	}
	return t.Node
}
