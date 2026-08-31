// Package safety enforces the cluster-wide guard rails every remediation
// must pass before executing: concurrency limits, per-target cooldowns, flap
// detection, and the global pause switch. All execution paths in the
// controller go through Gate.Allow — never around it.
package safety

import (
	"context"
	"errors"
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

// DenialKind classifies a refusal into the lever that caused it. It exists so
// a caller can label a metric without parsing Reason, which is human-facing
// prose that changes whenever the message improves.
type DenialKind string

const (
	// DenialGlobalPause: the fleet-wide pause switch is down.
	DenialGlobalPause DenialKind = "global_pause"
	// DenialCooldown: this (target, action) pair ran recently.
	DenialCooldown DenialKind = "cooldown"
	// DenialConcurrency: a remediation or reboot cap is full.
	DenialConcurrency DenialKind = "concurrency"
)

// Denial explains why an action was not allowed. It is surfaced in metrics,
// logs, and the incident audit trail.
type Denial struct {
	// Kind is the machine-readable lever; Reason is what a human reads.
	Kind   DenialKind
	Reason string
}

func (d *Denial) Error() string { return "safety: " + d.Reason }

// DenialKindOf reports the lever behind an admission error, and whether the
// error was a *Denial at all. Callers use it instead of a type switch so the
// classification stays in one place.
func DenialKindOf(err error) (DenialKind, bool) {
	var denial *Denial
	if errors.As(err, &denial) {
		return denial.Kind, true
	}
	return "", false
}

// Gate is the single admission point for remediation actions.
type Gate struct {
	mu     sync.Mutex
	limits Limits
	paused bool
	// pauseActor and pauseChangedAt make the durable red-button state
	// inspectable when a new leader restores it.  They are not used as an
	// authorization decision; identity is owned by the API boundary.
	pauseActor     string
	pauseChangedAt time.Time
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

// Pause freezes all automated execution (the "big red button").  It remains
// available to tests and early startup, where no persistent store has been
// attached yet.  The operator API must use SetPaused, which refuses to claim a
// durable pause when its write fails.
func (g *Gate) Pause() { g.setPausedBestEffort(true, "") }

// Resume re-enables automated execution. See Pause for why the operator API
// uses SetPaused instead.
func (g *Gate) Resume() { g.setPausedBestEffort(false, "") }

// Paused reports the pause state.
func (g *Gate) Paused() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.paused }

// DryRun reports whether the gate is in dry-run mode.
func (g *Gate) DryRun() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.limits.DryRun }

// Limits returns a point-in-time copy of the gate's accounting limits.  The
// controller seeds each immutable runtime snapshot from it; callers must not
// retain the result as a live view.
func (g *Gate) Limits() Limits { g.mu.Lock(); defer g.mu.Unlock(); return g.limits }

// SetPaused changes the global pause only after its replacement state was
// durably written.  A red button that acknowledges success in one leader's
// memory but disappears on failover is worse than a visible 503: it invites an
// operator to believe remediation stopped while a replacement process resumes
// it.  Store attachment happens in RestoreAndPersist before the controller API
// is exposed.
func (g *Gate) SetPaused(ctx context.Context, paused bool, actor string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.store == nil {
		return errors.New("global pause persistence is unavailable")
	}
	changedAt := g.now().UTC()
	persistCtx, cancel := boundedStateContext(ctx)
	defer cancel()
	if err := g.persistPauseLocked(persistCtx, paused, actor, changedAt); err != nil {
		return err
	}
	g.paused, g.pauseActor, g.pauseChangedAt = paused, actor, changedAt
	return nil
}

func (g *Gate) setPausedBestEffort(paused bool, actor string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	changedAt := g.now().UTC()
	if g.store != nil {
		persistCtx, cancel := boundedStateContext(context.Background())
		err := g.persistPauseLocked(persistCtx, paused, actor, changedAt)
		cancel()
		if err != nil && g.storeLog != nil {
			g.storeLog.Warn("persisting global pause failed; state survives in memory only", "err", err)
		}
	}
	g.paused, g.pauseActor, g.pauseChangedAt = paused, actor, changedAt
}

// SetDryRun toggles dry-run mode at runtime (admin API).
func (g *Gate) SetDryRun(v bool) { g.mu.Lock(); g.limits.DryRun = v; g.mu.Unlock() }

// ApplyLimits installs a new limit set on a RUNNING gate, leaving every piece
// of live state — held slots, cooldowns, pause — exactly where it is.
//
// It exists because the controller reloads its configuration in place rather
// than rolling its Deployment (the operator keeps the config-digest off the
// pod template on purpose: under leader election a rollout deadlocks). Until
// this, the reload re-installed playbooks, profiles and the confinement
// selector but never these, so `spec.safety.executionMode` did nothing at all
// to a running controller — an installation switched to Enabled executed
// nothing.
//
// It does NOT by itself stop an incident that is already open. The dry-run
// decision was stamped on each incident when it opened, so making the gate
// answer differently changes nothing for work already in flight; the
// controller has to consult this gate at execution time as well, which is what
// Controller.effectiveDryRun does. Both halves are needed, and this comment
// claimed the whole fix for a while when it was only one of them.
//
// Lowering a concurrency cap below what is currently held is allowed and does
// not evict anything: the in-flight work finishes and the new cap binds the
// next admission. Preempting a running remediation to satisfy a config change
// would be a more violent act than the change asked for.
func (g *Gate) ApplyLimits(limits Limits) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limits = limits
}

// Allow admits the FIRST step of a remediation on a target, reserving the
// target's remediation slot and the step's reboot-class slot. The remediation
// slot is held until ReleaseRemediation (the incident terminalized); the step's
// reboot slot until StepDone. Subsequent steps of the same remediation are
// admitted with AllowHeld. A non-nil error is always *Denial.
func (g *Gate) Allow(target types.Target, action types.ActionType) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	key := targetKey(target)
	if kind, reason := g.stepDenialLocked(key, action); reason != "" {
		return &Denial{Kind: kind, Reason: reason}
	}
	if g.active[key] == 0 && len(g.active) >= g.limits.MaxConcurrentRemediations {
		return &Denial{Kind: DenialConcurrency,
			Reason: fmt.Sprintf("concurrency limit reached (%d targets in remediation)", len(g.active))}
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

	if kind, reason := g.stepDenialLocked(targetKey(target), action); reason != "" {
		return &Denial{Kind: kind, Reason: reason}
	}
	if isRebootClass(action) {
		g.reboots[targetKey(target)]++
	}
	return nil
}

// stepDenialLocked evaluates the per-step admission checks shared by Allow and
// AllowHeld. Called with g.mu held; returns the classified lever and a denial
// reason, or an empty reason when the step is admitted.
func (g *Gate) stepDenialLocked(key string, action types.ActionType) (DenialKind, string) {
	if g.paused {
		return DenialGlobalPause, "system is paused"
	}
	if until, ok := g.cooldownUntil[key+"|"+string(action)]; ok && g.now().Before(until) {
		return DenialCooldown, fmt.Sprintf("cooldown until %s for %s on %s", until.Format(time.RFC3339), action, key)
	}
	if isRebootClass(action) && g.reboots[key] == 0 && len(g.reboots) >= g.limits.MaxConcurrentReboots {
		return DenialConcurrency, fmt.Sprintf("reboot concurrency limit reached (%d in progress)", len(g.reboots))
	}
	return "", ""
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
		persistCtx, cancel := boundedStateContext(context.Background())
		g.persistLocked(persistCtx)
		cancel()
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
	persistCtx, cancel := boundedStateContext(context.Background())
	g.persistLocked(persistCtx)
	cancel()
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

// targetKey is the identity the gate counts, refcounts and cools down by. Two
// incidents about the SAME target deliberately share one slot — the cap is a
// cap on targets in remediation, not on incidents — so this function must not
// map two DIFFERENT devices onto one key.
//
// It used to, and the day two unattributed GPUs on one node stopped collapsing
// into a single incident, that started to matter. With no UUID the key was the
// bare node, so a PCIe switch failure — eight cards falling off one node's bus,
// the classic correlated fault — produced eight incidents that all shared one
// key. Each was then admitted by the refcount as though it were another action
// on the same device, and MaxConcurrentRemediations, which the operator sets
// precisely to bound this, never fired. A correlated multi-device failure is
// exactly when a blast-radius cap most needs to hold.
//
// The bus address is the device identity available before a UUID is, which is
// the whole reason it is on the Target. Using it here also makes the release
// after a promotion precise: releasing the old key used to decrement a count
// siblings were sharing.
//
// A target with neither a UUID nor an address is node-scoped, and still keys to
// the node alone — unchanged.
func targetKey(t types.Target) string {
	if t.PCIAddr != "" {
		// A later signal can promote this target from PCI-only to a UUID without
		// changing the physical device. Keep the gate under that immutable
		// identity, rather than moving a reservation after a transaction commits.
		return t.Node + "/pci:" + types.NormalizePCIAddress(t.PCIAddr)
	}
	if t.IsGPU() {
		return t.Node + "/" + t.GPUUUID
	}
	return t.Node
}
