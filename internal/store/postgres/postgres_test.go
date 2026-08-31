package postgres

// Conformance tests against a live PostgreSQL. They run when
// KUBENEURON_TEST_POSTGRES_DSN is set (CI provides a service container;
// locally: docker run -d -p 15432:5432 -e POSTGRES_PASSWORD=test
// -e POSTGRES_DB=kubeneuron postgres:16-alpine) and skip otherwise, so the
// default unit run never needs Docker.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("KUBENEURON_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("KUBENEURON_TEST_POSTGRES_DSN not set")
	}
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Isolated runs: truncate everything between tests.
	for _, table := range []string{
		"event_outbox", "events", "audit_log", "approvals", "actions",
		"accelerator_reports", "safety_state", "incidents", "nodes",
	} {
		if _, err := s.SQL.Exec("TRUNCATE TABLE " + table + " CASCADE"); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	return s
}

func testIncident(id string) *types.Incident {
	now := time.Now()
	return &types.Incident{
		ID: id, Target: types.Target{Node: "node-a", GPUUUID: "GPU-1"},
		Class: types.ClassECCDBE, State: types.StateOpen, SignalSeen: 1,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
}

func TestIncidentLifecycleAndTxAtomicity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.WithTx(ctx, func(tx store.Tx) error {
		if err := tx.CreateIncident(ctx, testIncident("inc-1")); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, &types.AuditEntry{
			IncidentID: "inc-1", Time: time.Now(),
			FromState: types.StateOpen, ToState: types.StateOpen,
			Actor: "system", Action: "open",
		})
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	open, err := s.GetOpenIncident(ctx, types.Target{Node: "node-a", GPUUUID: "GPU-1"}, types.ClassECCDBE)
	if err != nil || open.ID != "inc-1" {
		t.Fatalf("GetOpenIncident = %v, %v", open, err)
	}
	trail, err := s.AuditTrail(ctx, "inc-1")
	if err != nil || len(trail) != 1 {
		t.Fatalf("AuditTrail = %d entries, %v", len(trail), err)
	}

	// A failing fn rolls back both writes.
	sentinel := errors.New("boom")
	err = s.WithTx(ctx, func(tx store.Tx) error {
		other := testIncident("inc-2")
		other.Target.Node = "node-b" // distinct target: no unique-index clash
		if err := tx.CreateIncident(ctx, other); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx error = %v", err)
	}
	if _, err := s.GetIncident(ctx, "inc-2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rolled-back incident visible: %v", err)
	}

	// The partial unique index allows one open incident per (target, class).
	if err := s.CreateIncident(ctx, testIncident("inc-dup")); err == nil {
		t.Fatal("second open incident for the same target/class must be rejected")
	}
}

func TestUpdateIncidentOptimisticConcurrency(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	inc := testIncident("inc-oc")
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// Two writers snapshot the same version.
	a, err := s.GetIncident(ctx, "inc-oc")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.GetIncident(ctx, "inc-oc")
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != 0 {
		t.Fatalf("fresh incident version = %d, want 0", a.Version)
	}

	// The first writer wins and its in-memory version advances.
	a.SignalSeen = 7
	if err := s.UpdateIncident(ctx, a); err != nil {
		t.Fatalf("first update = %v", err)
	}
	if a.Version != 1 {
		t.Fatalf("version after successful update = %d, want 1", a.Version)
	}

	// The stale writer must conflict, not clobber. On READ COMMITTED this is the
	// exact case that used to regress state/StepIndex: a writer working from an
	// earlier snapshot is now rejected instead of overwriting the newer row.
	b.SignalSeen = 99
	if err := s.UpdateIncident(ctx, b); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale update = %v, want ErrConflict", err)
	}
	got, err := s.GetIncident(ctx, "inc-oc")
	if err != nil {
		t.Fatal(err)
	}
	if got.SignalSeen != 7 {
		t.Fatalf("SignalSeen = %d, want 7 (the winning write must survive)", got.SignalSeen)
	}

	// A genuinely absent incident is ErrNotFound, never ErrConflict.
	if err := s.UpdateIncident(ctx, testIncident("inc-ghost")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update of absent incident = %v, want ErrNotFound", err)
	}
}

func TestActionLeaseProtocol(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	action := types.Action{ID: "act-1", IncidentID: "inc-1", Type: types.ActionRunDiag, Timeout: time.Minute}
	if err := s.EnqueueAction(ctx, "node-a", action); err != nil {
		t.Fatal(err)
	}
	// Idempotent enqueue (ON CONFLICT DO NOTHING).
	if err := s.EnqueueAction(ctx, "node-a", action); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimNextAction(ctx, "node-a", "", time.Minute)
	if err != nil || claimed == nil || claimed.Action.ID != "act-1" || claimed.LeaseToken == "" {
		t.Fatalf("ClaimNextAction = %+v, %v", claimed, err)
	}
	// One unexpired lease per node: no claimable work reports ErrNotFound.
	second, err := s.ClaimNextAction(ctx, "node-a", "", time.Minute)
	if !errors.Is(err, store.ErrNotFound) || second != nil {
		t.Fatalf("second claim = %+v, %v (want ErrNotFound)", second, err)
	}
	// A wrong lease token cannot complete the action.
	res := types.ActionResult{ActionID: "act-1", OK: true}
	if err := s.CompleteClaimedAction(ctx, "act-1", "wrong-token", "", res); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("wrong-token completion = %v, want ErrLeaseLost", err)
	}
	if err := s.CompleteClaimedAction(ctx, "act-1", claimed.LeaseToken, "", res); err != nil {
		t.Fatalf("completion with the held lease: %v", err)
	}
	done, err := s.GetAction(ctx, "act-1")
	if err != nil || done.Result == nil || !done.Result.OK {
		t.Fatalf("completed action = %+v, %v", done, err)
	}
}

func TestEventOutboxExactlyOnceProcessing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	fresh, err := s.ArchiveAndEnqueueEvent(ctx, &types.AgentEvent{
		EventID: "ev-1", Node: "node-a", XID: 79, Timestamp: time.Now(),
	})
	if err != nil || !fresh {
		t.Fatalf("archive = %v, %v", fresh, err)
	}
	// Replay of the same capture ID is a clean duplicate.
	fresh, err = s.ArchiveAndEnqueueEvent(ctx, &types.AgentEvent{
		EventID: "ev-1", Node: "node-a", XID: 79, Timestamp: time.Now(),
	})
	if err != nil || fresh {
		t.Fatalf("duplicate archive = %v, %v (want false)", fresh, err)
	}

	claimed, err := s.ClaimNextEvent(ctx, "worker-1", time.Minute)
	if err != nil || claimed == nil || claimed.Event.EventID != "ev-1" {
		t.Fatalf("ClaimNextEvent = %+v, %v", claimed, err)
	}
	// Process commits the mutation and the acknowledgment together.
	err = s.ProcessClaimedEvent(ctx, claimed.OutboxID, claimed.LeaseToken, func(tx store.Tx) error {
		return tx.CreateIncident(ctx, testIncident("inc-from-event"))
	})
	if err != nil {
		t.Fatalf("ProcessClaimedEvent: %v", err)
	}
	if _, err := s.GetIncident(ctx, "inc-from-event"); err != nil {
		t.Fatalf("incident from processed event: %v", err)
	}
	// The done item cannot be claimed again: no claimable work is reported
	// as ErrNotFound (the ingest loop's idle signal).
	if next, err := s.ClaimNextEvent(ctx, "worker-1", time.Minute); !errors.Is(err, store.ErrNotFound) || next != nil {
		t.Fatalf("claim after done = %+v, %v (want ErrNotFound)", next, err)
	}
	// A stale token cannot process anything.
	err = s.ProcessClaimedEvent(ctx, claimed.OutboxID, "stale", func(store.Tx) error { return nil })
	if !errors.Is(err, store.ErrEventLeaseLost) {
		t.Fatalf("stale-token process = %v, want ErrEventLeaseLost", err)
	}
}

func TestAcceleratorReportStalenessAndSafetyState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	report := &types.AgentAcceleratorReport{
		Node: "node-a", NodeUID: "uid-a", Vendor: types.AcceleratorVendorNVIDIA,
		ObservedAt: now, TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
		Readiness:      types.AcceleratorReadinessReady,
		DriverVersion:  "580.159.03",
		RuntimeVersion: "dcgm-3.3",
		Devices: []types.AgentAcceleratorDevice{{
			ID: "GPU-1", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU,
		}},
	}
	if err := s.UpsertAcceleratorReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	stale := *report
	stale.ObservedAt = now.Add(-time.Minute)
	if err := s.UpsertAcceleratorReport(ctx, &stale); !errors.Is(err, store.ErrStaleAcceleratorReport) {
		t.Fatalf("out-of-order report = %v, want ErrStaleAcceleratorReport", err)
	}
	got, err := s.GetAcceleratorReport(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if err != nil || got.Readiness != types.AcceleratorReadinessReady || len(got.Devices) != 1 {
		t.Fatalf("GetAcceleratorReport = %+v, %v", got, err)
	}

	if err := s.SaveSafetyState(ctx, "cooldowns", []byte(`{"k":"v"}`)); err != nil {
		t.Fatal(err)
	}
	payload, err := s.LoadSafetyState(ctx, "cooldowns")
	if err != nil || string(payload) != `{"k":"v"}` {
		t.Fatalf("LoadSafetyState = %q, %v", payload, err)
	}
}

func TestPruneAndSchemaCeiling(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueAction(ctx, "n1", types.Action{IncidentID: "inc-a", ID: "act-old", Type: types.ActionRunDiag}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAction(ctx, "act-old", types.ActionResult{ActionID: "act-old", OK: true}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := s.SQL.Exec(`UPDATE actions SET updated_at=$1`, old); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Prune(ctx, 24*time.Hour, 0)
	if err != nil || stats.Actions != 1 {
		t.Fatalf("Prune stats = %+v, %v (want 1 action)", stats, err)
	}

	// An older binary refuses a newer schema.
	if _, err := s.SQL.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (9999, 'now')`); err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("KUBENEURON_TEST_POSTGRES_DSN")
	if _, err := Open(context.Background(), dsn); err == nil {
		t.Fatal("Open must refuse a future schema version")
	}
	if _, err := s.SQL.Exec(`DELETE FROM schema_version WHERE version = 9999`); err != nil {
		t.Fatal(err)
	}
}

func TestActionProtocolAttemptsBootAndCancellation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, a := range []struct{ id, incident string }{
		{"act-1", "inc-1"}, {"act-2", "inc-1"}, {"act-3", "inc-2"},
	} {
		if err := s.EnqueueAction(ctx, "node-a", types.Action{IncidentID: a.incident,
			ID: a.id, Type: types.ActionRunDiag,
		}); err != nil {
			t.Fatal(err)
		}
	}

	claimed, err := s.ClaimNextAction(ctx, "node-a", "boot-1", time.Minute)
	if err != nil || claimed.Action.ID != "act-1" {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if claimed.Attempts != 1 || claimed.ExecutorBootID != "boot-1" {
		t.Fatalf("claim attempts=%d boot=%q, want 1/boot-1", claimed.Attempts, claimed.ExecutorBootID)
	}

	// Expire the lease; the reclaim increments attempts and rebinds the boot.
	if _, err := s.SQL.Exec(
		`UPDATE actions SET lease_expires_at_ns=$1 WHERE id=$2`,
		time.Now().Add(-time.Second).UnixNano(), "act-1"); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ClaimNextAction(ctx, "node-a", "boot-2", time.Minute)
	if err != nil || reclaimed.Action.ID != "act-1" {
		t.Fatalf("reclaim = %+v, %v", reclaimed, err)
	}
	if reclaimed.Attempts != 2 || reclaimed.ExecutorBootID != "boot-2" {
		t.Fatalf("reclaim attempts=%d boot=%q, want 2/boot-2", reclaimed.Attempts, reclaimed.ExecutorBootID)
	}

	// A result from a different boot with the current lease is a mismatch.
	err = s.CompleteClaimedAction(ctx, "act-1", reclaimed.LeaseToken, "boot-3",
		types.ActionResult{ActionID: "act-1", OK: true})
	if !errors.Is(err, store.ErrExecutorBootMismatch) {
		t.Fatalf("cross-boot completion = %v, want ErrExecutorBootMismatch", err)
	}

	// Cancellation tombstones only this incident's pending work.
	cancelled, err := s.CancelPendingActionsForIncident(ctx, "inc-1")
	if err != nil || cancelled != 1 {
		t.Fatalf("cancelled = %d, %v (want 1: act-2 only)", cancelled, err)
	}
	tombstoned, err := s.GetAction(ctx, "act-2")
	if err != nil || !tombstoned.Cancelled {
		t.Fatalf("cancelled action = %+v, %v (want Cancelled)", tombstoned, err)
	}

	// The leased action still completes on its own boot, and the next claim
	// skips the tombstone to serve the other incident.
	if err := s.CompleteClaimedAction(ctx, "act-1", reclaimed.LeaseToken, "boot-2",
		types.ActionResult{ActionID: "act-1", OK: true}); err != nil {
		t.Fatal(err)
	}
	next, err := s.ClaimNextAction(ctx, "node-a", "boot-2", time.Minute)
	if err != nil || next.Action.ID != "act-3" {
		t.Fatalf("claim after cancel = %+v, %v (want act-3)", next, err)
	}
}
