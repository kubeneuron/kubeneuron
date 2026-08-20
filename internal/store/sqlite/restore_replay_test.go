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
