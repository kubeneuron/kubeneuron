package spool

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func openTestSpool(t *testing.T, xids ...int) *Spool {
	t.Helper()
	queue, err := Open(t.TempDir() + "/spool.jsonl")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	for _, xid := range xids {
		if err := queue.Append(types.AgentEvent{Node: "node-1", XID: xid}); err != nil {
			t.Fatalf("Append(XID %d) error = %v", xid, err)
		}
	}
	return queue
}

func replayXIDs(t *testing.T, queue *Spool, limit int) []int {
	t.Helper()
	var xids []int
	_, err := queue.ReplayBatch(context.Background(), limit, func(_ context.Context, event types.AgentEvent) error {
		xids = append(xids, event.XID)
		return nil
	})
	if err != nil {
		t.Fatalf("ReplayBatch() error = %v", err)
	}
	return xids
}

func TestReplayBatchIsBoundedAndFIFO(t *testing.T) {
	queue := openTestSpool(t, 1, 2, 3, 4)

	if got, want := replayXIDs(t, queue, 2), []int{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first replay XIDs = %v, want %v", got, want)
	}
	if got, want := replayXIDs(t, queue, 2), []int{3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second replay XIDs = %v, want %v", got, want)
	}
	if got := replayXIDs(t, queue, 2); len(got) != 0 {
		t.Fatalf("empty replay XIDs = %v, want none", got)
	}
}

func TestReplayBatchPreservesFailedEventAndTail(t *testing.T) {
	queue := openTestSpool(t, 10, 20, 30, 40)
	wantErr := errors.New("controller unavailable")
	var attempted []int

	sent, err := queue.ReplayBatch(context.Background(), 3, func(_ context.Context, event types.AgentEvent) error {
		attempted = append(attempted, event.XID)
		if event.XID == 20 {
			return wantErr
		}
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("ReplayBatch() error = %v, want %v", err, wantErr)
	}
	if sent != 1 {
		t.Fatalf("ReplayBatch() sent = %d, want 1", sent)
	}
	if want := []int{10, 20}; !reflect.DeepEqual(attempted, want) {
		t.Fatalf("attempted XIDs = %v, want %v", attempted, want)
	}
	if got, want := replayXIDs(t, queue, 10), []int{20, 30, 40}; !reflect.DeepEqual(got, want) {
		t.Fatalf("replay after failure XIDs = %v, want %v", got, want)
	}
}

func TestReplayBatchCancellationPreservesQueue(t *testing.T) {
	queue := openTestSpool(t, 7, 8)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false

	sent, err := queue.ReplayBatch(ctx, 2, func(_ context.Context, _ types.AgentEvent) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReplayBatch() error = %v, want %v", err, context.Canceled)
	}
	if sent != 0 || called {
		t.Fatalf("ReplayBatch() = (sent %d, callback %t), want (0, false)", sent, called)
	}
	if got, want := replayXIDs(t, queue, 2), []int{7, 8}; !reflect.DeepEqual(got, want) {
		t.Fatalf("replay after cancellation XIDs = %v, want %v", got, want)
	}
}

func TestReplayBatchCancellationCommitsOnlyCompletedPrefix(t *testing.T) {
	queue := openTestSpool(t, 70, 80, 90)
	ctx, cancel := context.WithCancel(context.Background())

	sent, err := queue.ReplayBatch(ctx, 3, func(_ context.Context, event types.AgentEvent) error {
		if event.XID != 70 {
			t.Fatalf("callback XID = %d, want only 70 before cancellation", event.XID)
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReplayBatch() error = %v, want %v", err, context.Canceled)
	}
	if sent != 1 {
		t.Fatalf("ReplayBatch() sent = %d, want 1", sent)
	}
	if got, want := replayXIDs(t, queue, 3), []int{80, 90}; !reflect.DeepEqual(got, want) {
		t.Fatalf("replay after mid-batch cancellation XIDs = %v, want %v", got, want)
	}
}

func TestReplayBatchRejectsInvalidArguments(t *testing.T) {
	queue := openTestSpool(t)
	if _, err := queue.ReplayBatch(context.Background(), 0, func(context.Context, types.AgentEvent) error { return nil }); err == nil {
		t.Fatal("ReplayBatch(limit 0) error = nil, want validation error")
	}
	if _, err := queue.ReplayBatch(context.Background(), 1, nil); err == nil {
		t.Fatal("ReplayBatch(nil send) error = nil, want validation error")
	}
}

func TestSpoolLenTracksAppendsAndReplays(t *testing.T) {
	s, err := Open(t.TempDir() + "/spool.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.Append(types.AgentEvent{EventID: fmt.Sprintf("e%d", i), Node: "n", XID: 79}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Len(); got != 5 {
		t.Fatalf("Len = %d, want 5", got)
	}
	sent, err := s.ReplayBatch(context.Background(), 3, func(context.Context, types.AgentEvent) error { return nil })
	if err != nil || sent != 3 {
		t.Fatalf("ReplayBatch = %d, %v", sent, err)
	}
	if got := s.Len(); got != 2 {
		t.Fatalf("Len after replay = %d, want 2", got)
	}
}

func TestSpoolCountSurvivesReopen(t *testing.T) {
	path := t.TempDir() + "/spool.jsonl"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Append(types.AgentEvent{EventID: fmt.Sprintf("r%d", i), Node: "n", XID: 63}); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Len(); got != 3 {
		t.Fatalf("Len after reopen = %d, want 3", got)
	}
}
