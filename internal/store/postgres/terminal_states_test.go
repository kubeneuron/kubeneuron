package postgres

// The terminal-state guards, run against a live PostgreSQL.
//
// Rounds 21-25 fixed one defect class — a set of terminal states spelled out
// inline at a new call site instead of asked of one shared predicate — and
// proved every fix on SQLite only. The SQL that implements it lives in
// sqlcore, shared by both engines, so the fixes SHOULD hold here unchanged.
// "Should" is the word that made these worth writing: the two engines have
// separate migration sets (sqlite 0019, postgres 0010), placeholders are
// rebound per dialect, and a state string present in one schema and absent
// from the other would make the shared constant silently match nothing.
//
// These are deliberate duplicates of the sqlite tests, driven from the same
// state STRINGS, so that a fourth terminal state added to either schema fails
// on both engines rather than on whichever one somebody remembered.

import (
	"context"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// TestTerminalCoversEveryTerminalStatePG is the postgres half of the guard.
// An action in a terminal state that reports Terminal()=false tells every
// caller asking "can this still make progress" to keep waiting for something
// that will never happen — for the janitor's restore, that is a node whose GPU
// monitoring stays off for the whole retention window while it consumes the
// shared restore budget on every reconcile tick.
func TestTerminalCoversEveryTerminalStatePG(t *testing.T) {
	for _, state := range []string{"done", "dead", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			const id = "act-terminal"

			if err := s.EnqueueAction(ctx, "node-t", types.Action{
				ID: id, Type: types.ActionRestoreAcceleratorHost, Timeout: time.Minute,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SQL.ExecContext(ctx,
				`UPDATE actions SET state=$1, lease_token='', lease_expires_at_ns=0 WHERE id=$2`,
				state, id); err != nil {
				t.Fatal(err)
			}

			got, err := s.GetAction(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Terminal() {
				t.Fatalf("on postgres an action in state %q reports Terminal()=false; every "+
					"caller that asks whether it can still make progress is told to keep "+
					"waiting for something that will never happen", state)
			}
		})
	}
}

// TestNonTerminalStatesAreNotTerminalPG: the predicate is only useful if it
// also says no. A pending or leased action belongs to an agent that may be
// executing it at this moment, and calling it terminal mints a second copy
// beside the one already running — on a node whose GPU is being reset.
func TestNonTerminalStatesAreNotTerminalPG(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const id = "act-live"

	if err := s.EnqueueAction(ctx, "node-l", types.Action{
		ID: id, Type: types.ActionRestoreAcceleratorHost, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAction(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Terminal() {
		t.Fatal("a pending action reports Terminal()=true")
	}
	if _, err := s.ClaimNextAction(ctx, "node-l", "boot-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetAction(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got.Terminal() {
		t.Fatal("a LEASED action reports Terminal()=true; a second copy would be dispatched " +
			"beside the one the agent is executing")
	}
}

// TestDiscardCompletedActionEndsTheReplayPG covers the janitor's host restore,
// whose action ID is derived from the node name alone. Enqueue is idempotent on
// that ID and completed rows live for the retention window, so without the
// discard a second restore on the same node is answered from the first one's
// stored result with no agent involved at all — replaying an old success clears
// the durable marker and leaves that node's GPU monitoring off with nothing
// left to retry.
func TestDiscardCompletedActionEndsTheReplayPG(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const id = "restore-node-a"

	enqueue := func() {
		if err := s.EnqueueAction(ctx, "node-a", types.Action{
			ID: id, Type: types.ActionRestoreAcceleratorHost, Timeout: time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
	}

	enqueue()
	claimed, err := s.ClaimNextAction(ctx, "node-a", "boot-1", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("nothing was dispatched to claim: %v", err)
	}
	if err := s.CompleteClaimedAction(ctx, id, claimed.LeaseToken, "boot-1",
		types.ActionResult{ActionID: id, OK: false, Output: "attempt 1 failed"}); err != nil {
		t.Fatal(err)
	}

	// Without the discard the queue answers from history: reproduce that first,
	// so a passing test cannot mean "the replay never happened here".
	enqueue()
	got, err := s.GetAction(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Done || got.Result == nil || got.Result.OK {
		t.Fatalf("the replay was not reproduced on postgres, so this test proves nothing: %+v", got)
	}

	if err := s.DiscardCompletedAction(ctx, id); err != nil {
		t.Fatal(err)
	}
	enqueue()
	if got, err = s.GetAction(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got.Done {
		t.Fatal("the second restore attempt still read the first attempt's stored result; one " +
			"transient failure would leave this node's GPU monitoring off permanently")
	}
}

// TestDiscardCompletedActionLeavesLiveRowsAlonePG: a pending or leased row
// belongs to an agent that may be executing it right now. Deleting it strands
// that work and lets a second copy be dispatched beside it.
func TestDiscardCompletedActionLeavesLiveRowsAlonePG(t *testing.T) {
	s := openTestStore(t)
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
	if _, err := s.GetAction(ctx, id); err != nil {
		t.Fatalf("a pending action was deleted: %v", err)
	}

	if _, err := s.ClaimNextAction(ctx, "node-b", "boot-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.DiscardCompletedAction(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAction(ctx, id); err != nil {
		t.Fatalf("a LEASED action was deleted out from under its agent: %v", err)
	}
}

// TestDiscardRemovesEveryTerminalActionState is the guard that was missing.
//
// The Go predicate and the SQL constant are two halves of one rule, and until
// this test only the Go half was driven from the state strings. Cutting
// terminalActionStates down to ('done') left TestTerminalCoversEveryTerminalState
// and the replay test both PASSING on either engine: the first asks
// QueuedAction.Terminal(), which is Go-side, and the second only ever uses
// "done". The one test that did cover 'dead' reached that state by burning the
// claim-attempt budget and t.Skip'd when it did not get there — a skipped test
// guards nothing.
//
// So the SQL half of the exact defect class that recurred for nine rounds was
// unguarded. Set each state directly and assert the discard removes it: no
// dependence on attempt budgets, nothing to skip.
func TestDiscardRemovesEveryTerminalActionState(t *testing.T) {
	for _, state := range []string{"done", "dead", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			const id = "restore-node-x"

			if err := s.EnqueueAction(ctx, "node-x", types.Action{
				ID: id, Type: types.ActionRestoreAcceleratorHost, Timeout: time.Minute,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SQL.ExecContext(ctx,
				`UPDATE actions SET state=$1, lease_token='', lease_expires_at_ns=0 WHERE id=$2`,
				state, id); err != nil {
				t.Fatal(err)
			}

			if err := s.DiscardCompletedAction(ctx, id); err != nil {
				t.Fatal(err)
			}

			var rows int
			if err := s.SQL.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM actions WHERE id=$1`, id).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Fatalf("a %q action survived the discard; the janitor's next restore on this "+
					"node conflicts away against a row no agent can ever claim, so the node's "+
					"GPU monitoring stays off for the whole retention window while consuming "+
					"the janitor's shared restore budget on every reconcile tick", state)
			}
		})
	}
}

// TestPruneRemovesEveryTerminalOutboxStatePG is the event-outbox half. Beyond
// accumulation, the events delete spares any row still referenced by the
// outbox, so a dead-lettered outbox row pins its raw kernel fault text past the
// configured retention permanently — text that was promised to age out.
func TestPruneRemovesEveryTerminalOutboxStatePG(t *testing.T) {
	for _, state := range []string{"done", "dead"} {
		t.Run(state, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			stamp := time.Now().Add(-90 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)

			if _, err := s.SQL.ExecContext(ctx,
				`INSERT INTO events (id, node, xid, raw, timestamp) VALUES (1, 'n1', 79, 'Xid 79', $1)`,
				stamp); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SQL.ExecContext(ctx,
				`INSERT INTO event_outbox (event_row_id, state, attempts, created_at, updated_at)
				 VALUES (1, $1, 0, $2, $3)`, state, stamp, stamp); err != nil {
				t.Fatal(err)
			}

			if _, err := s.Prune(ctx, 24*time.Hour, 0); err != nil {
				t.Fatal(err)
			}

			var outbox, events int
			if err := s.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_outbox`).Scan(&outbox); err != nil {
				t.Fatal(err)
			}
			if err := s.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if outbox != 0 {
				t.Fatalf("a %q outbox row survived the prune on postgres", state)
			}
			if events != 0 {
				t.Fatalf("a %q outbox row pinned its raw event past retention on postgres; "+
					"kernel fault text that was promised to age out does not", state)
			}
		})
	}
}
