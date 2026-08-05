package spool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

// TestOpenRepairsTornTailSoNextAppendSurvives is the regression test for the
// torn-tail corruption bug. A power loss mid-Append can leave a newline-less
// partial final line. Without repair, the next Append O_APPENDs its JSON onto
// that fragment, producing one unparseable record that replay silently drops —
// destroying the fresh event while reporting it durable. Open must truncate the
// uncommitted fragment so the next appended event survives replay intact.
func TestOpenRepairsTornTailSoNextAppendSurvives(t *testing.T) {
	path := t.TempDir() + "/spool.jsonl"

	// One committed event (with its fsynced newline) followed by a torn tail:
	// a JSON fragment with no trailing newline, as a crash mid-Append leaves it.
	committed, err := json.Marshal(types.AgentEvent{EventID: "committed", Node: "node-1", XID: 48})
	if err != nil {
		t.Fatal(err)
	}
	disk := append(committed, '\n')
	disk = append(disk, []byte(`{"event_id":"torn","node":"node-1","xi`)...) // no newline
	if err := os.WriteFile(path, disk, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	// The uncommitted fragment is dropped; only the committed event remains.
	if got := s.Len(); got != 1 {
		t.Fatalf("Len after torn-tail repair = %d, want 1", got)
	}

	// A fresh event appended after recovery must be durable and parseable, not
	// glued onto the partial line.
	if err := s.Append(types.AgentEvent{EventID: "fresh", Node: "node-1", XID: 79}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	var ids []string
	if _, err := s.ReplayBatch(context.Background(), 10, func(_ context.Context, ev types.AgentEvent) error {
		ids = append(ids, ev.EventID)
		return nil
	}); err != nil {
		t.Fatalf("ReplayBatch() error = %v", err)
	}
	if want := []string{"committed", "fresh"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("replayed events = %v, want %v (fresh event must survive the torn tail)", ids, want)
	}
}

// TestOpenTruncatesAWhollyUncommittedFile covers a torn tail with no committed
// newline anywhere: the entire file is one uncommitted record and must be
// dropped, leaving an empty, appendable spool.
func TestOpenTruncatesAWhollyUncommittedFile(t *testing.T) {
	path := t.TempDir() + "/spool.jsonl"
	if err := os.WriteFile(path, []byte(`{"event_id":"torn","node":"n"`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got := s.Len(); got != 0 {
		t.Fatalf("Len after repair = %d, want 0", got)
	}
	if err := s.Append(types.AgentEvent{EventID: "fresh", Node: "n", XID: 79}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if got := replayXIDs(t, s, 10); !reflect.DeepEqual(got, []int{79}) {
		t.Fatalf("replayed XIDs = %v, want [79]", got)
	}
}

// TestAppendSelfHealsTornTailFromFailedInProcessAppend is the regression test
// for the in-process torn-tail loss. When an Append fails after a partial write
// (a short write on ENOSPC/EIO — exactly the regime the spool exists for) the
// tail is left without its committing newline while the process keeps running.
// Without self-healing, the NEXT Append O_APPENDs its JSON onto the fragment,
// fsyncs, and reports the fresh event durable — but on replay the glued line is
// one unparseable record that is skipped, silently losing BOTH the torn event
// and the fresh one. The healing Append must repair the tail first so the fresh
// event survives.
func TestAppendSelfHealsTornTailFromFailedInProcessAppend(t *testing.T) {
	path := t.TempDir() + "/spool.jsonl"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	// A first event committed normally (record + fsynced newline).
	if err := s.Append(types.AgentEvent{EventID: "A", Node: "n", XID: 48}); err != nil {
		t.Fatalf("Append(A) error = %v", err)
	}

	// Simulate an Append that failed after a partial write: a newline-less JSON
	// fragment is on disk and the spool was marked dirty, but count was never
	// incremented (the increment only happens after a successful fsync).
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(`{"event_id":"torn","node":"n","xi`)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()

	// The next Append must heal the torn tail before writing, not glue onto it.
	if err := s.Append(types.AgentEvent{EventID: "B", Node: "n", XID: 79}); err != nil {
		t.Fatalf("Append(B) error = %v", err)
	}
	if got := s.Len(); got != 2 {
		t.Fatalf("Len after heal = %d, want 2 (A and B, torn fragment dropped)", got)
	}

	var ids []string
	if _, err := s.ReplayBatch(context.Background(), 10, func(_ context.Context, ev types.AgentEvent) error {
		ids = append(ids, ev.EventID)
		return nil
	}); err != nil {
		t.Fatalf("ReplayBatch() error = %v", err)
	}
	if want := []string{"A", "B"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("replayed events = %v, want %v (fresh event must not be lost to a torn tail)", ids, want)
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
