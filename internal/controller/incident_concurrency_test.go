package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// A step runs in its own goroutine off a snapshot captured at ListIncidents.
// While it ran, a signal arrived and a concurrent ingest bumped SignalSeen. The
// step's post-step transition must not write its stale snapshot back and erase
// that count — it re-reads the row and merges the ingest-owned field.
func TestTransitionPreservesConcurrentSignalBump(t *testing.T) {
	c, st, _ := walkFixture(t, false)
	ctx := context.Background()

	now := time.Now()
	inc := &types.Incident{
		ID:     "inc-race",
		Target: types.Target{Node: "node-a", GPUUUID: "GPU-1"},
		Class:  types.ClassECCDBE, State: types.StateExecuting, Playbook: "drain-and-reset",
		StepIndex: 1, SignalSeen: 5,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// The step goroutine holds this snapshot; runStep increments StepIndex
	// locally after a successful step, before the transition.
	stale := inc.Clone()
	stale.StepIndex = 2

	// Meanwhile a concurrent ingest does a fresh read and bumps SignalSeen.
	fresh, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh.SignalSeen++ // 5 -> 6
	fresh.UpdatedAt = time.Now()
	if err := st.UpdateIncident(ctx, fresh); err != nil {
		t.Fatal(err)
	}

	// The post-step transition, built from the stale snapshot.
	if err := c.transition(ctx, stale, types.StateVerifying, "system", "reset", "done", nil); err != nil {
		t.Fatalf("transition = %v", err)
	}

	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SignalSeen != 6 {
		t.Fatalf("SignalSeen = %d, want 6 (the concurrent ingest must not be lost)", got.SignalSeen)
	}
	if got.State != types.StateVerifying {
		t.Fatalf("state = %s, want VERIFYING", got.State)
	}
	if got.StepIndex != 2 {
		t.Fatalf("StepIndex = %d, want 2 (step progress must not regress)", got.StepIndex)
	}
}

// When the incident's state has moved out from under a stale snapshot, the
// transition must fail closed with a conflict rather than overwrite the newer
// state — the caller retries on the next pass.
func TestTransitionConflictsWhenStateMovedUnderneath(t *testing.T) {
	c, st, _ := walkFixture(t, false)
	ctx := context.Background()

	now := time.Now()
	inc := &types.Incident{
		ID:     "inc-conflict",
		Target: types.Target{Node: "node-a", GPUUUID: "GPU-2"},
		Class:  types.ClassECCDBE, State: types.StateExecuting,
		StepIndex: 1, OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	stale := inc.Clone() // snapshot: still EXECUTING

	// A concurrent path quarantines the incident to NEEDS_HUMAN.
	fresh, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.transition(ctx, fresh, types.StateNeedsHuman, "system", "quarantine", "boom", nil); err != nil {
		t.Fatal(err)
	}

	// The stale EXECUTING->VERIFYING transition must be rejected, not applied.
	err = c.transition(ctx, stale, types.StateVerifying, "system", "reset", "done", nil)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale transition err = %v, want ErrConflict", err)
	}
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateNeedsHuman {
		t.Fatalf("state = %s, want NEEDS_HUMAN preserved", got.State)
	}
}
