package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// TestDiscardCompletedActionEndsTheReplay covers the janitor's host restore,
// whose action ID is derived from the node name alone.
//
// Enqueue is idempotent on that ID and completed rows live for the retention
// window — 90 days by default — so a second restore on the same node answered
// from the first one's stored result with no agent involved at all.
//
// Both directions were harmful. A restore that failed once returned that
// failure to every later janitor pass in zero milliseconds, and the
// stuck-restore report fires once per node, so the machine's persistence
// daemon stayed stopped permanently and nobody was told twice. A restore that
// SUCCEEDED was worse: the next time that node was quiesced and abandoned, the
// janitor replayed the old success and cleared the durable marker on the
// strength of it, leaving the node's GPU monitoring off with nothing left to
// retry.
func TestDiscardCompletedActionEndsTheReplay(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	const id = "restore-node-a"

	enqueue := func() {
		if err := s.EnqueueAction(ctx, "node-a", types.Action{
			ID: id, Type: types.ActionRestoreAcceleratorHost, Timeout: time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
	}
	finish := func(output string, ok bool) {
		claimed, err := s.ClaimNextAction(ctx, "node-a", "boot-1", time.Minute)
		if err != nil {
			t.Fatalf("claiming: %v", err)
		}
		if claimed == nil {
			t.Fatal("nothing was dispatched to claim, which is the defect itself")
		}
		if err := s.CompleteClaimedAction(ctx, id, claimed.LeaseToken, "boot-1",
			types.ActionResult{ActionID: id, OK: ok, Output: output}); err != nil {
			t.Fatal(err)
		}
	}

	// Episode one: the restore fails.
	enqueue()
	finish("attempt 1 failed", false)

	// Episode two without discarding: the queue answers from history.
	enqueue()
	got, err := s.GetAction(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Done || got.Result == nil || got.Result.OK {
		t.Fatalf("the replay was not reproduced, so this test proves nothing: %+v", got)
	}

	// With the discard, episode two really dispatches.
	if err := s.DiscardCompletedAction(ctx, id); err != nil {
		t.Fatal(err)
	}
	enqueue()
	got, err = s.GetAction(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Done {
		t.Fatal("the second restore attempt still read the first attempt's stored result; " +
			"one transient failure would leave this node's GPU monitoring off permanently")
	}
	finish("attempt 2 succeeded", true)
}

// TestDiscardCompletedActionLeavesLiveRowsAlone: a pending or leased action
// belongs to an agent that may be executing it right now. Removing it would
// strand that work and allow a second copy to be dispatched beside it.
func TestDiscardCompletedActionLeavesLiveRowsAlone(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	const id = "restore-node-b"

	if err := s.EnqueueAction(ctx, "node-b", types.Action{
		ID: id, Type: types.ActionRestoreAcceleratorHost, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DiscardCompletedAction(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAction(ctx, id)
	if err != nil {
		t.Fatalf("a pending action was deleted: %v", err)
	}
	if got.Done {
		t.Fatal("a pending action was reported done")
	}

	claimed, err := s.ClaimNextAction(ctx, "node-b", "boot-1", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("the pending action could not be claimed after the discard: %v", err)
	}
	if err := s.DiscardCompletedAction(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAction(ctx, id); err != nil {
		t.Fatalf("a LEASED action was deleted out from under its agent: %v", err)
	}
}

// TestDiscardCompletedActionClearsADeadLetteredRow covers the third terminal
// state, which the first version of the discard did not know about.
//
// The actions queue dead-letters a row after MaxActionAttempts claims. An agent
// that crash-loops — or that is restarted by its own ladder's reboot rung
// mid-action — burns those attempts on the janitor's deterministic restore ID.
// From then on the discard matched nothing, the enqueue conflicted away, and
// agentrpc polled a row no agent could ever claim until the caller's deadline.
//
// The janitor's restore budget is shared across every quiesced node, so that
// one node consumed it on every reconcile tick and starved the rest, and the
// condition self-healed only when retention dropped the row — ninety days by
// default. Exactly the harm the discard was added to prevent, through a
// different door.
func TestDiscardCompletedActionClearsADeadLetteredRow(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	const id = "restore-node-c"

	if err := s.EnqueueAction(ctx, "node-c", types.Action{
		ID: id, Type: types.ActionRestoreAcceleratorHost, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	// Burn the attempt budget the way a crash-looping agent does: claim, never
	// complete, let the lease expire, claim again.
	for i := 0; i < 12; i++ {
		claimed, err := s.ClaimNextAction(ctx, "node-c", "boot-1", time.Minute)
		if err != nil || claimed == nil {
			break
		}
		if _, err := s.sqlDB.ExecContext(ctx,
			`UPDATE actions SET lease_expires_at_ns=? WHERE id=?`,
			time.Now().Add(-time.Second).UnixNano(), id); err != nil {
			t.Fatal(err)
		}
	}

	var state string
	if err := s.sqlDB.QueryRowContext(ctx, `SELECT state FROM actions WHERE id=?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "dead" {
		t.Skipf("the attempt budget did not dead-letter the row (state=%q); this test needs that state", state)
	}

	if err := s.DiscardCompletedAction(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueAction(ctx, "node-c", types.Action{
		ID: id, Type: types.ActionRestoreAcceleratorHost, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextAction(ctx, "node-c", "boot-2", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("a dead-lettered restore could not be re-dispatched (%v); this node's GPU "+
			"monitoring would stay off for the whole retention window while consuming the "+
			"janitor's shared budget on every tick", err)
	}
}
