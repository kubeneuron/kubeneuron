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
		// Not a skip. This test's whole subject is the dead-lettered row, and
		// skipping when the setup does not produce one is how the SQL half of
		// the guard went unnoticed for four rounds: the run stayed green while
		// covering nothing. TestDiscardRemovesEveryTerminalActionState covers
		// the states directly; this one covers the PATH that reaches them, so
		// if that path stops dead-lettering, that is the finding.
		t.Fatalf("burning the claim-attempt budget left the row in state %q, not \"dead\"; a "+
			"crash-looping agent no longer dead-letters its action, so the queue has no "+
			"terminal state for work that can never succeed", state)
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

// TestDiscardRemovesEveryTerminalActionState is the SQL half of the guard, and
// it is here because the half above turned out not to cover it.
//
// QueuedAction.Terminal() and terminalActionStates are two halves of one rule.
// Cutting the SQL constant down to ('done') leaves the test above PASSING —
// it asks the Go predicate — and leaves the replay test passing too, because
// that one only ever uses "done". The single test that did reach 'dead' got
// there by burning the claim-attempt budget and t.Skip'd when it did not, and
// a skipped test guards nothing. So the SQL half of the defect class that
// recurred for nine consecutive rounds was unguarded on both engines.
//
// Set the state directly: no attempt budgets, nothing to skip.
func TestDiscardRemovesEveryTerminalActionState(t *testing.T) {
	for _, state := range []string{"done", "dead", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s := openLeaseTestStore(t)
			ctx := context.Background()
			const id = "restore-node-x"

			if err := s.EnqueueAction(ctx, "node-x", types.Action{
				ID: id, Type: types.ActionRestoreAcceleratorHost, Timeout: time.Minute,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.sqlDB.ExecContext(ctx,
				`UPDATE actions SET state=?, lease_token='', lease_expires_at_ns=0 WHERE id=?`,
				state, id); err != nil {
				t.Fatal(err)
			}

			if err := s.DiscardCompletedAction(ctx, id); err != nil {
				t.Fatal(err)
			}

			var rows int
			if err := s.sqlDB.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM actions WHERE id=?`, id).Scan(&rows); err != nil {
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

// TestTerminalCoversEveryTerminalState is the guard against the defect class
// that recurred in eight consecutive review rounds: a terminal-state set
// spelled out inline at a new site instead of asked of one shared predicate.
//
// The queue terminalises three ways. The prune query knew three; the discard
// knew two until it was fixed; the janitor's re-dispatch probe, added after
// that, knew two DIFFERENT ones — so a dead-lettered restore read back as
// "still in flight" and the probe handed back its wedged ID forever, leaving a
// node's GPU monitoring off for the retention window while consuming the
// janitor's shared budget on every tick.
//
// Driven from the STATE STRINGS on purpose. A fourth terminal state added to
// the schema without teaching QueuedAction.Terminal() about it fails here,
// which is the only way this stops happening.
func TestTerminalCoversEveryTerminalState(t *testing.T) {
	for _, state := range []string{"done", "dead", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s := openLeaseTestStore(t)
			ctx := context.Background()
			const id = "act-terminal"
			if err := s.EnqueueAction(ctx, "node-t", types.Action{
				ID: id, Type: types.ActionRestoreAcceleratorHost, Timeout: time.Minute,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.sqlDB.ExecContext(ctx,
				`UPDATE actions SET state=?, lease_token='', lease_expires_at_ns=0 WHERE id=?`,
				state, id); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetAction(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Terminal() {
				t.Fatalf("an action in state %q reports Terminal()=false; every caller that asks "+
					"whether it can still make progress is told to keep waiting for something "+
					"that will never happen", state)
			}
		})
	}
}

// TestNonTerminalStatesAreNotTerminal: the predicate is only useful if it also
// says no. A pending or leased action belongs to an agent that may be running
// it right now.
func TestNonTerminalStatesAreNotTerminal(t *testing.T) {
	s := openLeaseTestStore(t)
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
		t.Fatal("a pending action reports Terminal()=true; the janitor would mint a second " +
			"restore beside the one an agent is about to claim")
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

// TestPruneRemovesEveryTerminalOutboxState is the event-outbox half of the
// guard that round 21 built for the action queue — and it is here because the
// class recurred one queue over within a day.
//
// The outbox terminalises two ways: delivered, and dead-lettered after
// MaxEventAttempts. The prune knew one. Beyond accumulation, the events delete
// spares any row still referenced by the outbox, so a dead outbox row pins its
// raw kernel fault text past the configured retention permanently.
//
// Driven from the STATE STRINGS, like its sibling: a third terminal state added
// without teaching the prune fails here.
func TestPruneRemovesEveryTerminalOutboxState(t *testing.T) {
	for _, state := range []string{"done", "dead"} {
		t.Run(state, func(t *testing.T) {
			s := openLeaseTestStore(t)
			ctx := context.Background()
			old := time.Now().Add(-90 * 24 * time.Hour)

			stamp := old.UTC().Format(time.RFC3339Nano)
			if _, err := s.sqlDB.ExecContext(ctx,
				`INSERT INTO events (id, node, xid, raw, timestamp) VALUES (1, 'n1', 79, 'Xid 79', ?)`,
				stamp); err != nil {
				t.Fatal(err)
			}
			if _, err := s.sqlDB.ExecContext(ctx,
				`INSERT INTO event_outbox (event_row_id, state, attempts, created_at, updated_at)
				 VALUES (1, ?, 0, ?, ?)`, state, stamp, stamp); err != nil {
				t.Fatal(err)
			}

			if _, err := s.Prune(ctx, 24*time.Hour, 0); err != nil {
				t.Fatal(err)
			}

			var outbox, events int
			if err := s.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_outbox`).Scan(&outbox); err != nil {
				t.Fatal(err)
			}
			if err := s.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if outbox != 0 {
				t.Fatalf("a %q outbox row survived the prune", state)
			}
			if events != 0 {
				t.Fatalf("a %q outbox row pinned its raw event past retention; kernel fault text "+
					"that was promised to age out does not", state)
			}
		})
	}
}

// TestATimingOutActionCanStillReportWhyItTimedOut covers a result that was
// unreportable by construction.
//
// The lease used to end exactly at the action's own deadline. The executor
// cancels the work at T and POSTs the reason a moment later, and the
// completion predicate requires lease_expires_at_ns > now — so the ONE result
// worth having was the one guaranteed to be refused. The controller saw a bare
// timeout with no cause, and the row read as reclaimable, so the action could
// be handed out again.
//
// Driven with the shipped WaitIdle rung's 12h budget, because that is where it
// costs most: the reason is "GPU 0 still has 3 processes on it", the controller
// never hears it, and the redispatch is another twelve hours of a card held out
// of service.
func TestATimingOutActionCanStillReportWhyItTimedOut(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	const id = "act-waitidle"
	const budget = 12 * time.Hour

	if err := s.EnqueueAction(ctx, "node-a", types.Action{
		ID: id, Type: types.ActionWaitIdle, Timeout: budget,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextAction(ctx, "node-a", "boot-1", budget)
	if err != nil || claimed == nil {
		t.Fatalf("claiming: %v", err)
	}

	// The lease must survive the action's own deadline, or the agent has
	// nowhere to put the answer.
	var leaseNS int64
	if err := s.sqlDB.QueryRowContext(ctx,
		`SELECT lease_expires_at_ns FROM actions WHERE id=?`, id).Scan(&leaseNS); err != nil {
		t.Fatal(err)
	}
	remaining := time.Until(time.Unix(0, leaseNS))
	if remaining <= budget {
		t.Fatalf("the lease expires %s from now, at or before the action's own %s deadline; a "+
			"result POSTed the moment the work is cancelled is refused, so the controller "+
			"gets a timeout with no cause and the action can be dispatched again",
			remaining.Round(time.Second), budget)
	}

	// And the reason really is accepted at that boundary: wind the lease back
	// to exactly the deadline plus the grace, as it will be in production, and
	// complete.
	if _, err := s.sqlDB.ExecContext(ctx,
		`UPDATE actions SET lease_expires_at_ns=? WHERE id=?`,
		time.Now().Add(types.AgentResultGrace).UnixNano(), id); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteClaimedAction(ctx, id, claimed.LeaseToken, "boot-1",
		types.ActionResult{
			ActionID: id, OK: false,
			Output: "GPU-a still has 3 compute processes after 12h",
		}); err != nil {
		t.Fatalf("the agent could not report why the action timed out: %v", err)
	}

	got, err := s.GetAction(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.Output == "" {
		t.Fatal("the action completed with no reason recorded; the controller has a timeout and " +
			"nothing to tell an operator about it")
	}
}

// TestANodeScopedSignalDoesNotJoinADeviceIncident covers a seam between two
// changes: adding a bus address to the target, and the lookup that decides
// which incident a signal belongs to.
//
// The node-scoped branch constrained gpu_uuid alone — and every PCI-only
// incident has an empty gpu_uuid too. So a node-level alert or a manual trigger
// joined the oldest same-class DEVICE incident: the node's fault advanced a
// ladder aimed at one card, up to and including resetting it, while the
// node-scoped problem never opened an incident of its own.
func TestANodeScopedSignalDoesNotJoinADeviceIncident(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()

	device := &types.Incident{
		ID: "inc-device", Class: types.ClassECCDBE, State: types.StateOpen,
		Target:   types.Target{Node: "n1", PCIAddr: "0000:3b:00"},
		OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
	}
	if err := s.CreateIncident(ctx, device); err != nil {
		t.Fatal(err)
	}

	// A node-scoped signal: no UUID, no bus address.
	got, err := s.GetOpenIncident(ctx, types.Target{Node: "n1"}, types.ClassECCDBE)
	if err == nil && got != nil {
		t.Fatalf("a node-scoped signal joined %q, an incident about the card at %s; the node's "+
			"fault would drive that card's ladder and could reset it, and the node-scoped "+
			"problem would never open an incident of its own",
			got.ID, got.Target.PCIAddr)
	}

	// And a node-scoped incident is still found by a node-scoped signal.
	nodeWide := &types.Incident{
		ID: "inc-node", Class: types.ClassECCDBE, State: types.StateOpen,
		Target:   types.Target{Node: "n1"},
		OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
	}
	if err := s.CreateIncident(ctx, nodeWide); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetOpenIncident(ctx, types.Target{Node: "n1"}, types.ClassECCDBE)
	if err != nil || got == nil || got.ID != "inc-node" {
		t.Fatalf("a node-scoped signal did not find its own node-scoped incident: %v %v", got, err)
	}
}
