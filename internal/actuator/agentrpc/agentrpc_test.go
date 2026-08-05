package agentrpc

import (
	"context"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestExecuteWaitsForAgentResult(t *testing.T) {
	st := openStore(t)
	a := New(st, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	action := types.Action{ID: "act-1", Type: types.ActionGPUReset, Params: map[string]string{"gpu_index": "0"}}

	// Simulate the node's agent: poll the queue, execute, post the result.
	go func() {
		for {
			queued, err := st.NextPendingAction(ctx, "n1")
			if err == nil {
				_ = st.CompleteAction(ctx, queued.Action.ID, types.ActionResult{
					ActionID: queued.Action.ID, OK: true, Output: "reset done",
				})
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	res, err := a.Execute(ctx, types.Node{Name: "n1"}, action)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.OK || res.Output != "reset done" {
		t.Fatalf("result = %+v", res)
	}
}

func TestExecuteReturnsFailureAsError(t *testing.T) {
	st := openStore(t)
	a := New(st, 0)
	ctx := context.Background()

	action := types.Action{ID: "act-fail", Type: types.ActionGPUReset}
	if err := st.EnqueueAction(ctx, "n1", action); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteAction(ctx, "act-fail", types.ActionResult{
		ActionID: "act-fail", OK: false, Error: "reset refused",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := a.Execute(ctx, types.Node{Name: "n1"}, action)
	if err == nil {
		t.Fatal("failed action must surface as an error")
	}
	if res == nil || res.Error != "reset refused" {
		t.Fatalf("result = %+v", res)
	}
}

func TestExecuteReplayReturnsStoredResult(t *testing.T) {
	st := openStore(t)
	a := New(st, 0)
	ctx := context.Background()

	action := types.Action{ID: "act-replay", Type: types.ActionGPUReset}
	if err := st.EnqueueAction(ctx, "n1", action); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteAction(ctx, "act-replay", types.ActionResult{
		ActionID: "act-replay", OK: true, Output: "already done",
	}); err != nil {
		t.Fatal(err)
	}

	// A controller-restart replay re-attaches instead of re-executing.
	res, err := a.Execute(ctx, types.Node{Name: "n1"}, action)
	if err != nil || res.Output != "already done" {
		t.Fatalf("replay = %+v, %v", res, err)
	}
}

func TestExecuteTimeoutLeavesActionQueued(t *testing.T) {
	st := openStore(t)
	a := New(st, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()

	action := types.Action{ID: "act-slow", Type: types.ActionReboot}
	if _, err := a.Execute(ctx, types.Node{Name: "n1"}, action); err == nil {
		t.Fatal("Execute must fail when no agent picks up the action")
	}
	queued, err := st.GetAction(context.Background(), "act-slow")
	if err != nil || queued.Done {
		t.Fatalf("action must stay queued for replay: %+v, %v", queued, err)
	}
}

func TestHealthyRequiresFreshHeartbeat(t *testing.T) {
	st := openStore(t)
	a := New(st, 90*time.Second)
	ctx := context.Background()

	if err := a.Healthy(ctx, types.Node{Name: "ghost"}); err == nil {
		t.Fatal("unknown node must be unhealthy")
	}

	if err := st.UpsertAgentRegistration(ctx, &types.Node{Name: "n1", AgentLastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := a.Healthy(ctx, types.Node{Name: "n1"}); err != nil {
		t.Fatalf("fresh heartbeat must be healthy: %v", err)
	}

	if err := st.UpsertAgentRegistration(ctx, &types.Node{Name: "n2", AgentLastSeen: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := a.Healthy(ctx, types.Node{Name: "n2"}); err == nil {
		t.Fatal("stale heartbeat must be unhealthy")
	}
}
