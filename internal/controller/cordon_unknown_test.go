// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var _ platform.UnknownCordonKeeper = (*unknownPlatform)(nil)

// unknownPlatform is ownedPlatform with the unknown-incident record the real
// platform keeps on the node.
type unknownPlatform struct {
	ownedPlatform
	unknown map[string][]string
	// unschedulable is false for a node a human has uncordoned by hand.
	unschedulable map[string]bool
	cleared       []string
}

func (p *unknownPlatform) MarkCordonUnknownIfOwner(ctx context.Context, node, owner string) (bool, error) {
	if !slices.Contains(p.owners[node], owner) || slices.Contains(p.unknown[node], owner) {
		return false, nil
	}
	if _, err := p.MarkCordonHeldIfOwner(ctx, node, owner); err != nil {
		return false, err
	}
	if p.unknown == nil {
		p.unknown = map[string][]string{}
	}
	p.unknown[node] = append(p.unknown[node], owner)
	return true, nil
}

func (p *unknownPlatform) ForgetUnknownCordon(_ context.Context, node, owner string) error {
	notOwner := func(o string) bool { return o == owner }
	p.unknown[node] = slices.DeleteFunc(p.unknown[node], notOwner)
	p.heldOwners[node] = slices.DeleteFunc(p.heldOwners[node], notOwner)
	return nil
}

func (p *unknownPlatform) ClearUnknownCordonsReleasedByHand(context.Context) ([]string, error) {
	var cleared []string
	for node := range p.unknown {
		if p.unschedulable[node] {
			continue
		}
		if others := slices.DeleteFunc(slices.Clone(p.owners[node]), func(o string) bool {
			return slices.Contains(p.unknown[node], o)
		}); len(others) > 0 {
			// Other holds stay with their bookkeeping.
			p.heldOwners[node] = slices.DeleteFunc(p.heldOwners[node], func(o string) bool { return slices.Contains(p.unknown[node], o) })
			p.owners[node] = others
			delete(p.unknown, node)
			cleared = append(cleared, node)
			continue
		}
		delete(p.unknown, node)
		delete(p.owners, node)
		delete(p.reason, node)
		delete(p.held, node)
		delete(p.heldOwners, node)
		cleared = append(cleared, node)
	}
	p.cleared = append(p.cleared, cleared...)
	return cleared, nil
}

func (p *unknownPlatform) cordon(ctx context.Context, t *testing.T, inc *types.Incident) {
	t.Helper()
	if err := p.CordonForOwner(ctx, inc.Target.Node, inc.ID, cordonReason(inc)); err != nil {
		t.Fatal(err)
	}
	if p.unschedulable == nil {
		p.unschedulable = map[string]bool{}
	}
	p.unschedulable[inc.Target.Node] = true
}

// A reinstall with a new store knows none of the incidents the old one
// cordoned for. The cordon stays, a human is told once, and the hold is marked
// so no later pass releases it.
func TestCordonOfAnIncidentTheStoreNeverKnewIsKeptAndReportedOnce(t *testing.T) {
	p := &unknownPlatform{}
	c, _ := stackTestController(t, p)
	notifier := &recordingNotifier{}
	c.notifier = notifier
	ctx := context.Background()
	p.cordon(ctx, t, sharedCordonIncident("inc-old", types.ClassECCDBE))

	for range 3 {
		c.reconcileCordonedNodes(ctx)
	}
	if !p.cordoned("node-a") || len(p.uncordoned) != 0 {
		t.Fatalf("uncordoned = %v; a node whose incident the store never knew went back into service", p.uncordoned)
	}
	if !slices.Contains(p.heldOwners["node-a"], "inc-old") {
		t.Fatalf("held owners = %v, want the kept hold marked held", p.heldOwners["node-a"])
	}
	if len(notifier.events) != 1 {
		t.Fatalf("notifications = %d, want exactly one across passes", len(notifier.events))
	}
	ev := notifier.events[0]
	for _, want := range []string{
		"Node node-a carries a KubeNeuron cordon for incident inc-old",
		"has no record of that incident",
		"run kubectl uncordon node-a",
	} {
		if !strings.Contains(ev.Message, want) {
			t.Errorf("message %q does not contain %q", ev.Message, want)
		}
	}
	if ev.Kind != notify.EventNodeKept || ev.Incident != nil || ev.Node != "node-a" || ev.IncidentID != "inc-old" {
		t.Fatalf("notice = %+v, want a node_kept notice naming the node and the incident, with no incident record", ev)
	}
}

// The window the lead asked to close: a resolved incident's cordon that the
// janitor never saw before retention pruned the row. Without a record the
// janitor cannot tell this from a store that never knew, so it keeps the
// cordon. A verdict about another hold still does not strand it, and once that
// hold is resolved only the unknown hold keeps the node.
func TestHoldPrunedBeforeTheJanitorSawItIsKeptNotReleased(t *testing.T) {
	p := &unknownPlatform{}
	c, st := stackTestController(t, p)
	c.notifier = &recordingNotifier{}
	ctx := context.Background()
	halted := sharedCordonIncident("inc-1", types.ClassECCDBE)
	halted.State = types.StateNeedsHuman
	leaked := sharedCordonIncident("inc-2", types.ClassFellOffBus)
	for _, inc := range []*types.Incident{halted, leaked} {
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		p.cordon(ctx, t, inc)
	}
	c.reconcileCordonedNodes(ctx)

	leaked.State = types.StateResolved
	if err := st.UpdateIncident(ctx, leaked); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Prune(ctx, time.Nanosecond, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetIncident(ctx, "inc-2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetIncident(inc-2) = %v, want the row to have been pruned", err)
	}
	c.reconcileCordonedNodes(ctx)
	if !slices.Contains(p.unknown["node-a"], "inc-2") {
		t.Fatalf("unknown holds = %v, want inc-2 kept for a human", p.unknown["node-a"])
	}

	halted.State = types.StateResolved
	if err := st.UpdateIncident(ctx, halted); err != nil {
		t.Fatal(err)
	}
	c.reconcileCordonedNodes(ctx)
	if got := p.owners["node-a"]; !slices.Equal(got, []string{"inc-2"}) {
		t.Fatalf("holders = %v, want only the unknown hold left", got)
	}
}

// One `kubectl uncordon` must return the node to service: the janitor then
// removes the bookkeeping it left, and does not notify again.
func TestHandUncordonOfAKeptCordonClearsItsBookkeeping(t *testing.T) {
	p := &unknownPlatform{}
	c, _ := stackTestController(t, p)
	notifier := &recordingNotifier{}
	c.notifier = notifier
	ctx := context.Background()
	p.cordon(ctx, t, sharedCordonIncident("inc-old", types.ClassECCDBE))
	c.reconcileCordonedNodes(ctx)

	p.unschedulable["node-a"] = false // kubectl uncordon node-a
	c.reconcileCordonedNodes(ctx)
	c.reconcileCordonedNodes(ctx)
	if !slices.Equal(p.cleared, []string{"node-a"}) {
		t.Fatalf("cleared = %v, want node-a cleaned once", p.cleared)
	}
	if p.cordoned("node-a") {
		t.Fatalf("holders = %v after the human uncordoned the node", p.owners["node-a"])
	}
	if len(notifier.events) != 1 {
		t.Fatalf("notifications = %d, want no second notice after the uncordon", len(notifier.events))
	}
}

// A hold a human already owns because its incident halted is not touched by
// the hand-uncordon cleanup: that path is only for holds kept as unknown.
func TestHandUncordonCleanupLeavesHaltedHoldsAlone(t *testing.T) {
	p := &unknownPlatform{}
	c, st := stackTestController(t, p)
	c.notifier = &recordingNotifier{}
	ctx := context.Background()
	halted := sharedCordonIncident("inc-1", types.ClassECCDBE)
	halted.State = types.StateExpired
	if err := st.CreateIncident(ctx, halted); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateIncident(ctx, halted); err != nil {
		t.Fatal(err)
	}
	p.cordon(ctx, t, halted)
	c.reconcileCordonedNodes(ctx)
	p.unschedulable["node-a"] = false
	c.reconcileCordonedNodes(ctx)
	if len(p.cleared) != 0 || !slices.Contains(p.heldOwners["node-a"], "inc-1") {
		t.Fatalf("cleared = %v, held = %v; a halted hold was treated as an unknown one", p.cleared, p.heldOwners["node-a"])
	}
}

// Store retention waits on this signal, so it must fire after the janitors'
// first pass and not before.
func TestFirstJanitorPassClosesAfterThePass(t *testing.T) {
	p := &unknownPlatform{}
	c, _ := stackTestController(t, p)
	select {
	case <-c.FirstJanitorPass():
		t.Fatal("FirstJanitorPass closed before any janitor pass")
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.runJanitors(ctx) }()
	select {
	case <-c.FirstJanitorPass():
	case <-time.After(10 * time.Second):
		t.Fatal("FirstJanitorPass did not close after the first pass")
	}
	cancel()
	<-done
}

// Unlike a cordon, a quiesced accelerator stack whose incident this store has
// no record of is switched back on. Nothing is running on the node that needs
// it off, and keeping the device plugin off would leave a node a human
// uncordons without its GPUs.
func TestQuiesceOfAnIncidentTheStoreNeverKnewIsRestored(t *testing.T) {
	p := &stackPlatform{quiescedNodes: []string{"node-a"}}
	c, _ := stackTestController(t, p)
	c.restoreAbandonedAcceleratorStacks(context.Background())
	if len(p.restored) != 1 {
		t.Fatalf("restored = %v, want the accelerator stack switched back on", p.restored)
	}
}

// failingNotifier fails its first fail notices and records the rest.
type failingNotifier struct {
	recordingNotifier
	fail int
}

func (n *failingNotifier) Notify(ctx context.Context, ev notify.NotifyEvent) error {
	if n.fail > 0 {
		n.fail--
		return errors.New("slack is down")
	}
	return n.recordingNotifier.Notify(ctx, ev)
}

// A notice that could not be sent must not leave the record that stops the
// next one: the cordon stays, and the next pass reports it.
func TestKeptCordonIsReportedAgainAfterAFailedNotice(t *testing.T) {
	p := &unknownPlatform{}
	c, _ := stackTestController(t, p)
	notifier := &failingNotifier{fail: 1}
	c.notifier = notifier
	ctx := context.Background()
	p.cordon(ctx, t, sharedCordonIncident("inc-old", types.ClassECCDBE))

	c.reconcileCordonedNodes(ctx)
	if len(p.unknown["node-a"]) != 0 || !p.cordoned("node-a") {
		t.Fatalf("unknown = %v, cordoned = %v after a failed notice; want the record gone and the cordon kept",
			p.unknown["node-a"], p.cordoned("node-a"))
	}
	c.reconcileCordonedNodes(ctx)
	c.reconcileCordonedNodes(ctx)
	if len(notifier.events) != 1 || !slices.Contains(p.unknown["node-a"], "inc-old") {
		t.Fatalf("notices = %d, unknown = %v; want one notice on the retry and the record back",
			len(notifier.events), p.unknown["node-a"])
	}
}

// A pass that could not list the cordons or the taints has not seen them, so
// retention keeps waiting.
func TestFirstJanitorPassWaitsForAPassThatListedEverything(t *testing.T) {
	p := newTaintPlatform()
	p.listErr = errors.New("apiserver unavailable")
	c, _ := stackTestController(t, p)
	c.reconcileEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.runJanitors(ctx) }()
	defer func() { cancel(); <-done }()

	select {
	case <-c.FirstJanitorPass():
		t.Fatal("FirstJanitorPass closed after passes that could not list the degraded taints")
	case <-time.After(100 * time.Millisecond):
	}
	p.mu.Lock()
	p.listErr = nil
	p.mu.Unlock()
	select {
	case <-c.FirstJanitorPass():
	case <-time.After(10 * time.Second):
		t.Fatal("FirstJanitorPass did not close after a pass that listed everything")
	}
}
