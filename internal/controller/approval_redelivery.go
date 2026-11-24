// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/impact"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// An approval request that a configured channel has not accepted is sent to
// that channel again while its round waits for a decision. The pauses grow to
// a ceiling; the first is longer than the delivery queue's own retry budget
// (about 21 s). A channel that still holds the round in its queue does not
// queue it twice.
var approvalRedeliveryPauses = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute}

// approvalRedelivery is the in-memory schedule of those sends, by incident.
// Which channels accepted a round is durable (the store's approval_notified);
// only when to try next lives here, so after a restart a round some channel
// has not accepted is sent on the first pass.
type approvalRedelivery struct {
	mu    sync.Mutex
	slots map[string]*redeliverySlot
}

type redeliverySlot struct {
	epoch    int
	attempt  int
	at       time.Time
	deadline time.Time
	accepted map[string]bool
}

func jitter(d time.Duration) time.Duration {
	return d + time.Duration((rand.Float64()*0.2-0.1)*float64(d))
}

func (r *approvalRedelivery) slot(id string, epoch int, deadline time.Time) *redeliverySlot {
	if r.slots == nil {
		r.slots = map[string]*redeliverySlot{}
	}
	s := r.slots[id]
	if s == nil || s.epoch != epoch {
		s = &redeliverySlot{epoch: epoch, deadline: deadline, accepted: map[string]bool{}}
		r.slots[id] = s
	}
	return s
}

// sent records the request of round epoch as just sent, so the next send is
// due only after the first pause.
func (r *approvalRedelivery) sent(id string, epoch int, deadline, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slot(id, epoch, deadline).at = now.Add(jitter(approvalRedeliveryPauses[0]))
}

// again makes the round due on the next pass, undoing the resend due just
// counted, when that resend could not be built.
func (r *approvalRedelivery) again(id string, epoch int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.slots[id]; s != nil && s.epoch == epoch {
		s.attempt--
		s.at = time.Time{}
	}
}

func (r *approvalRedelivery) accept(id string, epoch int, channel string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.slots[id]; s != nil && s.epoch == epoch {
		s.accepted[channel] = true
	}
}

// due returns the channels the round should be sent to now, out of want,
// and which resend this is; nil when nothing is due. stored are the channels
// the store recorded as having accepted. A round this process has no record
// of is due at once.
func (r *approvalRedelivery) due(id string, epoch int, deadline, now time.Time, want []string, stored map[string]bool) (map[string]bool, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for other, s := range r.slots {
		if !now.Before(s.deadline) {
			delete(r.slots, other)
		}
	}
	s := r.slot(id, epoch, deadline)
	for ch := range stored {
		s.accepted[ch] = true
	}
	missing := false
	for _, ch := range want {
		if !s.accepted[ch] {
			missing = true
		}
	}
	if !missing || now.Before(s.at) {
		return nil, 0
	}
	s.attempt++
	s.at = now.Add(jitter(approvalRedeliveryPauses[min(s.attempt, len(approvalRedeliveryPauses)-1)]))
	skip := make(map[string]bool, len(s.accepted))
	for ch := range s.accepted {
		skip[ch] = true
	}
	return skip, s.attempt
}

// approvalDelivered returns the callback a channel calls once it accepted the
// request of round epoch. It runs on the channel's delivery goroutine.
func (c *Controller) approvalDelivered(id string, epoch int) func(string) {
	marker, ok := c.store.(store.ApprovalNotificationStore)
	if !ok {
		return nil
	}
	return func(channel string) {
		c.approvalRetry.accept(id, epoch, channel)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := marker.MarkApprovalNotified(ctx, id, epoch, channel); err != nil {
			// The channel gets the request again after this process
			// restarts; a duplicate request is the cost, not a lost one.
			c.log.Warn("could not record that an approval request was delivered",
				"incident", id, "round", epoch, "channel", channel, "err", err)
		}
	}
}

// redeliverApprovalRequest sends the request of the incident's current round
// again to the configured channels that have not accepted it, when the next
// send is due. It is called only for a round that is still undecided and not
// expired.
func (c *Controller) redeliverApprovalRequest(ctx context.Context, inc *types.Incident, deadline time.Time) {
	channels := notify.Channels(c.notifier)
	if len(channels) == 0 || inc.ApprovalEpoch <= 0 {
		return
	}
	delivered := c.approvalDelivered(inc.ID, inc.ApprovalEpoch)
	if delivered == nil {
		return
	}
	skip, attempt := c.approvalRetry.due(inc.ID, inc.ApprovalEpoch, deadline, time.Now(), channels, inc.ApprovalNotified.Accepted(inc.ApprovalEpoch))
	if skip == nil {
		return
	}
	request, err := c.store.GetApprovalRequest(ctx, inc.ID, inc.ApprovalEpoch)
	if err != nil {
		c.approvalRetry.again(inc.ID, inc.ApprovalEpoch)
		c.log.Warn("approval request resend postponed: could not read the round's request",
			"incident", inc.ID, "round", inc.ApprovalEpoch, "err", err)
		return
	}
	if request.StepName == "" {
		return
	}
	// The request carries the round's stored preview, so the digest in its
	// approve command is the one this round binds to; nothing is recomputed.
	// A round without a stored preview is sent without one, as at the park;
	// a preview that cannot be read postpones the resend rather than send a
	// request that differs from the round's first.
	var ac notify.ApprovalContext
	if previews, ok := c.impactStore(); ok {
		rec, err := previews.LatestImpactPreview(ctx, inc.ID, inc.ApprovalEpoch)
		var p impact.Preview
		if err == nil {
			p, err = impact.Parse(rec)
		}
		switch {
		case err == nil:
			ac = approvalContext(&p)
		case !errors.Is(err, store.ErrNotFound):
			c.approvalRetry.again(inc.ID, inc.ApprovalEpoch)
			c.log.Warn("approval request resend postponed: could not read the round's impact preview",
				"incident", inc.ID, "round", inc.ApprovalEpoch, "err", err)
			return
		}
	}
	ac.Deadline = deadline
	ac.Delivered = delivered
	ac.Skip = skip
	ac.Sent = func(channel string) {
		metrics.ApprovalRequestRedeliveries.Inc()
		c.log.Warn("approval request not accepted by a notification channel yet; sending it again",
			"incident", inc.ID, "round", inc.ApprovalEpoch, "step", request.StepName, "channel", channel, "resend", attempt)
	}
	if err := notify.RequestApprovalWithContext(ctx, c.notifier, inc, request.StepName, ac); err != nil {
		c.log.Warn("approval request resend failed", "incident", inc.ID, "round", inc.ApprovalEpoch, "err", err)
	}
}
