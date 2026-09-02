package controller

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/safety"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestDryRunTombstonesQueuedNonRestorativeActions(t *testing.T) {
	ctx := context.Background()
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 1})
	c := New(st, nil, nil, gate, nil, nil, nil, nil, log)
	if err := st.EnqueueAction(ctx, "node-a", types.Action{ID: "stop-me", Type: types.ActionGPUReset}); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueAction(ctx, "node-a", types.Action{ID: "also-stop-me", Type: types.ActionRunDiag}); err != nil {
		t.Fatal(err)
	}

	dry := safety.Limits{MaxConcurrentRemediations: 1, DryRun: true}
	if err := c.InstallRuntimeConfigContext(ctx, RuntimeConfig{SafetyLimits: &dry}); err != nil {
		t.Fatalf("switch to DryRun: %v", err)
	}
	stopped, err := st.GetAction(ctx, "stop-me")
	if err != nil || !stopped.Cancelled {
		t.Fatalf("queued action after stop = %+v, %v; want cancelled", stopped, err)
	}
	diagnostic, err := st.GetAction(ctx, "also-stop-me")
	if err != nil || !diagnostic.Cancelled {
		t.Fatalf("queued diagnostic after stop = %+v, %v; want cancelled", diagnostic, err)
	}
	if got, err := c.NextAction(httptest.NewRequest("GET", types.AgentActionLeasePath, nil), "node-a"); err != nil || got != nil {
		t.Fatalf("NextAction during DryRun = %+v, %v; want no action", got, err)
	}

	// Actions accumulated while stopped must not spring to life when execution
	// is enabled again. The resume transition is a new authorization boundary.
	if err := st.EnqueueAction(ctx, "node-a", types.Action{ID: "queued-while-dry", Type: types.ActionReboot}); err != nil {
		t.Fatal(err)
	}
	live := safety.Limits{MaxConcurrentRemediations: 1}
	if err := c.InstallRuntimeConfigContext(ctx, RuntimeConfig{SafetyLimits: &live}); err != nil {
		t.Fatalf("switch to Enabled: %v", err)
	}
	stopped, err = st.GetAction(ctx, "queued-while-dry")
	if err != nil || !stopped.Cancelled {
		t.Fatalf("action queued while DryRun = %+v, %v; want cancelled", stopped, err)
	}
}

func TestPauseTombstonesQueuedActionsButPreservesHostRestore(t *testing.T) {
	ctx := context.Background()
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 1})
	if err := gate.RestoreAndPersist(ctx, st, log); err != nil {
		t.Fatal(err)
	}
	c := New(st, nil, nil, gate, nil, nil, nil, nil, log)
	if err := st.EnqueueAction(ctx, "node-a", types.Action{ID: "cancel-on-pause", Type: types.ActionGPUReset}); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueAction(ctx, "node-a", types.Action{ID: "restore-on-pause", Type: types.ActionRestoreAcceleratorHost}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetPaused(ctx, true, "alice"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	cancelled, err := st.GetAction(ctx, "cancel-on-pause")
	if err != nil || !cancelled.Cancelled {
		t.Fatalf("destructive action after pause = %+v, %v; want cancelled", cancelled, err)
	}
	restore, err := st.GetAction(ctx, "restore-on-pause")
	if err != nil || restore.Cancelled {
		t.Fatalf("host restore after pause = %+v, %v; want retained", restore, err)
	}

	// A destructive action that arrived after the stop must not hide the older
	// restore. NextAction filters in the atomic claim, rather than leasing the
	// oldest action first and discovering too late that it is unsafe.
	if err := st.EnqueueAction(ctx, "node-a", types.Action{ID: "queued-after-pause", Type: types.ActionGPUReset}); err != nil {
		t.Fatal(err)
	}
	claimed, err := c.NextAction(httptest.NewRequest("GET", types.AgentActionLeasePath, nil), "node-a")
	if err != nil || claimed == nil || claimed.Action.Type != types.ActionRestoreAcceleratorHost {
		t.Fatalf("NextAction while paused = %+v, %v; want retained host restore", claimed, err)
	}
	unsafe, err := st.GetAction(ctx, "queued-after-pause")
	if err != nil || unsafe.LeaseToken != "" || unsafe.Cancelled {
		t.Fatalf("destructive action while paused = %+v, %v; want pending and unleased", unsafe, err)
	}
}
