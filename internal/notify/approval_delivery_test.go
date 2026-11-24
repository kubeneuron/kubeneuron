// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// deliveries records the channels reported as having accepted a request.
type deliveries struct {
	mu       sync.Mutex
	channels []string
}

func (d *deliveries) context() ApprovalContext {
	return ApprovalContext{Delivered: func(channel string) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.channels = append(d.channels, channel)
	}}
}

func (d *deliveries) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Sorted(slices.Values(d.channels))
}

// namedNotifier is a channel that names itself.
type namedNotifier struct {
	*flakyNotifier
	name string
}

func (n namedNotifier) ChannelName() string { return n.name }

func up(name string) namedNotifier   { return namedNotifier{&flakyNotifier{}, name} }
func down(name string) namedNotifier { return namedNotifier{&flakyNotifier{failures: 1 << 30}, name} }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// Async reports an approval request delivered, under its channel's name,
// after the attempt that succeeded, and never for one it dead-lettered.
func TestAsyncReportsApprovalDeliveryOnlyOnSuccess(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failures  int
		delivered []string
	}{
		{"after two failures", 2, []string{"slack"}},
		{"dead-lettered", asyncMaxAttempts + 10, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &flakyNotifier{failures: tc.failures}
			a, _, _ := asyncWithRecordedSleep(inner)
			a.Channel = "slack"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a.Start(ctx)
			var d deliveries
			_ = a.RequestApprovalWithContext(context.Background(), &types.Incident{ID: "inc-1"}, "reboot", d.context())
			if len(d.got()) != 0 {
				t.Fatal("reported delivered when queued")
			}
			waitFor(t, func() bool { return inner.count() >= min(tc.failures+1, asyncMaxAttempts) })
			time.Sleep(20 * time.Millisecond)
			if got := d.got(); !slices.Equal(got, tc.delivered) {
				t.Fatalf("delivered reported for %v, want %v", got, tc.delivered)
			}
		})
	}
}

// Through Multi, each channel that accepts is reported once under its own
// name; the log is never reported, and a channel in Skip is not sent to.
func TestMultiReportsEachAcceptingChannel(t *testing.T) {
	log := &Log{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, tc := range []struct {
		name      string
		m         Multi
		skip      map[string]bool
		delivered []string
	}{
		{"log only", Multi{log}, nil, nil},
		{"log and a channel that is down", Multi{log, down("webhook")}, nil, nil},
		{"log, one down, one up", Multi{log, down("pagerduty"), up("slack")}, nil, []string{"slack"}},
		{"two channels up", Multi{log, up("slack"), up("webhook")}, nil, []string{"slack", "webhook"}},
		{"accepted channel skipped", Multi{log, up("slack"), up("pagerduty")}, map[string]bool{"slack": true}, []string{"pagerduty"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d deliveries
			ac := d.context()
			ac.Skip = tc.skip
			_ = RequestApprovalWithContext(context.Background(), tc.m, &types.Incident{ID: "inc-1"}, "reboot", ac)
			if got := d.got(); !slices.Equal(got, tc.delivered) {
				t.Fatalf("delivered reported for %v, want %v", got, tc.delivered)
			}
			for _, member := range tc.m {
				if n, ok := member.(namedNotifier); ok && tc.skip[n.name] && n.count() != 0 {
					t.Fatalf("skipped channel %s was sent the request", n.name)
				}
			}
		})
	}
	if got := Channels(Multi{log, up("slack"), Multi{up("webhook")}}); !slices.Equal(got, []string{"slack", "webhook"}) {
		t.Fatalf("Channels = %v", got)
	}
	if got := Channels(log); got != nil {
		t.Fatalf("Channels(log) = %v, want none", got)
	}
}

// A channel behind Async counts when Async delivers it, not when Multi
// queues it.
func TestMultiWaitsForAsyncChannel(t *testing.T) {
	inner := &flakyNotifier{failures: 1}
	a, _, _ := asyncWithRecordedSleep(inner)
	a.Channel = "webhook"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := Multi{&Log{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, a}
	var d deliveries
	_ = RequestApprovalWithContext(context.Background(), m, &types.Incident{ID: "inc-1"}, "reboot", d.context())
	if len(d.got()) != 0 {
		t.Fatal("reported delivered before the channel delivered")
	}
	a.Start(ctx)
	waitFor(t, func() bool { return len(d.got()) == 1 })
	if got := d.got(); !slices.Equal(got, []string{"webhook"}) || inner.count() != 2 {
		t.Fatalf("delivered=%v attempts=%d, want webhook once after one failure", got, inner.count())
	}
}

// An approval request queued behind other notifications while the channel
// is down, and sent again before the queue reached it, reaches the channel
// once: the resend is not queued while the first is still pending. Once
// delivered, a later send of the round is queued again.
func TestAsyncQueuesAPendingRoundOnce(t *testing.T) {
	const queued = 5
	// Every notification ahead of the request fails once, then the channel
	// is back.
	inner := &flakyNotifier{failures: queued}
	a, _, _ := asyncWithRecordedSleep(inner)
	a.Channel = "slack"
	inc := &types.Incident{ID: "inc-1", ApprovalEpoch: 1}
	for range queued {
		_ = a.Notify(context.Background(), NotifyEvent{Kind: EventOpened, Incident: inc})
	}
	var d deliveries
	var handed atomic.Int32
	ac := d.context()
	ac.Sent = func(string) { handed.Add(1) }
	_ = a.RequestApprovalWithContext(context.Background(), inc, "reboot", ac)
	_ = a.RequestApprovalWithContext(context.Background(), inc, "reboot", ac)
	if got := handed.Load(); got != 1 {
		t.Fatalf("reported handed to the channel %d times, want once: the resend found the round queued", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	waitFor(t, func() bool { return len(d.got()) == 1 })
	time.Sleep(20 * time.Millisecond)
	inner.mu.Lock()
	approvals := inner.approvals
	inner.mu.Unlock()
	if got := d.got(); approvals != 1 || !slices.Equal(got, []string{"slack"}) {
		t.Fatalf("channel got %d requests, delivered %v; want exactly one", approvals, got)
	}

	_ = a.RequestApprovalWithContext(context.Background(), inc, "reboot", ac)
	waitFor(t, func() bool { return len(d.got()) == 2 })
	if got := d.got(); len(got) != 2 || handed.Load() != 2 {
		t.Fatalf("a delivered round was not queued again: %v", got)
	}
}

// Every channel's dead letter names the channel, so with several channels
// the log says which one dropped the request.
func TestAsyncDeadLetterNamesTheChannel(t *testing.T) {
	var buf syncBuffer
	inner := &flakyNotifier{failures: 1 << 30}
	a, _, _ := asyncWithRecordedSleep(inner)
	a.log = slog.New(slog.NewJSONHandler(&buf, nil))
	a.Channel = "webhook"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	_ = a.RequestApprovalWithContext(context.Background(), &types.Incident{ID: "inc-1", ApprovalEpoch: 1}, "reboot", ApprovalContext{})
	_ = a.Notify(context.Background(), NotifyEvent{Kind: EventOpened, Incident: &types.Incident{ID: "inc-2"}})

	want := map[string]string{
		"approval request dead-lettered after retries": "inc-1",
		"notification dead-lettered after retries":     "inc-2",
	}
	lines := func() map[string]map[string]any {
		got := map[string]map[string]any{}
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) == nil {
				if msg, _ := rec["msg"].(string); want[msg] != "" {
					got[msg] = rec
				}
			}
		}
		return got
	}
	waitFor(t, func() bool { return len(lines()) == len(want) })
	got := lines()
	for msg, incident := range want {
		rec, ok := got[msg]
		if !ok {
			t.Fatalf("no %q line in %s", msg, buf.String())
		}
		if rec["channel"] != "webhook" || rec["incident"] != incident {
			t.Errorf("%q: channel %v, incident %v; want webhook, %s", msg, rec["channel"], rec["incident"], incident)
		}
	}
}
