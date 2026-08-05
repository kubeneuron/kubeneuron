package controller

// Round-7 item A: remediation-slot ownership is the durable
// Incident.RemediationSlotHeld bit, not an in-memory map with a lossy
// failover inference. These tests hold the cases the inference got wrong.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The exact case the old inference (EXECUTING||VERIFYING||StepIndex>0)
// missed: escalate() resets StepIndex to 0 and returns the incident to
// EVALUATING while it still holds its slot. A new leader must rebuild that
// occupancy from the durable bit, or the cap undercounts across failover.
func TestFailoverPreservesTheSlotOfAnEscalatedIncident(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	// What the old leader durably left behind after escalate(): EVALUATING,
	// StepIndex 0, Attempt bumped, slot bit still set.
	now := time.Now()
	inc := &types.Incident{
		ID: "inc-escalated", Target: types.Target{Node: "node-a"},
		Class: types.ClassFellOffBus, State: types.StateEvaluating,
		Playbook: "rung-2", StepIndex: 0, Attempt: 1,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
		RemediationSlotHeld: true,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1})
	c := New(st, st, nil, gate, nil, nil, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.RebuildGateOccupancy(ctx); err != nil {
		t.Fatal(err)
	}

	if err := gate.Allow(types.Target{Node: "node-b"}, types.ActionGPUReset); err == nil {
		t.Fatal("the escalated incident's slot was dropped across failover: the cap admitted a second remediation")
	}
	// The escalated incident's own next rung is admitted as a held target.
	if err := gate.AllowHeld(inc.Target, types.ActionGPUReset); err != nil {
		t.Fatalf("the escalated incident's own next step must be admitted: %v", err)
	}
}

// Halting clears the bit in the same transaction and releases the gate slot
// exactly once; the durable row is the authority a restart rebuilds from.
func TestHaltingTransitionClearsTheDurableBitAndFreesTheSlot(t *testing.T) {
	book := &playbook.Playbook{Name: "pb", Target: "node",
		Steps: []playbook.Step{{Name: "diag", Action: "agent.run_diag"}}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"pb": book}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1})
	c := New(st, st, engine, gate, nil, nil, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	now := time.Now()
	inc := &types.Incident{
		ID: "inc-halt", Target: types.Target{Node: "node-a"},
		Class: types.ClassFellOffBus, State: types.StateEvaluating,
		Playbook: "pb", OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
		RemediationSlotHeld: true,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	gate.OccupyRemediation(inc.Target) // the in-memory projection of the bit

	if err := c.quarantine(ctx, inc, "operator takeover"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateNeedsHuman || got.RemediationSlotHeld {
		t.Fatalf("after halt: state=%s bit=%v, want NEEDS_HUMAN with the bit cleared durably", got.State, got.RemediationSlotHeld)
	}
	if err := gate.Allow(types.Target{Node: "node-b"}, types.ActionGPUReset); err != nil {
		t.Fatalf("the halted incident's slot must be free: %v", err)
	}
}

// Crash window: gate.Allow succeeded but the EXECUTING transition never
// committed (state moved concurrently). The bit must not be durable and a
// rebuild from the row must occupy nothing — the failed admission cannot leak
// occupancy into the next leadership.
func TestFailedExecutingTransitionLeavesNoDurableSlot(t *testing.T) {
	book := &playbook.Playbook{Name: "pb", Target: "node",
		Steps: []playbook.Step{{Name: "diag", Action: "agent.run_diag"}}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"pb": book}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1})
	c := New(st, st, engine, gate, nil, nil, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	now := time.Now()
	inc := &types.Incident{
		ID: "inc-conflict", Target: types.Target{Node: "node-a"},
		Class: types.ClassFellOffBus, State: types.StateEvaluating,
		Playbook: "pb", OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	// Move the durable state out from under the snapshot so the EXECUTING
	// transition fails its fresh.State check.
	moved, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	moved.State = types.StateObserving
	if err := st.UpdateIncident(ctx, moved); err != nil {
		t.Fatal(err)
	}

	step := &book.Steps[0]
	if err := c.startStep(ctx, inc, step, "system"); err == nil {
		t.Fatal("startStep must surface the transition conflict")
	}
	if inc.RemediationSlotHeld {
		t.Fatal("the in-memory bit must be undone when the transition never committed")
	}
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemediationSlotHeld {
		t.Fatal("the durable bit must not exist for a transition that never committed")
	}
	// The gate slot acquired for the failed step was handed back.
	if err := gate.Allow(types.Target{Node: "node-b"}, types.ActionGPUReset); err != nil {
		t.Fatalf("failed admission leaked gate occupancy: %v", err)
	}
}
