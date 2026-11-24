// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	asyncQueueSize   = 256
	asyncSendTimeout = 10 * time.Second
	// Delivery retries: 4 attempts spanning ~21s (1s, 4s, 16s backoff). A
	// transient notifier blip is absorbed; a real outage dead-letters the
	// item to the log with a metric and moves on — the audit trail, not the
	// notification channel, is the system of record.
	asyncMaxAttempts   = 4
	asyncFirstBackoff  = time.Second
	asyncBackoffFactor = 4
)

// Async decouples notification delivery from the caller: Notify and
// RequestApproval enqueue and return immediately, and a single worker
// delivers in order. A slow or down notifier (Slack outage) can then never
// stall signal ingestion or the reconcile walk. When the queue overflows,
// the oldest-style behavior is to drop with a counted log line — the audit
// trail, not Slack, is the system of record.
type Async struct {
	// Channel names this channel in delivery reports (slack, webhook,
	// pagerduty).
	Channel string
	inner   Notifier
	log     *slog.Logger
	queue   chan asyncItem
	// pending holds the approval rounds queued or being retried here, so a
	// request sent again while the first is still in the queue is not
	// queued twice.
	mu      sync.Mutex
	pending map[string]bool
	// sleep is time.Sleep in production; tests inject a recorder. It must
	// remain interruptible by ctx in the worker loop.
	sleep func(ctx context.Context, d time.Duration)
}

type asyncItem struct {
	event    *NotifyEvent
	incident *incidentApproval
}

type incidentApproval struct {
	incident *types.Incident
	stepName string
	context  ApprovalContext
}

// NewAsync wraps inner; Start must be called to begin delivery.
func NewAsync(inner Notifier, log *slog.Logger) *Async {
	return &Async{
		inner: inner, log: log, queue: make(chan asyncItem, asyncQueueSize), pending: map[string]bool{},
		sleep: func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		},
	}
}

// Start runs the delivery worker until ctx is done.
func (a *Async) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case item := <-a.queue:
				a.deliver(ctx, item)
			}
		}
	}()
}

// deliver attempts one queued item with bounded retries. Retrying blocks
// the in-order queue on purpose: during a notifier outage the queue fills,
// new items are dropped with a metric, and this worker keeps the channel's
// ordering guarantees instead of reordering around failures.
func (a *Async) deliver(ctx context.Context, item asyncItem) {
	if item.incident != nil {
		defer a.release(item.incident.incident)
	}
	backoff := asyncFirstBackoff
	var err error
	for attempt := 1; attempt <= asyncMaxAttempts; attempt++ {
		sendCtx, cancel := context.WithTimeout(context.Background(), asyncSendTimeout)
		if item.event != nil {
			err = a.inner.Notify(sendCtx, *item.event)
		} else if item.incident != nil {
			ac := item.incident.context
			ac.Delivered = nil // reported below, once, after the attempt that succeeded
			ac.Sent = nil      // reported when queued
			err = RequestApprovalWithContext(sendCtx, a.inner, item.incident.incident, item.incident.stepName, ac)
		}
		cancel()
		if err == nil {
			if item.incident != nil && item.incident.context.Delivered != nil {
				item.incident.context.Delivered(a.ChannelName())
			}
			return
		}
		// The bundled notifiers already redact; an inner notifier written
		// elsewhere may not, and this line repeats on every retry.
		err = RedactError(err)
		if attempt == asyncMaxAttempts || ctx.Err() != nil {
			break
		}
		a.log.Warn("notification delivery failed; retrying",
			"channel", a.ChannelName(), "attempt", attempt, "backoff", backoff, "err", err)
		a.sleep(ctx, backoff)
		backoff *= asyncBackoffFactor
	}
	// Dead letter: the full payload lands in the log stream so an operator
	// can replay it by hand; the metric drives the alert.
	metrics.NotificationsDropped.WithLabelValues("dead_letter").Inc()
	if item.event != nil {
		a.log.Error("notification dead-lettered after retries",
			"channel", a.ChannelName(), "kind", item.event.Kind, "incident", incidentID(item.event.Incident),
			"message", item.event.Message, "err", err)
		return
	}
	a.log.Error("approval request dead-lettered after retries",
		"channel", a.ChannelName(), "incident", incidentID(item.incident.incident),
		"step", item.incident.stepName, "err", err)
}

// Notify implements Notifier: enqueue and return. The incident is deep-copied
// before it crosses into the delivery goroutine — the reconcile loop keeps
// mutating its own copy (StepIndex, State) after this call returns.
func (a *Async) Notify(_ context.Context, ev NotifyEvent) error {
	ev.Incident = ev.Incident.Clone()
	select {
	case a.queue <- asyncItem{event: &ev}:
	default:
		metrics.NotificationsDropped.WithLabelValues("event").Inc()
		a.log.Warn("notification queue full, dropping", "channel", a.ChannelName(), "kind", ev.Kind, "incident", incidentID(ev.Incident))
	}
	return nil
}

// RequestApproval implements Notifier: enqueue and return. The incident is
// deep-copied for the same reason as in Notify.
func (a *Async) RequestApproval(ctx context.Context, inc *types.Incident, stepName string) error {
	return a.RequestApprovalWithContext(ctx, inc, stepName, ApprovalContext{})
}

var _ ContextualNotifier = (*Async)(nil)

func roundKey(inc *types.Incident) string {
	if inc == nil {
		return ""
	}
	return inc.ID + "#" + strconv.Itoa(inc.ApprovalEpoch)
}

func (a *Async) claim(inc *types.Incident) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := roundKey(inc)
	if key == "" {
		return true
	}
	if a.pending[key] {
		return false
	}
	a.pending[key] = true
	return true
}

func (a *Async) release(inc *types.Incident) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pending, roundKey(inc))
}

// ChannelName implements NamedChannel.
func (a *Async) ChannelName() string {
	if a.Channel != "" {
		return a.Channel
	}
	return channelName(a.inner)
}

// reportsDelivery marks Async as reporting an approval request's delivery
// after its own attempts, not when it is queued.
func (a *Async) reportsDelivery() {}

// RequestApprovalWithContext implements ContextualNotifier: the context rides
// along in the queue item.
func (a *Async) RequestApprovalWithContext(_ context.Context, inc *types.Incident, stepName string, ac ApprovalContext) error {
	if !a.claim(inc) {
		return nil // this round is still queued or being retried here
	}
	select {
	case a.queue <- asyncItem{incident: &incidentApproval{incident: inc.Clone(), stepName: stepName, context: ac}}:
		if ac.Sent != nil {
			ac.Sent(a.ChannelName())
		}
	default:
		a.release(inc)
		metrics.NotificationsDropped.WithLabelValues("approval_request").Inc()
		a.log.Warn("notification queue full, dropping approval request", "channel", a.ChannelName(), "incident", incidentID(inc))
	}
	return nil
}

func incidentID(inc *types.Incident) string {
	if inc == nil {
		return "<none>"
	}
	return inc.ID
}
