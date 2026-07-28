package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// blockingNotifier simulates a slow/down destination.
type blockingNotifier struct {
	mu       sync.Mutex
	delivery chan struct{}
	events   []NotifyEvent
	fail     bool
}

func (b *blockingNotifier) Notify(_ context.Context, ev NotifyEvent) error {
	if b.delivery != nil {
		<-b.delivery
	}
	b.mu.Lock()
	b.events = append(b.events, ev)
	b.mu.Unlock()
	if b.fail {
		return errors.New("slack down")
	}
	return nil
}

func (b *blockingNotifier) RequestApproval(context.Context, *types.Incident, string) error {
	return nil
}

func (b *blockingNotifier) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

func testEvent() NotifyEvent {
	return NotifyEvent{Kind: EventOpened, Incident: &types.Incident{ID: "inc-1"}}
}

func TestAsyncNeverBlocksCaller(t *testing.T) {
	inner := &blockingNotifier{delivery: make(chan struct{})}
	a := NewAsync(inner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)

	done := make(chan struct{})
	go func() {
		for i := 0; i < asyncQueueSize+10; i++ {
			_ = a.Notify(context.Background(), testEvent())
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked the caller while the inner notifier was stuck")
	}

	// Unblock delivery; queued events drain.
	close(inner.delivery)
	deadline := time.Now().Add(2 * time.Second)
	for inner.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if inner.count() == 0 {
		t.Fatal("no events delivered after unblocking")
	}
}

// The caller keeps mutating its incident after Notify/RequestApproval return
// (the reconcile loop bumps StepIndex/State); the queued copy must be
// isolated. Run with -race: the historical bug was a shared pointer.
func TestAsyncCopiesIncidentBeforeQueueing(t *testing.T) {
	inner := &blockingNotifier{}
	a := NewAsync(inner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)

	inc := &types.Incident{ID: "inc-race", State: types.StateExecuting}
	if err := a.Notify(context.Background(), NotifyEvent{Kind: EventActionTaken, Incident: inc}); err != nil {
		t.Fatal(err)
	}
	if err := a.RequestApproval(context.Background(), inc, "step-1"); err != nil {
		t.Fatal(err)
	}
	// Concurrent caller-side mutation, as the reconcile loop does.
	inc.StepIndex++
	inc.State = types.StateVerifying

	deadline := time.Now().Add(2 * time.Second)
	for inner.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.events) == 0 {
		t.Fatal("event not delivered")
	}
	if got := inner.events[0].Incident; got == inc {
		t.Fatal("queued event shares the caller's incident pointer")
	}
	if inner.events[0].Incident.State != types.StateExecuting {
		t.Fatal("queued copy must keep the state observed at enqueue time")
	}
}

// A nil incident must not panic the enqueue or drop-logging paths.
func TestAsyncNilIncidentIsSafe(t *testing.T) {
	inner := &blockingNotifier{}
	a := NewAsync(inner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Not started: the queue fills, exercising the drop path too.
	for i := 0; i < asyncQueueSize+5; i++ {
		if err := a.Notify(context.Background(), NotifyEvent{Kind: EventOpened}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.RequestApproval(context.Background(), nil, "step"); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncDeliveryErrorIsSwallowed(t *testing.T) {
	inner := &blockingNotifier{fail: true}
	a := NewAsync(inner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)

	if err := a.Notify(context.Background(), testEvent()); err != nil {
		t.Fatalf("async Notify must not surface delivery errors: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for inner.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if inner.count() != 1 {
		t.Fatal("event not delivered")
	}
}

// flakyNotifier fails a fixed number of times, then succeeds.
type flakyNotifier struct {
	mu        sync.Mutex
	failures  int
	attempts  int
	approvals int
}

func (f *flakyNotifier) Notify(context.Context, NotifyEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.attempts <= f.failures {
		return errors.New("transient outage")
	}
	return nil
}

func (f *flakyNotifier) RequestApproval(context.Context, *types.Incident, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	f.approvals++
	if f.attempts <= f.failures {
		return errors.New("transient outage")
	}
	return nil
}

func (f *flakyNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func asyncWithRecordedSleep(inner Notifier) (*Async, *[]time.Duration, *sync.Mutex) {
	a := NewAsync(inner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var mu sync.Mutex
	var slept []time.Duration
	a.sleep = func(_ context.Context, d time.Duration) {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
	}
	return a, &slept, &mu
}

func TestAsyncRetriesTransientFailureWithBackoff(t *testing.T) {
	inner := &flakyNotifier{failures: 2}
	a, slept, mu := asyncWithRecordedSleep(inner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)

	_ = a.Notify(context.Background(), testEvent())
	deadline := time.Now().Add(5 * time.Second)
	for inner.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if inner.count() != 3 {
		t.Fatalf("attempts = %d, want 2 failures then success", inner.count())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*slept) != 2 || (*slept)[0] != asyncFirstBackoff || (*slept)[1] != asyncFirstBackoff*asyncBackoffFactor {
		t.Fatalf("backoffs = %v, want [1s 4s]", *slept)
	}
}

func TestAsyncDeadLettersAfterFinalAttempt(t *testing.T) {
	inner := &flakyNotifier{failures: asyncMaxAttempts + 10}
	a, slept, mu := asyncWithRecordedSleep(inner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)

	// An approval request exercises the second delivery path.
	_ = a.RequestApproval(context.Background(), &types.Incident{ID: "inc-1"}, "reboot")
	deadline := time.Now().Add(5 * time.Second)
	for inner.count() < asyncMaxAttempts && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // give a would-be 5th attempt a chance
	if inner.count() != asyncMaxAttempts {
		t.Fatalf("attempts = %d, want exactly %d before dead-lettering", inner.count(), asyncMaxAttempts)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*slept) != asyncMaxAttempts-1 {
		t.Fatalf("backoff sleeps = %d, want %d", len(*slept), asyncMaxAttempts-1)
	}
}
