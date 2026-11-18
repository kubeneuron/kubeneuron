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

func queuedSpot(node string) interruption.Notice {
	return interruption.Notice{Node: node, Interruption: cloud.Interruption{
		InstanceID: "i-" + node, Kind: cloud.InterruptionSpotReclaim, Deadline: time.Now().Add(2 * time.Minute),
		Source: "aws-eventbridge:ec2/spot-instance-interruption-warning", Key: "aws-eventbridge:spot-" + node, Once: true,
	}}
}

func queuedRunning(node string) interruption.Notice {
	return interruption.Notice{Node: node, Interruption: cloud.Interruption{
		InstanceID: "i-" + node, Source: "aws-eventbridge:ec2/instance-state-change",
		Key: "aws-eventbridge:running-" + node, Once: true, Ended: true,
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

// The queue's report that the instance is running again closes the incident
// as ended, releases its cordon, and is acknowledged.
func TestQueuedEndClosesTheIncident(t *testing.T) {
	c, st, plat := queueFixture(t)
	ctx := context.Background()
	plat.deliver(queuedSpot("n1"))
	c.pollInterruptions(ctx)
	id := interruptionIncident(t, st, "n1").ID
	settle(t, c, st, id)

	end := queuedRunning("n1")
	plat.deliver(end)
	c.pollInterruptions(ctx)
	inc, err := st.GetIncident(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != types.StateResolved {
		t.Fatalf("state %s, want resolved as ended", inc.State)
	}
	if _, results := trailActions(t, st, id); !containsText(results, "the cloud reported that the interruption of n1 is over") {
		t.Fatalf("not closed on the cloud's report: %v", results)
	}
	if len(plat.released) != 1 || plat.released[0] != "n1/"+id {
		t.Fatalf("released %v, want the incident's own cordon", plat.released)
	}
	if !plat.wasAcked(end.Key) {
		t.Fatal("the end report was not acknowledged")
	}
}

// An end report whose incident could not be closed is not acknowledged, so
// it is read again.
func TestQueuedEndIsKeptWhenTheIncidentCannotClose(t *testing.T) {
	c, st, plat := queueFixture(t)
	ctx := context.Background()
	plat.deliver(queuedSpot("n1"))
	c.pollInterruptions(ctx)
	id := interruptionIncident(t, st, "n1").ID
	settle(t, c, st, id)

	end := queuedRunning("n1")
	plat.deliver(end)
	plat.existsErr = errors.New("apiserver unavailable")
	c.pollInterruptions(ctx)
	if plat.wasAcked(end.Key) {
		t.Fatal("an end report was acknowledged although the incident stayed open")
	}
	plat.existsErr = nil
	c.pollInterruptions(ctx)
	if inc, _ := st.GetIncident(ctx, id); inc.State != types.StateResolved || !plat.wasAcked(end.Key) {
		t.Fatalf("state %s, acknowledged %v; want resolved and acknowledged", inc.State, plat.acked)
	}
}

// An end report for a node with no open incident, and a notice for a node
// KubeNeuron does not manage, are acknowledged and open nothing.
func TestQueuedNoticesThatNeedNothingAreAcknowledged(t *testing.T) {
	c, st, plat := queueFixture(t)
	ctx := context.Background()
	end, elsewhere := queuedRunning("n1"), queuedSpot("elsewhere")
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
