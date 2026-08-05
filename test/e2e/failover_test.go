package e2e

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kubeneuron/kubeneuron/internal/actuator/agentrpc"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/postgres"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// failoverStore is the store surface the failover-replay scenario needs.
type failoverStore interface {
	store.Store
}

// TestFailoverReplayAttachesToTheSameAction proves the Phase 5 exit
// criterion "a controller failover leaves no duplicate action": a deposed
// leader dies while its dispatched action is leased to the node agent, the
// new leader replays the step, and the replay attaches to the SAME queued
// action — the agent executes exactly once.
//
// The SQLite variant always runs; the PostgreSQL variant (the HA store the
// criterion is actually about) runs when KUBENEURON_TEST_POSTGRES_DSN is
// set, exactly like the store conformance suite.
func TestFailoverReplayAttachesToTheSameAction(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		st, err := sqlite.Open(filepath.Join(t.TempDir(), "failover.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		runFailoverReplay(t, st)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("KUBENEURON_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("KUBENEURON_TEST_POSTGRES_DSN not set")
		}
		// Run in a private database: go test runs packages in PARALLEL, and the
		// store conformance suite truncates every table of the shared database
		// between ITS tests — which raced this test's rows out from under it
		// whenever both packages ran with the DSN set.
		dsn = privateDatabase(t, dsn, "kubeneuron_e2e_failover")
		st, err := postgres.Open(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if _, err := st.SQL.Exec(`DELETE FROM actions WHERE id = 'failover-act-1'`); err != nil {
			t.Fatal(err)
		}
		runFailoverReplay(t, st)
	})
}

func runFailoverReplay(t *testing.T, st failoverStore) {
	ctx := context.Background()
	node := types.Node{Name: "failover-node"}
	// Deterministic action identity: exactly what the controller derives
	// from (incident, step, attempt), so a replay after failover reuses it.
	action := types.Action{
		ID: "failover-act-1", Type: types.ActionGPUReset, Timeout: time.Minute,
		Params: map[string]string{"incident_id": "failover-inc-1"},
	}

	// Leader A dispatches: the action is enqueued and A starts waiting.
	leaderA := agentrpc.New(st, time.Minute)
	ctxA, killA := context.WithCancel(ctx)
	aDone := make(chan error, 1)
	go func() {
		_, err := leaderA.Execute(ctxA, node, action)
		aDone <- err
	}()

	// The node agent claims the action (lease + boot binding) and begins
	// the irreversible side effect.
	var claimed *types.QueuedAction
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		claimed, err = st.ClaimNextAction(ctx, node.Name, "boot-1", time.Minute)
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if claimed == nil || claimed.Action.ID != action.ID {
		t.Fatalf("agent claim = %+v", claimed)
	}
	if claimed.Attempts != 1 {
		t.Fatalf("claim attempts = %d, want 1", claimed.Attempts)
	}

	// Leader A dies mid-step (deposed / crashed). Its wait aborts; the
	// leased action survives in the shared store.
	killA()
	if err := <-aDone; err == nil {
		t.Fatal("killed leader's Execute must return an error, not a result")
	}

	// The new leader B replays the step. The idempotent enqueue must
	// re-attach to the SAME action — never dispatch a second one.
	leaderB := agentrpc.New(st, time.Minute)
	bDone := make(chan error, 1)
	go func() {
		res, err := leaderB.Execute(ctx, node, action)
		if err == nil && (res == nil || !res.OK) {
			err = errors.New("leader B got a non-OK result")
		}
		bDone <- err
	}()

	// While the original lease is live, no second copy is claimable: the
	// agent polling for more work sees an empty queue.
	if _, err := st.ClaimNextAction(ctx, node.Name, "boot-1", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second claim during the lease = %v, want ErrNotFound (no duplicate action)", err)
	}

	// The agent finishes the one side effect and posts its result on the
	// original lease and boot identity.
	if err := st.CompleteClaimedAction(ctx, action.ID, claimed.LeaseToken, "boot-1", types.ActionResult{
		ActionID: action.ID, OK: true, Output: "reset once",
	}); err != nil {
		t.Fatal(err)
	}

	// Leader B's replayed step completes from that single execution.
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("leader B replay: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("leader B never observed the completed action")
	}

	// The system of record shows exactly one execution: one claim, one
	// stored result, nothing left in the queue for this node.
	final, err := st.GetAction(ctx, action.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Attempts != 1 {
		t.Fatalf("final attempts = %d, want 1 (the side effect ran once)", final.Attempts)
	}
	if !final.Done || final.Result == nil || final.Result.Output != "reset once" {
		t.Fatalf("final action = %+v, want the single stored result", final)
	}
	if _, err := st.ClaimNextAction(ctx, node.Name, "boot-1", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("post-completion claim = %v, want an empty queue", err)
	}
}

// privateDatabase derives a DSN pointing at a dedicated database (dropped and
// recreated fresh), so this package cannot race the store conformance suite's
// table truncation on the shared database when go test runs both in parallel.
func privateDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	admin, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect for database setup: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name); err != nil {
		t.Fatalf("drop private database: %v", err)
	}
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create private database: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}
