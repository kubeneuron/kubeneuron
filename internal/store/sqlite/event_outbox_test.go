package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func eventForOutbox(id string) *types.AgentEvent {
	return &types.AgentEvent{
		EventID:   id,
		Node:      "node-a",
		GPUIndex:  2,
		GPUUUID:   "GPU-abc",
		XID:       79,
		Raw:       "NVRM: Xid (PCI:0000:01:00): 79",
		Timestamp: time.Now().UTC().Truncate(time.Nanosecond),
	}
}

func TestWriteEventAtomicallyArchivesAndEnqueues(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	ev := eventForOutbox("event-1")

	fresh, err := s.WriteEvent(ctx, ev)
	if err != nil || !fresh {
		t.Fatalf("first WriteEvent = %v, %v; want fresh archive", fresh, err)
	}
	// The externally compatible EventSink path must use the same atomic
	// outbox hand-off, not merely archive the raw event.
	var events, pending int
	if err := s.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := s.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_outbox WHERE state='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if events != 1 || pending != 1 {
		t.Fatalf("archive/outbox rows = %d/%d, want 1/1", events, pending)
	}

	// At-least-once replay must neither duplicate the archive nor enqueue a
	// second workflow item, even after the original one was acknowledged.
	claimed, err := s.ClaimNextEvent(ctx, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Event.EventID != ev.EventID || claimed.Event.GPUUUID != ev.GPUUUID || claimed.Attempt != 1 {
		t.Fatalf("claimed event = %+v, want archived event on first attempt", claimed)
	}
	if err := s.CompleteClaimedEvent(ctx, claimed.OutboxID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	fresh, err = s.ArchiveAndEnqueueEvent(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("replayed EventID reported fresh=true")
	}
	if _, err := s.ClaimNextEvent(ctx, "worker-b", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim after duplicate replay = %v, want ErrNotFound", err)
	}
}

func TestArchiveAndEnqueueEventRollsBackWhenOutboxInsertFails(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	if _, err := s.sqlDB.ExecContext(ctx, `
		CREATE TRIGGER reject_event_outbox
		BEFORE INSERT ON event_outbox
		BEGIN
			SELECT RAISE(ABORT, 'forced outbox failure');
		END`); err != nil {
		t.Fatal(err)
	}

	fresh, err := s.ArchiveAndEnqueueEvent(ctx, eventForOutbox("event-rollback"))
	if err == nil {
		t.Fatalf("ArchiveAndEnqueueEvent = %v, nil; want outbox failure", fresh)
	}
	var events, outbox int
	if err := s.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := s.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if events != 0 || outbox != 0 {
		t.Fatalf("failed archive left events/outbox rows = %d/%d, want 0/0", events, outbox)
	}
}

func TestClaimNextEventLeasesAndCompletes(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	first := eventForOutbox("event-1")
	second := eventForOutbox("event-2")
	if _, err := s.ArchiveAndEnqueueEvent(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ArchiveAndEnqueueEvent(ctx, second); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	claimedFirst, err := s.ClaimNextEvent(ctx, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimedFirst.Event.EventID != first.EventID {
		t.Fatalf("first claim EventID = %q, want %q", claimedFirst.Event.EventID, first.EventID)
	}
	if claimedFirst.LeaseToken == "" || !claimedFirst.LeaseExpiresAt.After(before) {
		t.Fatalf("first claim missing valid lease: %+v", claimedFirst)
	}

	// A worker pool can claim separate events concurrently; leases isolate the
	// acknowledgements rather than serializing the entire queue.
	claimedSecond, err := s.ClaimNextEvent(ctx, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimedSecond.Event.EventID != second.EventID {
		t.Fatalf("second claim EventID = %q, want %q", claimedSecond.Event.EventID, second.EventID)
	}
	if err := s.CompleteClaimedEvent(ctx, claimedFirst.OutboxID, "wrong-token"); !errors.Is(err, store.ErrEventLeaseLost) {
		t.Fatalf("completion with wrong token = %v, want ErrEventLeaseLost", err)
	}
	if err := s.CompleteClaimedEvent(ctx, claimedFirst.OutboxID, claimedFirst.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteClaimedEvent(ctx, claimedSecond.OutboxID, claimedSecond.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextEvent(ctx, "worker-c", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim after completions = %v, want ErrNotFound", err)
	}
}

func TestClaimNextEventReclaimsExpiredLeaseAndRejectsStaleCompletion(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	if _, err := s.ArchiveAndEnqueueEvent(ctx, eventForOutbox("event-reclaim")); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimNextEvent(ctx, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.sqlDB.ExecContext(ctx,
		`UPDATE event_outbox SET lease_expires_at_ns=? WHERE id=?`, time.Now().Add(-time.Second).UnixNano(), first.OutboxID); err != nil {
		t.Fatal(err)
	}

	second, err := s.ClaimNextEvent(ctx, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.OutboxID != first.OutboxID || second.Attempt != 2 {
		t.Fatalf("reclaim = %+v, want outbox ID %d at attempt 2", second, first.OutboxID)
	}
	if second.LeaseToken == first.LeaseToken {
		t.Fatal("reclaimed event reused prior lease token")
	}
	if err := s.CompleteClaimedEvent(ctx, first.OutboxID, first.LeaseToken); !errors.Is(err, store.ErrEventLeaseLost) {
		t.Fatalf("stale completion = %v, want ErrEventLeaseLost", err)
	}
	if err := s.CompleteClaimedEvent(ctx, second.OutboxID, second.LeaseToken); err != nil {
		t.Fatal(err)
	}
}

func TestClaimNextEventRejectsInvalidLeaseAndUnknownCompletion(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	if _, err := s.ArchiveAndEnqueueEvent(ctx, eventForOutbox("event-invalid")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextEvent(ctx, "", time.Minute); err == nil {
		t.Fatal("claim with empty worker ID succeeded")
	}
	if _, err := s.ClaimNextEvent(ctx, "worker-a", 0); err == nil {
		t.Fatal("claim with zero lease duration succeeded")
	}
	if err := s.CompleteClaimedEvent(ctx, 999, "token"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("completion of unknown outbox row = %v, want ErrNotFound", err)
	}
}

func TestProcessClaimedEventCommitsWorkflowMutationAndAcknowledgementTogether(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	ev := eventForOutbox("event-process-success")
	if _, err := s.ArchiveAndEnqueueEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextEvent(ctx, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Nanosecond)
	target := types.Target{Node: ev.Node, GPUUUID: ev.GPUUUID, GPUIndex: ev.GPUIndex}
	inc := &types.Incident{
		ID:             "inc-process-success",
		Target:         target,
		Class:          types.ClassFellOffBus,
		State:          types.StateOpen,
		SignalSeen:     1,
		OpenedAt:       now,
		UpdatedAt:      now,
		StateChangedAt: now,
	}
	err = s.ProcessClaimedEvent(ctx, claimed.OutboxID, claimed.LeaseToken, func(tx store.Tx) error {
		if _, err := tx.GetOpenIncident(ctx, target, inc.Class); !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := tx.CreateIncident(ctx, inc); err != nil {
			return err
		}
		// The Tx read must observe the incident opened by this callback, so a
		// worker can atomically choose between opening and updating an incident.
		got, err := tx.GetOpenIncident(ctx, target, inc.Class)
		if err != nil {
			return err
		}
		if got.ID != inc.ID {
			return errors.New("transactional incident lookup returned wrong incident")
		}
		return tx.AppendAudit(ctx, &types.AuditEntry{
			IncidentID: inc.ID,
			Time:       now,
			FromState:  types.StateOpen,
			ToState:    types.StateOpen,
			Actor:      "event-outbox-test",
			Action:     "open",
		})
	})
	if err != nil {
		t.Fatalf("ProcessClaimedEvent: %v", err)
	}

	if _, err := s.GetIncident(ctx, inc.ID); err != nil {
		t.Fatalf("workflow incident not committed: %v", err)
	}
	trail, err := s.AuditTrail(ctx, inc.ID)
	if err != nil || len(trail) != 1 {
		t.Fatalf("workflow audit = %d entries, %v; want one committed entry", len(trail), err)
	}
	if _, err := s.ClaimNextEvent(ctx, "worker-b", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("successfully processed event claimed again: %v, want ErrNotFound", err)
	}
}

func TestProcessClaimedEventRollsBackCallbackAndRemainsRetryable(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	if _, err := s.ArchiveAndEnqueueEvent(ctx, eventForOutbox("event-process-rollback")); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimNextEvent(ctx, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	boom := errors.New("workflow callback failed")
	err = s.ProcessClaimedEvent(ctx, first.OutboxID, first.LeaseToken, func(tx store.Tx) error {
		if err := tx.CreateIncident(ctx, testIncident("inc-process-rollback", time.Now())); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("ProcessClaimedEvent callback error = %v, want %v", err, boom)
	}
	if _, err := s.GetIncident(ctx, "inc-process-rollback"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("callback incident visible after rollback: %v, want ErrNotFound", err)
	}
	var state string
	if err := s.sqlDB.QueryRowContext(ctx, `SELECT state FROM event_outbox WHERE id=?`, first.OutboxID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "leased" {
		t.Fatalf("outbox state after callback rollback = %q, want leased", state)
	}

	// A failed callback leaves the durable item outstanding. Expire this test
	// lease without sleeping and prove a later worker can process it exactly
	// once.
	if _, err := s.sqlDB.ExecContext(ctx,
		`UPDATE event_outbox SET lease_expires_at_ns=? WHERE id=?`, time.Now().Add(-time.Second).UnixNano(), first.OutboxID); err != nil {
		t.Fatal(err)
	}
	retry, err := s.ClaimNextEvent(ctx, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if retry.OutboxID != first.OutboxID || retry.Attempt != 2 {
		t.Fatalf("retry = %+v, want same outbox item at attempt 2", retry)
	}
	if err := s.ProcessClaimedEvent(ctx, retry.OutboxID, retry.LeaseToken, func(store.Tx) error { return nil }); err != nil {
		t.Fatalf("retry ProcessClaimedEvent: %v", err)
	}
	if _, err := s.ClaimNextEvent(ctx, "worker-c", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("successfully retried event claimed again: %v, want ErrNotFound", err)
	}
}

func TestProcessClaimedEventRejectsStaleTokenBeforeCallback(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	if _, err := s.ArchiveAndEnqueueEvent(ctx, eventForOutbox("event-process-stale")); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimNextEvent(ctx, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.sqlDB.ExecContext(ctx,
		`UPDATE event_outbox SET lease_expires_at_ns=? WHERE id=?`, time.Now().Add(-time.Second).UnixNano(), first.OutboxID); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimNextEvent(ctx, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	called := false
	err = s.ProcessClaimedEvent(ctx, first.OutboxID, first.LeaseToken, func(store.Tx) error {
		called = true
		return nil
	})
	if !errors.Is(err, store.ErrEventLeaseLost) {
		t.Fatalf("stale ProcessClaimedEvent = %v, want ErrEventLeaseLost", err)
	}
	if called {
		t.Fatal("stale token invoked workflow callback")
	}
	if err := s.ProcessClaimedEvent(ctx, second.OutboxID, second.LeaseToken, func(store.Tx) error { return nil }); err != nil {
		t.Fatalf("current ProcessClaimedEvent: %v", err)
	}
}
