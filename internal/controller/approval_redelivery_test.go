// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/impact"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// channel is a notification channel that can be down; it records every
// approval request it was handed, delivered or not.
type channel struct {
	name      string
	mu        sync.Mutex
	down      bool
	requests  []int // the round of each request
	deadlines []time.Time
	digests   []string
}

func (ch *channel) ChannelName() string { return ch.name }

func (ch *channel) Notify(context.Context, notify.NotifyEvent) error { return nil }

func (ch *channel) RequestApproval(ctx context.Context, inc *types.Incident, step string) error {
	return ch.RequestApprovalWithContext(ctx, inc, step, notify.ApprovalContext{})
}

func (ch *channel) RequestApprovalWithContext(_ context.Context, inc *types.Incident, _ string, ac notify.ApprovalContext) error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.requests = append(ch.requests, inc.ApprovalEpoch)
	ch.deadlines = append(ch.deadlines, ac.Deadline)
	ch.digests = append(ch.digests, ac.ImpactDigest)
	if ch.down {
		return errors.New("channel unavailable")
	}
	return nil
}

func (ch *channel) setDown(down bool) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.down = down
}

func (ch *channel) sent() []int {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return append([]int(nil), ch.requests...)
}

// accepted reports whether the store records the channel as having accepted
// round 1.
func accepted(t *testing.T, st *sqlite.Store, id, channel string) bool {
	t.Helper()
	return mustIncident(t, st, id).ApprovalNotified.Accepted(1)[channel]
}

func redeliveryEngine(t *testing.T) *playbook.Engine {
	t.Helper()
	pb := &playbook.Playbook{
		Name: "pb", Target: "node",
		Steps: []playbook.Step{{Name: "reboot", Action: "agent.reboot", Approval: "required"}},
	}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"pb": pb}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func redeliveryController(st *sqlite.Store, engine *playbook.Engine, n notify.Notifier) *Controller {
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1})
	return New(st, st, engine, gate, nil, nil, nil, n, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// redeliveryFixture parks one incident for approval with the given channels
// behind the shipped log channel, as the controller binary wires them.
func redeliveryFixture(t *testing.T, channels ...notify.Notifier) (*Controller, *sqlite.Store, *types.Incident) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := redeliveryController(st, redeliveryEngine(t), logAnd(channels...))
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	inc := &types.Incident{
		ID: "inc-redeliver", Target: types.Target{Node: "n1"},
		Class: types.ClassFellOffBus, State: types.StateEvaluating, Playbook: "pb",
		OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
	}
	allowCloudApprovalFixture(c, inc)
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	c.reconcile(ctx)
	parked := mustIncident(t, st, inc.ID)
	if parked.State != types.StateAwaitingApproval || parked.ApprovalEpoch != 1 {
		t.Fatalf("not parked: state=%s epoch=%d", parked.State, parked.ApprovalEpoch)
	}
	return c, st, inc
}

func logAnd(channels ...notify.Notifier) notify.Multi {
	return append(notify.Multi{&notify.Log{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}, channels...)
}

// makeDue moves the next send of every scheduled round to now.
func (c *Controller) makeDue() {
	c.approvalRetry.mu.Lock()
	defer c.approvalRetry.mu.Unlock()
	for _, s := range c.approvalRetry.slots {
		s.at = time.Time{}
	}
}

// A request a channel did not accept is sent again when its pause has
// passed, not on every pass, until the channel accepts it; then the store
// records it and nothing more is sent.
func TestUndeliveredApprovalRequestIsSentUntilTheChannelAcceptsIt(t *testing.T) {
	ch := &channel{name: "webhook", down: true}
	c, st, inc := redeliveryFixture(t, ch)
	ctx := context.Background()

	c.reconcile(ctx)
	if got := ch.sent(); len(got) != 1 {
		t.Fatalf("sent %v before the first pause passed, want the park's request only", got)
	}
	before := testutil.ToFloat64(metrics.ApprovalRequestRedeliveries)
	c.makeDue()
	c.reconcile(ctx)
	if got := testutil.ToFloat64(metrics.ApprovalRequestRedeliveries) - before; got != 1 {
		t.Fatalf("one resend counted %v times", got)
	}
	if got := ch.sent(); len(got) != 2 || accepted(t, st, inc.ID, "webhook") {
		t.Fatalf("after one resend to a channel that is down: sent %v, accepted %v", got, accepted(t, st, inc.ID, "webhook"))
	}

	ch.setDown(false)
	c.makeDue()
	c.reconcile(ctx)
	if !accepted(t, st, inc.ID, "webhook") {
		t.Fatal("not recorded after the channel accepted")
	}
	c.makeDue()
	c.reconcile(ctx)
	got := ch.sent()
	if len(got) != 3 {
		t.Fatalf("sent %v, want exactly three requests: park, resend to a down channel, delivered resend", got)
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if !ch.deadlines[2].Equal(ch.deadlines[0]) || ch.deadlines[0].IsZero() {
		t.Fatalf("resend deadline %v, want the round's %v", ch.deadlines[2], ch.deadlines[0])
	}
}

// After a restart, a round a channel has not accepted is sent on the first
// pass; one it accepted is not sent again.
func TestRestartSendsAnUndeliveredRoundOnTheFirstPass(t *testing.T) {
	down := &channel{name: "slack", down: true}
	_, st, inc := redeliveryFixture(t, down)
	ctx := context.Background()

	up := &channel{name: "slack"}
	restarted := redeliveryController(st, redeliveryEngine(t), logAnd(up))
	allowCloudApprovalFixture(restarted, inc)
	restarted.reconcile(ctx)
	if got := up.sent(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("first pass after restart sent %v, want round 1 once", got)
	}
	if !accepted(t, st, inc.ID, "slack") {
		t.Fatal("not recorded after the channel accepted")
	}

	again := &channel{name: "slack"}
	second := redeliveryController(st, redeliveryEngine(t), logAnd(again))
	allowCloudApprovalFixture(second, inc)
	second.reconcile(ctx)
	if got := again.sent(); len(got) != 0 {
		t.Fatalf("a delivered round was sent again after a restart: %v", got)
	}
}

// A decision ends the round: nothing more is sent for it.
func TestDecisionStopsResendingTheRequest(t *testing.T) {
	ch := &channel{name: "slack", down: true}
	c, _, inc := redeliveryFixture(t, ch)
	ctx := context.Background()
	if err := c.DecideApproval(ctx, inc.ID, "token:walk", "cli", types.ApprovalRejected, 1, "", ""); err != nil {
		t.Fatal(err)
	}
	c.reconcile(ctx)
	c.makeDue()
	c.reconcile(ctx)
	if got := ch.sent(); len(got) != 1 {
		t.Fatalf("sent %v after the rejection, want only the park's request", got)
	}
}

// A new round is a new request, whether or not the previous one was
// delivered.
func TestNewRoundIsRequestedAgain(t *testing.T) {
	ch := &channel{name: "slack"}
	c, st, inc := redeliveryFixture(t, ch)
	ctx := context.Background()
	if !accepted(t, st, inc.ID, "slack") {
		t.Fatal("round 1 not recorded")
	}
	current := mustIncident(t, st, inc.ID)
	step, _, err := c.runtimeConfig(ctx).Engine.NextStep(current)
	if err != nil {
		t.Fatal(err)
	}
	ch.setDown(true)
	// The park reports the channel's refusal; the round is committed anyway.
	_ = c.parkForApproval(ctx, current, step, "test re-park")
	if got := ch.sent(); len(got) != 2 || got[1] != 2 {
		t.Fatalf("sent %v, want round 1 then round 2", got)
	}
	c.makeDue()
	c.reconcile(ctx)
	if got := ch.sent(); len(got) != 3 || got[2] != 2 {
		t.Fatalf("undelivered round 2 not sent again: %v", got)
	}
}

// Each channel is tracked on its own: the one that accepted gets the request
// once, also across a restart, and the one that was down gets it until it
// accepts.
func TestEachChannelIsSentUntilItAccepts(t *testing.T) {
	slack, pagerduty := &channel{name: "slack"}, &channel{name: "pagerduty", down: true}
	c, st, inc := redeliveryFixture(t, slack, pagerduty)
	ctx := context.Background()
	c.makeDue()
	c.reconcile(ctx)
	if len(slack.sent()) != 1 || len(pagerduty.sent()) != 2 {
		t.Fatalf("slack got %v, pagerduty got %v; want one and two", slack.sent(), pagerduty.sent())
	}

	slack2, pagerduty2 := &channel{name: "slack"}, &channel{name: "pagerduty", down: true}
	restarted := redeliveryController(st, redeliveryEngine(t), logAnd(slack2, pagerduty2))
	allowCloudApprovalFixture(restarted, inc)
	restarted.reconcile(ctx)
	if len(slack2.sent()) != 0 || len(pagerduty2.sent()) != 1 {
		t.Fatalf("after restart slack got %v, pagerduty got %v; want none and one", slack2.sent(), pagerduty2.sent())
	}

	pagerduty2.setDown(false)
	restarted.makeDue()
	restarted.reconcile(ctx)
	restarted.makeDue()
	restarted.reconcile(ctx)
	if len(slack2.sent()) != 0 || len(pagerduty2.sent()) != 2 || !accepted(t, st, inc.ID, "pagerduty") {
		t.Fatalf("after recovery slack got %v, pagerduty got %v", slack2.sent(), pagerduty2.sent())
	}
}

// A channel removed from the configuration does not hold the round open; one
// added while the round waits gets the request on the next pass.
func TestConfiguredChannelsDecideWhatIsMissing(t *testing.T) {
	slack, pagerduty := &channel{name: "slack"}, &channel{name: "pagerduty", down: true}
	_, st, inc := redeliveryFixture(t, slack, pagerduty)
	ctx := context.Background()

	slackOnly := &channel{name: "slack"}
	removed := redeliveryController(st, redeliveryEngine(t), logAnd(slackOnly))
	allowCloudApprovalFixture(removed, inc)
	removed.reconcile(ctx)
	removed.makeDue()
	removed.reconcile(ctx)
	if got := slackOnly.sent(); len(got) != 0 {
		t.Fatalf("sent %v with every configured channel accepted", got)
	}

	slack3, webhook := &channel{name: "slack"}, &channel{name: "webhook"}
	added := redeliveryController(st, redeliveryEngine(t), logAnd(slack3, webhook))
	allowCloudApprovalFixture(added, inc)
	added.reconcile(ctx)
	if len(slack3.sent()) != 0 || len(webhook.sent()) != 1 {
		t.Fatalf("slack got %v, the added webhook got %v; want none and one", slack3.sent(), webhook.sent())
	}
}

// A resend that finds the round still in the channel's queue hands nothing
// to the channel and is not counted; the queued request is the one that
// arrives, once.
func TestResendOfAQueuedRoundIsNotCounted(t *testing.T) {
	ch := &channel{name: "slack"}
	async := notify.NewAsync(ch, slog.New(slog.NewTextHandler(io.Discard, nil)))
	async.Channel = "slack"
	c, st, inc := redeliveryFixture(t, async)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	before := testutil.ToFloat64(metrics.ApprovalRequestRedeliveries)
	c.makeDue()
	c.reconcile(ctx)
	if got := testutil.ToFloat64(metrics.ApprovalRequestRedeliveries) - before; got != 0 {
		t.Fatalf("a resend the queue refused was counted %v times", got)
	}
	async.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !accepted(t, st, inc.ID, "slack") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	c.makeDue()
	c.reconcile(ctx)
	if got := ch.sent(); len(got) != 1 || !accepted(t, st, inc.ID, "slack") {
		t.Fatalf("channel got %v, accepted %v; want the queued request once", got, accepted(t, st, inc.ID, "slack"))
	}
	if got := testutil.ToFloat64(metrics.ApprovalRequestRedeliveries) - before; got != 0 {
		t.Fatalf("counted %v resends, want none", got)
	}
}

// previewFault is a store whose impact previews cannot be read.
type previewFault struct {
	*sqlite.Store
	err error
}

func (p *previewFault) LatestImpactPreview(ctx context.Context, id string, epoch int) (*types.ImpactPreview, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.Store.LatestImpactPreview(ctx, id, epoch)
}

// A resend carries the round's stored preview. When the round has none it is
// sent without one, as at the park; when the preview cannot be read the
// resend waits for the next pass instead of going out without its digest.
func TestResendCarriesTheRoundsPreviewOrWaits(t *testing.T) {
	first := &channel{name: "slack", down: true}
	_, st, inc := redeliveryFixture(t, first)
	ctx := context.Background()
	stored := impact.Unavailable(impact.Input{IncidentID: inc.ID, Round: 1, ComputedAt: time.Now(), Node: "n1"},
		impact.UnavailableNoPendingStep, "test preview")
	rec, err := impact.Record(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutImpactPreview(ctx, rec); err != nil {
		t.Fatal(err)
	}
	parkDigest := stored.Digest
	if parkDigest == "" {
		t.Fatal("stored preview has no digest")
	}

	faulty := &previewFault{Store: st, err: errors.New("disk I/O error")}
	ch := &channel{name: "slack"}
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1})
	c := New(faulty, st, redeliveryEngine(t), gate, nil, nil, nil, logAnd(ch), slog.New(slog.NewTextHandler(io.Discard, nil)))
	allowCloudApprovalFixture(c, inc)
	c.reconcile(ctx)
	if got := ch.sent(); len(got) != 0 {
		t.Fatalf("sent %v while the preview could not be read", got)
	}
	faulty.err = nil
	c.reconcile(ctx) // the next pass, without waiting for a pause
	ch.mu.Lock()
	digests := append([]string(nil), ch.digests...)
	ch.mu.Unlock()
	if len(digests) != 1 || digests[0] != parkDigest {
		t.Fatalf("resend digests %q, want the stored %q once", digests, parkDigest)
	}

	none := &previewFault{Store: st, err: store.ErrNotFound}
	other := &channel{name: "webhook"}
	c2 := New(none, st, redeliveryEngine(t), gate, nil, nil, nil, logAnd(other), slog.New(slog.NewTextHandler(io.Discard, nil)))
	allowCloudApprovalFixture(c2, inc)
	c2.reconcile(ctx)
	other.mu.Lock()
	defer other.mu.Unlock()
	if len(other.digests) != 1 || other.digests[0] != "" {
		t.Fatalf("a round without a stored preview: digests %q, want one request without a digest", other.digests)
	}
}
