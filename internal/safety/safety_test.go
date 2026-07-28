package safety

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func target(node string) types.Target { return types.Target{Node: node} }

func TestGateConcurrencyLimit(t *testing.T) {
	g := NewGate(Limits{MaxConcurrentRemediations: 2, MaxConcurrentReboots: 1})

	if err := g.Allow(target("n1"), types.ActionGPUReset); err != nil {
		t.Fatalf("first action must be allowed: %v", err)
	}
	if err := g.Allow(target("n2"), types.ActionGPUReset); err != nil {
		t.Fatalf("second action must be allowed: %v", err)
	}
	if err := g.Allow(target("n3"), types.ActionGPUReset); err == nil {
		t.Fatal("third concurrent action must be denied")
	}
	g.Done(target("n1"), types.ActionGPUReset, 0)
	if err := g.Allow(target("n3"), types.ActionGPUReset); err != nil {
		t.Fatalf("slot freed, action must be allowed: %v", err)
	}
}

// Two incidents acting on the same target must not lose the target's slot
// when the first one finishes: the reservation is refcounted, and the
// concurrency limit only frees up after the last action on that target.
func TestGateSameTargetConcurrentActions(t *testing.T) {
	g := NewGate(Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1})

	if err := g.Allow(target("n1"), types.ActionGPUReset); err != nil {
		t.Fatalf("first action must be allowed: %v", err)
	}
	if err := g.Allow(target("n1"), types.ActionCollectBundle); err != nil {
		t.Fatalf("second action on the same target must be allowed: %v", err)
	}
	// First incident finishes; n1 still has one in-flight action, so another
	// target must remain denied by the limit of one.
	g.Done(target("n1"), types.ActionGPUReset, 0)
	if err := g.Allow(target("n2"), types.ActionGPUReset); err == nil {
		t.Fatal("n1 still active: a second target must be denied")
	}
	g.Done(target("n1"), types.ActionCollectBundle, 0)
	if err := g.Allow(target("n2"), types.ActionGPUReset); err != nil {
		t.Fatalf("all n1 actions done, slot must be free: %v", err)
	}
}

// A non-reboot Done on a target must not release the reboot slot an
// in-flight reboot-class action on the same target still holds.
func TestGateRebootSlotSurvivesNonRebootDone(t *testing.T) {
	g := NewGate(Limits{MaxConcurrentRemediations: 10, MaxConcurrentReboots: 1})

	if err := g.Allow(target("n1"), types.ActionReboot); err != nil {
		t.Fatalf("reboot must be allowed: %v", err)
	}
	if err := g.Allow(target("n1"), types.ActionCollectBundle); err != nil {
		t.Fatalf("bundle on the same target must be allowed: %v", err)
	}
	g.Done(target("n1"), types.ActionCollectBundle, 0)
	if err := g.Allow(target("n2"), types.ActionReboot); err == nil {
		t.Fatal("n1 reboot still in flight: a second reboot must be denied")
	}
	g.Done(target("n1"), types.ActionReboot, 0)
	if err := g.Allow(target("n2"), types.ActionReboot); err != nil {
		t.Fatalf("reboot finished, next reboot must be allowed: %v", err)
	}
}

// RecordCooldown must not release concurrency slots (the historical bug:
// advanceVerifying called Done to record a cooldown and freed a slot a
// concurrent incident still owned).
func TestGateRecordCooldownDoesNotReleaseSlots(t *testing.T) {
	g := NewGate(Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1})

	if err := g.Allow(target("n1"), types.ActionGPUReset); err != nil {
		t.Fatal(err)
	}
	g.RecordCooldown(target("n1"), types.ActionRunDiag, 30*time.Minute)
	if err := g.Allow(target("n2"), types.ActionGPUReset); err == nil {
		t.Fatal("RecordCooldown must not free n1's active slot")
	}
	if remaining := g.CooldownRemaining(target("n1"), types.ActionRunDiag); remaining <= 0 {
		t.Fatal("cooldown must be recorded")
	}
	// Zero and negative cooldowns record nothing.
	g.RecordCooldown(target("n1"), types.ActionGPUReset, 0)
	if remaining := g.CooldownRemaining(target("n1"), types.ActionGPUReset); remaining != 0 {
		t.Fatal("zero cooldown must not be recorded")
	}
}

// Done without a matching Allow (defensive path) must not underflow the
// refcount and corrupt later accounting.
func TestGateUnmatchedDoneIsHarmless(t *testing.T) {
	g := NewGate(Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1})

	g.Done(target("n1"), types.ActionGPUReset, 0)
	if err := g.Allow(target("n1"), types.ActionGPUReset); err != nil {
		t.Fatalf("gate must stay usable after unmatched Done: %v", err)
	}
	if err := g.Allow(target("n2"), types.ActionGPUReset); err == nil {
		t.Fatal("limit of one target must still be enforced")
	}
}

func TestGateRebootLimit(t *testing.T) {
	g := NewGate(Limits{MaxConcurrentRemediations: 10, MaxConcurrentReboots: 1})
	if err := g.Allow(target("n1"), types.ActionReboot); err != nil {
		t.Fatalf("first reboot must be allowed: %v", err)
	}
	if err := g.Allow(target("n2"), types.ActionReboot); err == nil {
		t.Fatal("second concurrent reboot must be denied")
	}
	if err := g.Allow(target("n2"), types.ActionGPUReset); err != nil {
		t.Fatalf("non-reboot action must still be allowed: %v", err)
	}
}

func TestGatePause(t *testing.T) {
	g := NewGate(Limits{MaxConcurrentRemediations: 10, MaxConcurrentReboots: 1})
	g.Pause()
	if err := g.Allow(target("n1"), types.ActionGPUReset); err == nil {
		t.Fatal("paused gate must deny everything")
	}
	g.Resume()
	if err := g.Allow(target("n1"), types.ActionGPUReset); err != nil {
		t.Fatalf("resumed gate must allow: %v", err)
	}
}

func TestGateCooldown(t *testing.T) {
	g := NewGate(Limits{MaxConcurrentRemediations: 10, MaxConcurrentReboots: 1})
	if err := g.Allow(target("n1"), types.ActionGPUReset); err != nil {
		t.Fatal(err)
	}
	g.Done(target("n1"), types.ActionGPUReset, 30*time.Minute)
	if err := g.Allow(target("n1"), types.ActionGPUReset); err == nil {
		t.Fatal("action within cooldown must be denied")
	}
	// A different action type on the same target is not blocked.
	if err := g.Allow(target("n1"), types.ActionCollectBundle); err != nil {
		t.Fatalf("different action must not share the cooldown: %v", err)
	}
}

func TestFlapDetector(t *testing.T) {
	f := NewFlapDetector(3, 24*time.Hour)
	tgt := target("n1")
	for cycle := 1; cycle <= 3; cycle++ {
		f.RecordResolved(tgt, types.ClassECCDBE)
		flapping := f.RecordReopen(tgt, types.ClassECCDBE)
		if cycle < 3 && flapping {
			t.Fatalf("cycle %d must not flag flapping yet", cycle)
		}
		if cycle == 3 && !flapping {
			t.Fatal("third resolve/reopen cycle within the window must flag flapping")
		}
	}
	// A different class on the same target counts separately.
	if f.RecordReopen(tgt, types.ClassNVLink) {
		t.Fatal("different class must have its own flap counter")
	}
}

// Brand-new incidents with no prior resolution are churn, not flapping, and
// a retried open transition (RecordReopen called twice for one incident)
// must not double-count.
func TestFlapDetectorCountsOnlyResolvedReopens(t *testing.T) {
	f := NewFlapDetector(2, 24*time.Hour)
	tgt := target("n1")

	for i := 0; i < 10; i++ {
		if f.RecordReopen(tgt, types.ClassECCDBE) {
			t.Fatal("reopens without any resolution must never flag flapping")
		}
	}

	f.RecordResolved(tgt, types.ClassECCDBE)
	if f.RecordReopen(tgt, types.ClassECCDBE) {
		t.Fatal("first counted cycle is below threshold 2")
	}
	// Retry of the same open transition: the resolution was consumed, so
	// this must not add a second count.
	if f.RecordReopen(tgt, types.ClassECCDBE) {
		t.Fatal("retried reopen without a new resolution must not count")
	}
	f.RecordResolved(tgt, types.ClassECCDBE)
	if !f.RecordReopen(tgt, types.ClassECCDBE) {
		t.Fatal("second resolve/reopen cycle must reach threshold 2")
	}
}

// Stale pairs are garbage-collected so the maps stay bounded.
func TestFlapDetectorGCsStaleKeys(t *testing.T) {
	f := NewFlapDetector(3, time.Hour)
	now := time.Now()
	f.now = func() time.Time { return now }

	f.RecordResolved(target("n1"), types.ClassECCDBE)
	f.RecordReopen(target("n1"), types.ClassECCDBE)
	f.RecordResolved(target("n2"), types.ClassNVLink)

	now = now.Add(2 * time.Hour)
	f.RecordResolved(target("n3"), types.ClassECCDBE)
	f.RecordReopen(target("n3"), types.ClassECCDBE)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reopens) != 1 {
		t.Fatalf("stale reopen keys not pruned: %d entries", len(f.reopens))
	}
	if len(f.pendingResolve) != 0 {
		t.Fatalf("stale pending resolutions not pruned: %d entries", len(f.pendingResolve))
	}
}

func TestFlapDetectorDisabled(t *testing.T) {
	f := NewFlapDetector(0, time.Hour)
	for i := 0; i < 10; i++ {
		if f.RecordReopen(target("n1"), types.ClassECCDBE) {
			t.Fatal("disabled detector must never flag")
		}
	}
}

func TestGateCooldownPruning(t *testing.T) {
	g := NewGate(Limits{MaxConcurrentRemediations: 10, MaxConcurrentReboots: 1})
	now := time.Now()
	g.now = func() time.Time { return now }

	if err := g.Allow(target("n1"), types.ActionGPUReset); err != nil {
		t.Fatal(err)
	}
	g.Done(target("n1"), types.ActionGPUReset, 30*time.Minute)
	if len(g.cooldownUntil) != 1 {
		t.Fatalf("cooldown entries = %d, want 1", len(g.cooldownUntil))
	}

	// After the cooldown elapses, the next Done prunes the stale entry so
	// the map cannot grow without bound.
	now = now.Add(time.Hour)
	if err := g.Allow(target("n2"), types.ActionGPUReset); err != nil {
		t.Fatal(err)
	}
	g.Done(target("n2"), types.ActionGPUReset, 0)
	if len(g.cooldownUntil) != 0 {
		t.Fatalf("expired cooldown entries not pruned: %d left", len(g.cooldownUntil))
	}
}

type memStateStore struct {
	mu    sync.Mutex
	kinds map[string][]byte
}

func (m *memStateStore) SaveSafetyState(kind string, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.kinds == nil {
		m.kinds = map[string][]byte{}
	}
	m.kinds[kind] = append([]byte(nil), payload...)
	return nil
}

func (m *memStateStore) LoadSafetyState(kind string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kinds[kind], nil
}

// A cooldown recorded before a controller restart must still deny the action
// after the restart; expired cooldowns are dropped during restore.
func TestGateCooldownSurvivesRestart(t *testing.T) {
	store := &memStateStore{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	g1 := NewGate(Limits{MaxConcurrentRemediations: 10, MaxConcurrentReboots: 1})
	if err := g1.RestoreAndPersist(store, log); err != nil {
		t.Fatal(err)
	}
	g1.RecordCooldown(target("n1"), types.ActionGPUReset, 30*time.Minute)
	g1.RecordCooldown(target("n2"), types.ActionReboot, -time.Minute) // already expired

	// "Restart": a fresh gate restoring from the same store.
	g2 := NewGate(Limits{MaxConcurrentRemediations: 10, MaxConcurrentReboots: 1})
	if err := g2.RestoreAndPersist(store, log); err != nil {
		t.Fatal(err)
	}
	if err := g2.Allow(target("n1"), types.ActionGPUReset); err == nil {
		t.Fatal("cooldown must survive the restart")
	}
	if err := g2.Allow(target("n2"), types.ActionReboot); err != nil {
		t.Fatalf("expired cooldown must not be restored: %v", err)
	}
	// In-flight slots are deliberately not persisted: EXECUTING incidents are
	// re-driven through Allow after recovery.
	if err := g2.Allow(target("n3"), types.ActionGPUReset); err != nil {
		t.Fatalf("fresh gate must accept new work: %v", err)
	}
}

// Flap cycles counted before a restart must keep counting after it.
func TestFlapHistorySurvivesRestart(t *testing.T) {
	store := &memStateStore{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	f1 := NewFlapDetector(2, 24*time.Hour)
	if err := f1.RestoreAndPersist(store, log); err != nil {
		t.Fatal(err)
	}
	f1.RecordResolved(target("n1"), types.ClassECCDBE)
	if f1.RecordReopen(target("n1"), types.ClassECCDBE) {
		t.Fatal("one cycle is below threshold 2")
	}
	f1.RecordResolved(target("n1"), types.ClassECCDBE)

	f2 := NewFlapDetector(2, 24*time.Hour)
	if err := f2.RestoreAndPersist(store, log); err != nil {
		t.Fatal(err)
	}
	// The pending resolution and the first counted cycle both survived, so
	// this reopen is the second cycle and must quarantine.
	if !f2.RecordReopen(target("n1"), types.ClassECCDBE) {
		t.Fatal("flap history must survive the restart and reach threshold 2")
	}
}
