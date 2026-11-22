// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/interruption"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// queuePlatform adds a cloud interruption queue: its notices are reported
// until acknowledged.
type queuePlatform struct {
	*interruptionPlatform
	qmu      sync.Mutex
	queued   []interruption.Notice
	acked    []string
	receives chan bool
	// existsErr fails the node presence check.
	existsErr error
}

func (p *queuePlatform) NodeExists(ctx context.Context, node string) (bool, error) {
	p.qmu.Lock()
	err := p.existsErr
	p.qmu.Unlock()
	if err != nil {
		return false, err
	}
	return p.interruptionPlatform.NodeExists(ctx, node)
}

func (p *queuePlatform) CloudInterruptionQueueConfigured() bool { return true }

func (p *queuePlatform) CloudInterruptions(context.Context) ([]interruption.Notice, error) {
	p.qmu.Lock()
	defer p.qmu.Unlock()
	return append([]interruption.Notice(nil), p.queued...), nil
}

func (p *queuePlatform) ReceiveCloudInterruptions(ctx context.Context) (bool, error) {
	select {
	case fresh := <-p.receives:
		return fresh, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (p *queuePlatform) AckCloudInterruption(_ context.Context, key string) error {
	p.qmu.Lock()
	defer p.qmu.Unlock()
	p.acked = append(p.acked, key)
	p.queued = slices.DeleteFunc(p.queued, func(n interruption.Notice) bool { return n.Key == key })
	return nil
}

func (p *queuePlatform) deliver(n ...interruption.Notice) {
	p.qmu.Lock()
	defer p.qmu.Unlock()
	p.queued = append(p.queued, n...)
}

func (p *queuePlatform) wasAcked(key string) bool {
	p.qmu.Lock()
	defer p.qmu.Unlock()
	return slices.Contains(p.acked, key)
}

// pending reports whether a notice with key is still on the queue.
func (p *queuePlatform) pending(key string) bool {
	p.qmu.Lock()
	defer p.qmu.Unlock()
	return slices.ContainsFunc(p.queued, func(n interruption.Notice) bool { return n.Key == key })
}

func queuedSpot(node string) interruption.Notice {
	return interruption.Notice{Node: node, Interruption: cloud.Interruption{
		InstanceID: "i-" + node, Kind: cloud.InterruptionSpotReclaim, Deadline: time.Now().Add(2 * time.Minute),
		Source: "aws-eventbridge:ec2/spot-instance-interruption-warning", Key: "aws-eventbridge:spot-" + node, Once: true,
	}}
}

// queuedRunning reports the instance running again at at.
func queuedRunning(node string, at time.Time) interruption.Notice {
	return interruption.Notice{Node: node, Interruption: cloud.Interruption{
		InstanceID: "i-" + node, Source: "aws-eventbridge:ec2/instance-state-change",
		Key: "aws-eventbridge:running-" + node + "-" + at.Format(time.RFC3339Nano), At: at, Once: true, Ended: true,
	}}
}

// queuedHealth is an AWS Health scheduled change for the node's instance,
// sent at at; closed is the event's closing update.
func queuedHealth(node, event string, at, start time.Time, closed bool) interruption.Notice {
	return interruption.Notice{Node: node, Interruption: cloud.Interruption{
		InstanceID: "i-" + node, Kind: cloud.InterruptionScheduledMaintenance, Deadline: start,
		Source: "aws-eventbridge:health/ec2-scheduled-change", Key: "aws-eventbridge:" + event + "/i-" + node,
		At: at, Once: true, Ended: closed,
	}}
}

func queueFixture(t *testing.T) (*Controller, *sqlite.Store, *queuePlatform) {
	t.Helper()
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Automatic, autoSettings()), liveLimits(), 1)
	qp := &queuePlatform{interruptionPlatform: plat, receives: make(chan bool)}
	c.platform = qp
	return c, st, qp
}

// auditFailingStore fails every audit write while fail is set.
type auditFailingStore struct {
	store.Store
	fail bool
}

func (s *auditFailingStore) AppendAudit(ctx context.Context, e *types.AuditEntry) error {
	if s.fail {
		return errors.New("disk full")
	}
	return s.Store.AppendAudit(ctx, e)
}

// A queued notice is acknowledged, and so deleted from the queue, only once
// it is recorded: a failed write leaves it to be delivered again.
func TestQueuedNoticeIsAcknowledgedOnlyOnceRecorded(t *testing.T) {
	c, st, plat := queueFixture(t)
	ctx := context.Background()
	failing := &auditFailingStore{Store: st, fail: true}
	c.store = failing
	n := queuedSpot("n1")
	plat.deliver(n)
	c.pollInterruptions(ctx)
	if plat.wasAcked(n.Key) {
		t.Fatal("a notice that could not be recorded was acknowledged")
	}

	failing.fail = false
	c.pollInterruptions(ctx)
	if !plat.wasAcked(n.Key) {
		t.Fatal("a recorded notice was not acknowledged")
	}
	id := interruptionIncident(t, st, "n1").ID
	notices, err := c.interruptionNotices(ctx, &types.Incident{ID: id})
	if err != nil || len(notices) != 1 || !notices[0].Once {
		t.Fatalf("recorded notices %+v, %v; want the one queued notice", notices, err)
	}
}

// A queue delivers a notice once: that it is no longer read does not end the
// incident, also after a restart forgets what was read.
func TestQueuedNoticeDoesNotEndWhenNoLongerRead(t *testing.T) {
	c, st, plat := queueFixture(t)
	ctx := context.Background()
	plat.deliver(queuedSpot("n1"))
	c.pollInterruptions(ctx)
	id := interruptionIncident(t, st, "n1").ID
	settle(t, c, st, id)

	c.pollInterruptions(ctx)
	c.interruptions.keep(map[string]bool{})
	c.pollInterruptions(ctx)
	if inc, _ := st.GetIncident(ctx, id); inc.State.Halted() {
		t.Fatalf("the incident of a queued notice ended when the notice was no longer read: %s", inc.State)
	}
	if len(plat.released) != 0 {
		t.Fatalf("cordon released: %v", plat.released)
	}
}

// openQueued records the notices and returns the node's incident.
func openQueued(t *testing.T, c *Controller, st *sqlite.Store, plat *queuePlatform, n ...interruption.Notice) string {
	t.Helper()
	plat.deliver(n...)
	c.pollInterruptions(context.Background())
	id := interruptionIncident(t, st, n[0].Node).ID
	settle(t, c, st, id)
	return id
}

func queuedIncidentState(t *testing.T, st *sqlite.Store, id string) types.IncidentState {
	t.Helper()
	inc, err := st.GetIncident(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return inc.State
}

// The instance running again after a maintenance notice closes the incident
// as ended, releases its cordon, and is acknowledged.
func TestQueuedRunningEndsAnEarlierMaintenanceNotice(t *testing.T) {
	c, st, plat := queueFixture(t)
	now := time.Now()
	id := openQueued(t, c, st, plat, queuedHealth("n1", "A", now.Add(-time.Hour), now.Add(time.Minute), false))

	end := queuedRunning("n1", now)
	plat.deliver(end)
	c.pollInterruptions(context.Background())
	if got := queuedIncidentState(t, st, id); got != types.StateResolved {
		t.Fatalf("state %s, want resolved as ended", got)
	}
	if _, results := trailActions(t, st, id); !containsText(results, "the cloud reported each announced interruption of n1 over") {
		t.Fatalf("not closed on the cloud's report: %v", results)
	}
	if len(plat.released) != 1 || plat.released[0] != "n1/"+id {
		t.Fatalf("released %v, want the incident's own cordon", plat.released)
	}
	if !plat.wasAcked(end.Key) {
		t.Fatal("the end report was not acknowledged")
	}
}

// A queue does not keep order: a running report older than the notice ends
// nothing, and is acknowledged.
func TestQueuedRunningOlderThanTheNoticeEndsNothing(t *testing.T) {
	c, st, plat := queueFixture(t)
	now := time.Now()
	id := openQueued(t, c, st, plat, queuedHealth("n1", "A", now, now.Add(time.Minute), false))

	end := queuedRunning("n1", now.Add(-time.Minute))
	plat.deliver(end)
	c.pollInterruptions(context.Background())
	if got := queuedIncidentState(t, st, id); got.Halted() {
		t.Fatalf("an older running report closed the incident: %s", got)
	}
	if !plat.wasAcked(end.Key) {
		t.Fatal("an end report that matched nothing was not acknowledged")
	}
}

// A spot reclaim ends with the node, never with a running report.
func TestQueuedRunningDoesNotEndASpotNotice(t *testing.T) {
	c, st, plat := queueFixture(t)
	spot := queuedSpot("n1")
	spot.At = time.Now().Add(-time.Minute)
	id := openQueued(t, c, st, plat, spot)

	plat.deliver(queuedRunning("n1", time.Now()))
	c.pollInterruptions(context.Background())
	if got := queuedIncidentState(t, st, id); got.Halted() {
		t.Fatalf("a running report closed a spot reclaim: %s", got)
	}
}

// AWS Health closing event A ends A only; the incident stays open while B is
// in force, and closes when B is closed too.
func TestQueuedHealthClosedEndsOnlyItsEvent(t *testing.T) {
	c, st, plat := queueFixture(t)
	now := time.Now()
	start := now.Add(time.Minute)
	id := openQueued(t, c, st, plat, queuedHealth("n1", "A", now, start, false), queuedHealth("n1", "B", now, start, false))

	plat.deliver(queuedHealth("n1", "A", now.Add(time.Minute), start, true))
	c.pollInterruptions(context.Background())
	if got := queuedIncidentState(t, st, id); got.Halted() {
		t.Fatalf("closing event A closed the incident while B is in force: %s", got)
	}
	plat.deliver(queuedHealth("n1", "B", now.Add(time.Minute), start, true))
	c.pollInterruptions(context.Background())
	if got := queuedIncidentState(t, st, id); got != types.StateResolved {
		t.Fatalf("state %s, want resolved once both events closed", got)
	}
}

// An end report whose record could not be written is not acknowledged, so
// it is read again. Once written, it is acknowledged even if the incident
// cannot close yet: the incident closes on a later pass from its trail.
func TestQueuedEndIsAcknowledgedOnceWritten(t *testing.T) {
	c, st, plat := queueFixture(t)
	ctx := context.Background()
	now := time.Now()
	id := openQueued(t, c, st, plat, queuedHealth("n1", "A", now, now.Add(time.Minute), false))

	end := queuedHealth("n1", "A", now.Add(time.Minute), now.Add(time.Minute), true)
	plat.deliver(end)
	failing := &auditFailingStore{Store: st, fail: true}
	c.store = failing
	c.pollInterruptions(ctx)
	if !plat.pending(end.Key) {
		t.Fatal("an end report that could not be written was acknowledged")
	}

	failing.fail = false
	plat.existsErr = errors.New("apiserver unavailable")
	c.pollInterruptions(ctx)
	if plat.pending(end.Key) {
		t.Fatal("a written end report was not acknowledged")
	}
	if got := queuedIncidentState(t, st, id); got.Halted() {
		t.Fatalf("closed although the node could not be read: %s", got)
	}
	plat.existsErr = nil
	c.pollInterruptions(ctx)
	if got := queuedIncidentState(t, st, id); got != types.StateResolved {
		t.Fatalf("state %s, want resolved from the trail", got)
	}
}

// AWS Health moving a maintenance window updates the recorded deadline, and
// the incident's timing follows the new one; the signal is not counted again.
func TestQueuedHealthMovedDeadlineIsRecorded(t *testing.T) {
	c, st, plat := queueFixture(t)
	ctx := context.Background()
	now := time.Now()
	first := queuedHealth("n1", "A", now, now.Add(48*time.Hour), false)
	plat.deliver(first)
	c.pollInterruptions(ctx)
	inc := interruptionIncident(t, st, "n1")

	moved := queuedHealth("n1", "A", now.Add(time.Minute), now.Add(72*time.Hour), false)
	plat.deliver(moved)
	c.pollInterruptions(ctx)
	notices, err := c.interruptionNotices(ctx, inc)
	if err != nil || len(notices) != 1 || !notices[0].Deadline.Equal(moved.Deadline) {
		t.Fatalf("notices %+v, %v; want one with the moved deadline", notices, err)
	}
	if got, ok := urgentNotice(notices); !ok || !got.due().Equal(moved.Deadline) {
		t.Fatalf("timed by %+v, want the moved deadline", got)
	}
	if _, results := trailActions(t, st, inc.ID); !containsText(results, "the cloud moved the deadline") {
		t.Fatalf("move not recorded: %v", results)
	}
	after, _ := st.GetIncident(ctx, inc.ID)
	if after.SignalSeen != inc.SignalSeen {
		t.Fatalf("signal counted again: %d, was %d", after.SignalSeen, inc.SignalSeen)
	}
	if plat.pending(moved.Key) {
		t.Fatal("the update was not acknowledged")
	}
}

// A rebalance hint from the queue that no notice taking the node follows
// closes after an hour; it holds no cordon.
func TestQueuedRebalanceAloneClosesAfterAnHour(t *testing.T) {
	c, st, plat := queueFixture(t)
	ctx := context.Background()
	hint := queuedSpot("n1")
	hint.Kind, hint.Deadline, hint.Key = cloud.InterruptionRebalance, time.Time{}, "aws-eventbridge:rebalance-n1"
	plat.deliver(hint)
	c.pollInterruptions(ctx)
	id := interruptionIncident(t, st, "n1").ID
	c.pollInterruptions(ctx)
	if got := queuedIncidentState(t, st, id); got.Halted() {
		t.Fatalf("a fresh rebalance hint closed: %s", got)
	}
	clock := &fakeClock{now: time.Now().Add(interruptionOverdue + time.Minute)}
	c.checkpointNow = clock.Now
	c.pollInterruptions(ctx)
	if got := queuedIncidentState(t, st, id); got != types.StateResolved {
		t.Fatalf("state %s, want resolved an hour after the hint", got)
	}
}

// An end report for a node with no open incident, and a notice for a node
// KubeNeuron does not manage, are acknowledged and open nothing.
func TestQueuedNoticesThatNeedNothingAreAcknowledged(t *testing.T) {
	c, st, plat := queueFixture(t)
	ctx := context.Background()
	end, elsewhere := queuedRunning("n1", time.Now()), queuedSpot("elsewhere")
	plat.deliver(end, elsewhere)
	c.pollInterruptions(ctx)
	if incidents, _ := st.ListIncidents(ctx, store.IncidentFilter{}); len(incidents) != 0 {
		t.Fatalf("opened %d incidents", len(incidents))
	}
	if !plat.wasAcked(end.Key) || !plat.wasAcked(elsewhere.Key) {
		t.Fatalf("acknowledged %v, want both", plat.acked)
	}
}

// The receive loop wakes the interruption pass on a notice and stops with
// its context, as it does when the controller stops leading.
func TestQueueReceiveLoopWakesAndStops(t *testing.T) {
	c, _, plat := queueFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.receiveCloudInterruptions(ctx, plat)
		close(done)
	}()
	plat.receives <- false
	select {
	case <-c.interruptionWake:
		t.Fatal("woken with nothing received")
	default:
	}
	plat.receives <- true
	select {
	case <-c.interruptionWake:
	case <-time.After(5 * time.Second):
		t.Fatal("a received notice did not wake the interruption pass")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the receive loop did not stop with its context")
	}
}
