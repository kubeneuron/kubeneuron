package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func openLeaseTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func enqueueLeaseTestAction(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.EnqueueAction(context.Background(), "node-a", "incident-a", types.Action{
		ID: id, Type: types.ActionGPUReset,
	}); err != nil {
		t.Fatalf("EnqueueAction(%q): %v", id, err)
	}
}

func TestClaimNextActionLeasesOneActionPerNode(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	enqueueLeaseTestAction(t, s, "action-1")
	enqueueLeaseTestAction(t, s, "action-2")

	before := time.Now()
	first, err := s.ClaimNextAction(ctx, "node-a", "", time.Minute)
	if err != nil {
		t.Fatalf("first ClaimNextAction: %v", err)
	}
	if first.Action.ID != "action-1" {
		t.Fatalf("first claim = %q, want oldest action-1", first.Action.ID)
	}
	if first.LeaseToken == "" {
		t.Fatal("claimed action has no lease token")
	}
	if !first.LeaseExpiresAt.After(before) {
		t.Fatalf("lease expiry = %v, want after claim", first.LeaseExpiresAt)
	}

	// A second poll cannot claim another disruptive action while the first
	// action is actively leased to this node.
	if _, err := s.ClaimNextAction(ctx, "node-a", "", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second ClaimNextAction error = %v, want ErrNotFound", err)
	}
	if err := s.CompleteAction(ctx, first.Action.ID, types.ActionResult{ActionID: first.Action.ID, OK: true}); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("legacy completion of leased action = %v, want ErrLeaseLost", err)
	}

	if err := s.CompleteClaimedAction(ctx, first.Action.ID, first.LeaseToken, "", types.ActionResult{
		ActionID: first.Action.ID, OK: true, Output: "done",
	}); err != nil {
		t.Fatalf("CompleteClaimedAction: %v", err)
	}
	second, err := s.ClaimNextAction(ctx, "node-a", "", time.Minute)
	if err != nil {
		t.Fatalf("claim after completion: %v", err)
	}
	if second.Action.ID != "action-2" {
		t.Fatalf("claim after completion = %q, want action-2", second.Action.ID)
	}

	done, err := s.GetAction(ctx, first.Action.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !done.Done || done.Result == nil || done.Result.Output != "done" {
		t.Fatalf("completed action = %+v, want persisted result", done)
	}
	if done.LeaseToken != "" || !done.LeaseExpiresAt.IsZero() {
		t.Fatalf("completed action retained lease: token=%q expiry=%v", done.LeaseToken, done.LeaseExpiresAt)
	}
}

func TestClaimNextActionReclaimsExpiredLeaseAndRejectsStaleResult(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	enqueueLeaseTestAction(t, s, "action-1")

	first, err := s.ClaimNextAction(ctx, "node-a", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Avoid a sleep-based test: make the original lease unambiguously expired.
	if _, err := s.sqlDB.ExecContext(ctx,
		`UPDATE actions SET lease_expires_at_ns=? WHERE id=?`, time.Now().Add(-time.Second).UnixNano(), first.Action.ID); err != nil {
		t.Fatal(err)
	}

	second, err := s.ClaimNextAction(ctx, "node-a", "", time.Minute)
	if err != nil {
		t.Fatalf("reclaim expired action: %v", err)
	}
	if second.Action.ID != first.Action.ID {
		t.Fatalf("reclaimed %q, want %q", second.Action.ID, first.Action.ID)
	}
	if second.LeaseToken == first.LeaseToken {
		t.Fatal("reclaimed action reused its old lease token")
	}

	if err := s.CompleteClaimedAction(ctx, first.Action.ID, first.LeaseToken, "", types.ActionResult{
		ActionID: first.Action.ID, OK: true,
	}); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("completion with stale token = %v, want ErrLeaseLost", err)
	}
	if err := s.CompleteClaimedAction(ctx, second.Action.ID, second.LeaseToken, "", types.ActionResult{
		ActionID: second.Action.ID, OK: true,
	}); err != nil {
		t.Fatalf("completion with current token: %v", err)
	}
}

func TestClaimedActionRequiresCurrentUnexpiredLease(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	enqueueLeaseTestAction(t, s, "action-1")

	if _, err := s.ClaimNextAction(ctx, "node-a", "", 0); err == nil {
		t.Fatal("zero lease duration was accepted")
	}
	claimed, err := s.ClaimNextAction(ctx, "node-a", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteClaimedAction(ctx, claimed.Action.ID, "wrong-token", "", types.ActionResult{}); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("completion with wrong token = %v, want ErrLeaseLost", err)
	}
	if _, err := s.sqlDB.ExecContext(ctx,
		`UPDATE actions SET lease_expires_at_ns=? WHERE id=?`, time.Now().Add(-time.Second).UnixNano(), claimed.Action.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteClaimedAction(ctx, claimed.Action.ID, claimed.LeaseToken, "", types.ActionResult{}); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("completion with expired lease = %v, want ErrLeaseLost", err)
	}
	if err := s.CompleteClaimedAction(ctx, "does-not-exist", "token", "", types.ActionResult{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("completion for unknown action = %v, want ErrNotFound", err)
	}
}

func TestClaimLeaseCoversDeclaredActionTimeout(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	if err := s.EnqueueAction(ctx, "node-a", "incident-a", types.Action{
		ID: "long-action", Type: types.ActionWaitIdle, Timeout: 12 * time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(12 * time.Hour)
	claimed, err := s.ClaimNextAction(ctx, "node-a", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.LeaseExpiresAt.Before(before) {
		t.Fatalf("lease expiry = %v, want at least declared action timeout after %v", claimed.LeaseExpiresAt, before)
	}
}
