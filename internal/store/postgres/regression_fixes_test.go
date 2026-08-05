package postgres

// Regression tests for three STORE-layer defects, exercised against a live
// PostgreSQL (the engine whose READ COMMITTED concurrency exposed them).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlcore"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Fix 1: N concurrent claimers of one queued action must yield exactly one
// winner. The pre-fix UPDATE ... WHERE id=(SELECT ... LIMIT 1) had no
// claimable-state predicate in the outer WHERE, so two claimers picked and
// re-leased the same row, overwriting the token and double-counting attempts.
func TestClaimNextActionConcurrentSingleWinner(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueAction(ctx, "node-a", types.Action{IncidentID: "inc-1",
		ID: "act-1", Type: types.ActionGPUReset, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	const claimers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		tokens  = map[string]struct{}{}
	)
	start := make(chan struct{})
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claimed, err := s.ClaimNextAction(ctx, "node-a", "boot-x", time.Minute)
			if err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					t.Errorf("claim: unexpected error %v", err)
				}
				return
			}
			mu.Lock()
			winners++
			tokens[claimed.LeaseToken] = struct{}{}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Fatalf("concurrent claimers produced %d winners, want exactly 1", winners)
	}
	if len(tokens) != 1 {
		t.Fatalf("distinct lease tokens = %d, want 1 (no lease overwrite)", len(tokens))
	}
	got, err := s.GetAction(ctx, "act-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no double-count)", got.Attempts)
	}
}

// Fix 12: after an incident terminalizes, an action whose lease has expired is
// provably not executing and must be tombstoned, not re-enter the pool.
func TestTerminalizedIncidentExpiredLeaseNotReclaimable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueAction(ctx, "node-a", types.Action{IncidentID: "inc-gone",
		ID: "act-orphan", Type: types.ActionGPUReset, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextAction(ctx, "node-a", "boot-1", time.Minute)
	if err != nil || claimed.Action.ID != "act-orphan" {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	// The agent crashes; the lease expires.
	if _, err := s.SQL.Exec(
		`UPDATE actions SET lease_expires_at_ns=$1 WHERE id=$2`,
		time.Now().Add(-time.Second).UnixNano(), "act-orphan"); err != nil {
		t.Fatal(err)
	}
	// A human takes over and the incident terminalizes.
	cancelled, err := s.CancelPendingActionsForIncident(ctx, "inc-gone")
	if err != nil || cancelled != 1 {
		t.Fatalf("cancel = %d, %v (want 1: the expired-lease orphan)", cancelled, err)
	}
	if next, err := s.ClaimNextAction(ctx, "node-a", "boot-2", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reclaim after terminalize = %+v, %v (want ErrNotFound)", next, err)
	}
	got, err := s.GetAction(ctx, "act-orphan")
	if err != nil || !got.Cancelled {
		t.Fatalf("orphan = %+v, %v (want Cancelled)", got, err)
	}
}

// Fix 12: an unexpired lease at terminalization is genuinely in-flight and must
// survive, so its boot-bound completion contract still holds.
func TestTerminalizeLeavesInFlightLeaseAlone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueAction(ctx, "node-a", types.Action{IncidentID: "inc-live",
		ID: "act-live", Type: types.ActionGPUReset, Timeout: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextAction(ctx, "node-a", "boot-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.CancelPendingActionsForIncident(ctx, "inc-live")
	if err != nil || cancelled != 0 {
		t.Fatalf("cancel of in-flight lease = %d, %v (want 0)", cancelled, err)
	}
	// The node still finishes the work it was leased.
	if err := s.CompleteClaimedAction(ctx, "act-live", claimed.LeaseToken, "boot-1",
		types.ActionResult{ActionID: "act-live", OK: true}); err != nil {
		t.Fatalf("completion of surviving in-flight lease: %v", err)
	}
}

// Fix 12: a poison action must be dead-lettered once it exhausts its attempt
// budget instead of being re-leased on every expiry forever.
func TestActionDeadLetteredAfterMaxAttempts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueAction(ctx, "node-a", types.Action{IncidentID: "inc-poison",
		ID: "act-poison", Type: types.ActionGPUReset, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= sqlcore.MaxActionAttempts; i++ {
		claimed, err := s.ClaimNextAction(ctx, "node-a", "boot-x", time.Minute)
		if err != nil {
			t.Fatalf("claim %d: %v (poison action re-entered pool too few times)", i, err)
		}
		if claimed.Attempts != i {
			t.Fatalf("claim %d attempts = %d, want %d", i, claimed.Attempts, i)
		}
		if _, err := s.SQL.Exec(
			`UPDATE actions SET lease_expires_at_ns=$1 WHERE id=$2`,
			time.Now().Add(-time.Second).UnixNano(), "act-poison"); err != nil {
			t.Fatal(err)
		}
	}
	// The budget is spent: the next claim dead-letters it instead of re-leasing.
	if next, err := s.ClaimNextAction(ctx, "node-a", "boot-x", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim past budget = %+v, %v (want ErrNotFound)", next, err)
	}
	var state string
	var attempts int
	if err := s.SQL.QueryRow(
		`SELECT state, attempts FROM actions WHERE id=$1`, "act-poison").Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "dead" {
		t.Fatalf("poison action state = %q, want \"dead\"", state)
	}
	if attempts != sqlcore.MaxActionAttempts {
		t.Fatalf("poison action attempts = %d, want %d", attempts, sqlcore.MaxActionAttempts)
	}
}

// Fix 3: DeviceHolders must round-trip through upsert->get with the
// nil-vs-empty distinction intact — the reset gate reads nil as "agent did not
// look" and empty as "looked, none present", and the two must not be confused.
func TestAcceleratorReportDeviceHoldersRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	base := func(node string, holders []types.AgentDeviceHolder) *types.AgentAcceleratorReport {
		return &types.AgentAcceleratorReport{
			Node: node, Vendor: types.AcceleratorVendorNVIDIA,
			ObservedAt: now, TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
			Readiness: types.AcceleratorReadinessReady, DriverVersion: "580.1", RuntimeVersion: "dcgm-3.3",
			Devices: []types.AgentAcceleratorDevice{{
				ID: "GPU-1", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU,
			}},
			DeviceHolders: holders,
		}
	}

	// A held device round-trips as a populated slice.
	held := base("node-held", []types.AgentDeviceHolder{{PID: 42, Command: "nv-fabricmanager", Device: "/dev/nvidia0"}})
	if err := s.UpsertAcceleratorReport(ctx, held); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAcceleratorReport(ctx, "node-held", types.AcceleratorVendorNVIDIA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.DeviceHolders) != 1 || got.DeviceHolders[0].Command != "nv-fabricmanager" {
		t.Fatalf("held holders = %+v, want one nv-fabricmanager holder", got.DeviceHolders)
	}

	// An explicitly empty set ("observed, none present") stays non-nil.
	empty := base("node-empty", []types.AgentDeviceHolder{})
	if err := s.UpsertAcceleratorReport(ctx, empty); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAcceleratorReport(ctx, "node-empty", types.AcceleratorVendorNVIDIA)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeviceHolders == nil {
		t.Fatal("empty holders came back nil: lost the observed/not-observed distinction")
	}
	if len(got.DeviceHolders) != 0 {
		t.Fatalf("empty holders = %+v, want len 0", got.DeviceHolders)
	}

	// A nil set ("agent did not look") stays nil.
	unobserved := base("node-nil", nil)
	if err := s.UpsertAcceleratorReport(ctx, unobserved); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAcceleratorReport(ctx, "node-nil", types.AcceleratorVendorNVIDIA)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeviceHolders != nil {
		t.Fatalf("nil holders came back %+v, want nil", got.DeviceHolders)
	}
}

// Fix 2: an action leased for an incident that then terminalizes must not
// re-enter the claimable pool once its lease expires. The one-shot cancellation
// spares the unexpired lease at terminalization; the ClaimNextAction join on
// incidents keeps the orphaned action out of a restarted agent's hands for an
// incident a human already took over. A live incident's expired-lease action
// stays reclaimable (the control).
func TestClaimSkipsActionOfTerminalizedIncident(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Distinct targets so both incidents stay open under the one-open-incident-
	// per-target unique index.
	for _, id := range []string{"inc-taken-over", "inc-live"} {
		inc := testIncident(id)
		inc.Target.GPUUUID = "GPU-" + id
		if err := s.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EnqueueAction(ctx, "node-dead", types.Action{IncidentID: "inc-taken-over",
		ID: "act-terminal", Type: types.ActionGPUReset, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueAction(ctx, "node-live", types.Action{IncidentID: "inc-live",
		ID: "act-live-2", Type: types.ActionGPUReset, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	for _, node := range []string{"node-dead", "node-live"} {
		if _, err := s.ClaimNextAction(ctx, node, "boot-1", time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SQL.Exec(
		`UPDATE actions SET lease_expires_at_ns=$1 WHERE id IN ('act-terminal','act-live-2')`,
		time.Now().Add(-time.Second).UnixNano()); err != nil {
		t.Fatal(err)
	}
	// Quarantine failed the first incident closed to NEEDS_HUMAN.
	if _, err := s.SQL.Exec(`UPDATE incidents SET state='NEEDS_HUMAN' WHERE id=$1`, "inc-taken-over"); err != nil {
		t.Fatal(err)
	}

	if next, err := s.ClaimNextAction(ctx, "node-dead", "boot-2", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim of terminalized incident's action = %+v, %v (want ErrNotFound)", next, err)
	}
	claimed, err := s.ClaimNextAction(ctx, "node-live", "boot-2", time.Minute)
	if err != nil {
		t.Fatalf("reclaim of live incident's action: %v", err)
	}
	if claimed.Action.ID != "act-live-2" {
		t.Fatalf("reclaimed %q, want act-live-2 (live incident must stay claimable)", claimed.Action.ID)
	}
}

// Fix 6: an outbox event that exhausts its attempt budget is dead-lettered and
// no longer claimable, so a deterministically-failing event stops re-leasing
// forever and aborting each drain batch.
func TestEventDeadLetteredAfterMaxAttempts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.ArchiveAndEnqueueEvent(ctx, &types.AgentEvent{
		EventID: "poison-event", Node: "node-a", XID: 79, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= sqlcore.MaxEventAttempts; i++ {
		claimed, err := s.ClaimNextEvent(ctx, "worker-x", time.Minute)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if claimed.Attempt != i {
			t.Fatalf("claim %d attempt = %d, want %d", i, claimed.Attempt, i)
		}
		if _, err := s.SQL.Exec(
			`UPDATE event_outbox SET lease_expires_at_ns=$1 WHERE id=$2`,
			time.Now().Add(-time.Second).UnixNano(), claimed.OutboxID); err != nil {
			t.Fatal(err)
		}
	}
	if next, err := s.ClaimNextEvent(ctx, "worker-x", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim past budget = %+v, %v (want ErrNotFound)", next, err)
	}
	var state string
	var attempts int
	if err := s.SQL.QueryRow(
		`SELECT state, attempts FROM event_outbox
		 WHERE event_row_id=(SELECT id FROM events WHERE event_id=$1)`, "poison-event").Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "dead" {
		t.Fatalf("poison event state = %q, want \"dead\"", state)
	}
	if attempts != sqlcore.MaxEventAttempts {
		t.Fatalf("poison event attempts = %d, want %d", attempts, sqlcore.MaxEventAttempts)
	}
}

// Fix 7: retention prunes terminal 'dead' and 'cancelled' actions, not only
// 'done'; a claimable 'pending' action survives.
func TestPrunePrunesDeadAndCancelledActions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mk := func(id string) {
		t.Helper()
		if err := s.EnqueueAction(ctx, "node-a", types.Action{IncidentID: "inc-" + id,
			ID: id, Type: types.ActionRunDiag,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("act-done")
	if err := s.CompleteAction(ctx, "act-done", types.ActionResult{ActionID: "act-done", OK: true}); err != nil {
		t.Fatal(err)
	}
	mk("act-cancelled")
	if _, err := s.CancelPendingActionsForIncident(ctx, "inc-act-cancelled"); err != nil {
		t.Fatal(err)
	}
	mk("act-dead")
	if _, err := s.SQL.Exec(`UPDATE actions SET state='dead' WHERE id=$1`, "act-dead"); err != nil {
		t.Fatal(err)
	}
	mk("act-pending")

	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := s.SQL.Exec(`UPDATE actions SET updated_at=$1`, old); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Prune(ctx, 24*time.Hour, 0)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if stats.Actions != 3 {
		t.Fatalf("pruned actions = %d, want 3 (done, dead, cancelled)", stats.Actions)
	}
	var remaining int
	var survivor string
	if err := s.SQL.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id),'') FROM actions`).Scan(&remaining, &survivor); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 || survivor != "act-pending" {
		t.Fatalf("after prune count=%d survivor=%q, want 1/act-pending", remaining, survivor)
	}
}

// Nit: a completion from a different boot on an EXPIRED lease is diagnosed as a
// lost lease (the action was reclaimable), while a foreign boot on an unexpired
// lease is a true boot mismatch. Both fail closed; only the diagnosis differs.
func TestCompleteExpiredLeaseIsLeaseLostNotBootMismatch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueAction(ctx, "node-a", types.Action{IncidentID: "inc-a",
		ID: "act-boot", Type: types.ActionGPUReset, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextAction(ctx, "node-a", "boot-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SQL.Exec(
		`UPDATE actions SET lease_expires_at_ns=$1 WHERE id=$2`,
		time.Now().Add(-time.Second).UnixNano(), "act-boot"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteClaimedAction(ctx, "act-boot", claimed.LeaseToken, "boot-2",
		types.ActionResult{ActionID: "act-boot", OK: true}); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("expired-lease foreign-boot completion = %v, want ErrLeaseLost", err)
	}

	if err := s.EnqueueAction(ctx, "node-b", types.Action{IncidentID: "inc-b",
		ID: "act-boot2", Type: types.ActionGPUReset, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	live, err := s.ClaimNextAction(ctx, "node-b", "boot-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteClaimedAction(ctx, "act-boot2", live.LeaseToken, "boot-2",
		types.ActionResult{ActionID: "act-boot2", OK: true}); !errors.Is(err, store.ErrExecutorBootMismatch) {
		t.Fatalf("unexpired foreign-boot completion = %v, want ErrExecutorBootMismatch", err)
	}
}

// Fix (round 5, finding 1 — R6 fault-envelope regression): a neutral-fault
// fallback event (XID=0 + Fault{nvidia, ecc-dbe} + a PCI address) must keep its
// Fault and PCI address across the durable ArchiveAndEnqueueEvent ->
// ClaimNextEvent round trip. The controller reclassifies the row it reads back
// from the outbox, so a dropped Fault made ClassifyXID(0) fail and the event
// was durably acknowledged as non-actionable, silently losing the
// nvidia-smi/DCGM second detection source for a double-bit ECC error.
func TestOutboxPreservesNeutralFaultEnvelope(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	ev := &types.AgentEvent{
		EventID: "ecc-fallback-1", Node: "node07", GPUIndex: 2, GPUUUID: "GPU-xyz",
		PCIAddr: "0000:65:00.0", XID: 0,
		Fault: &types.FaultSignal{
			Vendor: "nvidia", Source: "nvidia-smi", Code: "ecc-dbe",
			Attributes: map[string]string{"volatile_uncorrectable_ecc": "2"},
		},
		Raw: "nvidia-smi -q: GPU 2 volatile uncorrectable ECC errors=2", Timestamp: time.Now().UTC(),
	}
	fresh, err := s.ArchiveAndEnqueueEvent(ctx, ev)
	if err != nil || !fresh {
		t.Fatalf("ArchiveAndEnqueueEvent = %v, %v; want fresh archive", fresh, err)
	}

	claimed, err := s.ClaimNextEvent(ctx, "worker-1", time.Minute)
	if err != nil {
		t.Fatalf("ClaimNextEvent: %v", err)
	}
	if claimed.Event.Fault == nil {
		t.Fatal("Fault dropped on the outbox round trip: the nvidia-smi/DCGM fallback source is silently lost")
	}
	if claimed.Event.Fault.Vendor != "nvidia" || claimed.Event.Fault.Code != "ecc-dbe" {
		t.Fatalf("Fault = %+v, want nvidia/ecc-dbe", claimed.Event.Fault)
	}
	if claimed.Event.Fault.Attributes["volatile_uncorrectable_ecc"] != "2" {
		t.Fatalf("Fault attributes = %+v, want counter preserved", claimed.Event.Fault.Attributes)
	}
	if claimed.Event.PCIAddr != "0000:65:00.0" {
		t.Fatalf("PCIAddr = %q, want 0000:65:00.0", claimed.Event.PCIAddr)
	}
	// The point of the fix: after the durable round trip the event still
	// classifies to ClassECCDBE, not "non-actionable".
	sig, ok := detect.SignalFromAgentEvent(claimed.Event)
	if !ok {
		t.Fatal("claimed fallback event is not actionable: ClassifyXID(0) fell through because Fault was lost")
	}
	if sig.Class != types.ClassECCDBE {
		t.Fatalf("claimed event class = %s, want ClassECCDBE", sig.Class)
	}
}

// Fix (round 5, finding 1): a legacy event row that predates the fault envelope
// (only an XID, no fault_json) must scan back to a nil Fault, not a fabricated
// one.
func TestOutboxLegacyEventScansNilFault(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Simulate a pre-0006 row: insert into events without fault_json/pci_addr,
	// letting the DEFAULT '' stand in for the columns a legacy writer never set.
	var rowID int64
	if err := s.SQL.QueryRow(`
		INSERT INTO events (event_id, node, gpu_index, gpu_uuid, xid, raw, timestamp)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		"legacy-xid-79", "node-a", 0, "GPU-a", 79, "NVRM: Xid 79",
		time.Now().UTC().Format(time.RFC3339Nano)).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.SQL.Exec(`
		INSERT INTO event_outbox (event_row_id, created_at, updated_at) VALUES ($1, $2, $3)`,
		rowID, now, now); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimNextEvent(ctx, "worker-1", time.Minute)
	if err != nil {
		t.Fatalf("ClaimNextEvent: %v", err)
	}
	if claimed.Event.Fault != nil {
		t.Fatalf("legacy row scanned Fault = %+v, want nil (no envelope was ever stored)", claimed.Event.Fault)
	}
	if claimed.Event.XID != 79 {
		t.Fatalf("legacy XID = %d, want 79", claimed.Event.XID)
	}
}

// Fix (round 5, finding 3 — audit-retention prune): pruning a terminal incident
// must also remove its spared leased action, or the terminal-incident claim
// guard goes vacuous (the join finds no incident) and a stale gpu_reset becomes
// claimable again. An unstamped action with no matching incident must survive.
func TestPruneTerminalIncidentRemovesSparedAction(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// A terminal (RESOLVED) incident whose spared action is claimable-shaped
	// (leased, lease expired) and held out of the pool ONLY by the incident guard.
	inc := testIncident("inc-terminal")
	inc.Target.GPUUUID = "GPU-terminal"
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueAction(ctx, "node-dead", types.Action{IncidentID: "inc-terminal",
		ID: "act-spared", Type: types.ActionGPUReset, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextAction(ctx, "node-dead", "boot-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SQL.Exec(
		`UPDATE actions SET lease_expires_at_ns=$1 WHERE id=$2`,
		time.Now().Add(-time.Second).UnixNano(), "act-spared"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SQL.Exec(
		`UPDATE incidents SET state='RESOLVED' WHERE id=$1`, "inc-terminal"); err != nil {
		t.Fatal(err)
	}

	// An unstamped janitor action on a different node must survive the prune.
	if err := s.EnqueueAction(ctx, "node-live", types.Action{IncidentID: "",
		ID: "act-unstamped", Type: types.ActionRunDiag, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	// Sanity: while the incident row exists the spared action is NOT claimable.
	if next, err := s.ClaimNextAction(ctx, "node-dead", "boot-2", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pre-prune claim = %+v, %v (want ErrNotFound: guarded by terminal incident)", next, err)
	}

	// Age the incident past the audit-retention window and prune audit history.
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := s.SQL.Exec(`UPDATE incidents SET updated_at=$1`, old); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Prune(ctx, 0, 24*time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if stats.Incidents != 1 {
		t.Fatalf("pruned incidents = %d, want 1", stats.Incidents)
	}
	if stats.Actions != 1 {
		t.Fatalf("pruned actions = %d, want 1 (the spared action of the pruned incident)", stats.Actions)
	}

	// The spared action is gone, so node-dead has nothing claimable even though
	// its incident guard is now vacuous — no re-armed gpu_reset.
	if next, err := s.ClaimNextAction(ctx, "node-dead", "boot-3", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("post-prune claim = %+v, %v (want ErrNotFound: spared action re-armed)", next, err)
	}
	// The unstamped action survived and stays claimable.
	claimed, err := s.ClaimNextAction(ctx, "node-live", "boot-3", time.Minute)
	if err != nil {
		t.Fatalf("unstamped action claim: %v", err)
	}
	if claimed.Action.ID != "act-unstamped" {
		t.Fatalf("claimed %q, want act-unstamped (unstamped action must survive prune)", claimed.Action.ID)
	}
}

// Round-7 item A: the remediation-slot bit must round-trip Create/Update/Get —
// it is what a new leader rebuilds gate occupancy from.
func TestIncidentRemediationSlotBitRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	inc := testIncident("inc-slot")
	inc.RemediationSlotHeld = true
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetIncident(ctx, "inc-slot")
	if err != nil || !got.RemediationSlotHeld {
		t.Fatalf("created bit = %v, %v; want held", got, err)
	}
	got.RemediationSlotHeld = false
	if err := s.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	if again, err := s.GetIncident(ctx, "inc-slot"); err != nil || again.RemediationSlotHeld {
		t.Fatalf("updated bit = %v, %v; want cleared", again, err)
	}
}
