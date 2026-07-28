package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestClaimRecordsAttemptsAndExecutorBoot(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	enqueueLeaseTestAction(t, s, "action-1")

	first, err := s.ClaimNextAction(ctx, "node-a", "boot-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first.Attempts != 1 || first.ExecutorBootID != "boot-1" {
		t.Fatalf("first claim attempts=%d boot=%q, want 1/boot-1", first.Attempts, first.ExecutorBootID)
	}
	if _, err := s.sqlDB.ExecContext(ctx,
		`UPDATE actions SET lease_expires_at_ns=? WHERE id=?`, time.Now().Add(-time.Second).UnixNano(), first.Action.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimNextAction(ctx, "node-a", "boot-2", time.Minute)
	if err != nil {
		t.Fatalf("reclaim after expiry: %v", err)
	}
	if second.Attempts != 2 || second.ExecutorBootID != "boot-2" {
		t.Fatalf("reclaim attempts=%d boot=%q, want 2/boot-2", second.Attempts, second.ExecutorBootID)
	}
}

func TestCompleteClaimedActionRejectsExecutorBootMismatch(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	enqueueLeaseTestAction(t, s, "action-1")

	claimed, err := s.ClaimNextAction(ctx, "node-a", "boot-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// A result posted from a different node boot with the same lease token is
	// not evidence the side effect completed on the boot that started it.
	err = s.CompleteClaimedAction(ctx, claimed.Action.ID, claimed.LeaseToken, "boot-2", types.ActionResult{
		ActionID: claimed.Action.ID, OK: true,
	})
	if !errors.Is(err, store.ErrExecutorBootMismatch) {
		t.Fatalf("cross-boot completion = %v, want ErrExecutorBootMismatch", err)
	}
	if err := s.CompleteClaimedAction(ctx, claimed.Action.ID, claimed.LeaseToken, "boot-1", types.ActionResult{
		ActionID: claimed.Action.ID, OK: true,
	}); err != nil {
		t.Fatalf("same-boot completion: %v", err)
	}
}

func TestCancelPendingActionsForIncidentSparesLeasedWork(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	// action-1 gets leased below; action-2 stays pending on incident-a;
	// action-3 belongs to another incident and must be untouched.
	enqueueLeaseTestAction(t, s, "action-1")
	enqueueLeaseTestAction(t, s, "action-2")
	if err := s.EnqueueAction(ctx, "node-a", "incident-b", types.Action{
		ID: "action-3", Type: types.ActionRunDiag,
	}); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimNextAction(ctx, "node-a", "boot-1", time.Minute)
	if err != nil || claimed.Action.ID != "action-1" {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}

	cancelled, err := s.CancelPendingActionsForIncident(ctx, "incident-a")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled != 1 {
		t.Fatalf("cancelled = %d, want 1 (pending only, this incident only)", cancelled)
	}
	tombstoned, err := s.GetAction(ctx, "action-2")
	if err != nil || !tombstoned.Cancelled {
		t.Fatalf("cancelled action = %+v, %v (want Cancelled)", tombstoned, err)
	}

	// The in-flight lease still completes normally: the node may already be
	// executing, so cancellation must never pretend it was revoked.
	if err := s.CompleteClaimedAction(ctx, "action-1", claimed.LeaseToken, "boot-1", types.ActionResult{
		ActionID: "action-1", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	// The next claim skips the tombstone and serves the other incident.
	next, err := s.ClaimNextAction(ctx, "node-a", "boot-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if next.Action.ID != "action-3" {
		t.Fatalf("claim after cancel = %q, want action-3 (skipping cancelled action-2)", next.Action.ID)
	}
}
