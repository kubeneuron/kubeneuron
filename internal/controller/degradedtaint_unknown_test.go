// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var (
	_ platform.DegradedTaintOwnership = (*ownedTaintPlatform)(nil)
	_ platform.DegradedTaintOwnership = (*cordonTaintPlatform)(nil)
	_ platform.UnknownCordonKeeper    = (*cordonTaintPlatform)(nil)
)

// ownedTaintPlatform is taintPlatform with the owner and unknown records the
// real platform keeps on the node.
type ownedTaintPlatform struct {
	*taintPlatform
	owners   map[string][]string
	recorded map[string]bool
	unknown  map[string]bool
	cleared  []string
	// conflicts is how many unknown marks lose to a concurrent write.
	conflicts int
}

func newOwnedTaintPlatform() *ownedTaintPlatform {
	return &ownedTaintPlatform{
		taintPlatform: newTaintPlatform(),
		owners:        map[string][]string{}, recorded: map[string]bool{}, unknown: map[string]bool{},
	}
}

func (p *ownedTaintPlatform) RemoveDegradedTaint(ctx context.Context, node string) error {
	if err := p.taintPlatform.RemoveDegradedTaint(ctx, node); err != nil {
		return err
	}
	p.forget(node)
	return nil
}

func (p *ownedTaintPlatform) UpdateDegradedTaintOwners(_ context.Context, node string, add, drop []string) error {
	if _, ok := p.marked(node); !ok {
		return nil
	}
	owners := slices.DeleteFunc(slices.Clone(p.owners[node]), func(o string) bool { return slices.Contains(drop, o) })
	for _, o := range add {
		if !slices.Contains(owners, o) {
			owners = append(owners, o)
		}
	}
	p.owners[node], p.recorded[node] = owners, true
	return nil
}

func (p *ownedTaintPlatform) DegradedTaintOwners(_ context.Context, node string) ([]string, bool, bool, error) {
	return p.owners[node], p.recorded[node], p.unknown[node], nil
}

func (p *ownedTaintPlatform) MarkDegradedTaintUnknown(_ context.Context, node string) (bool, error) {
	if _, ok := p.marked(node); !ok || p.unknown[node] {
		return false, nil
	}
	if p.conflicts > 0 {
		p.conflicts--
		return false, nil
	}
	p.unknown[node] = true
	return true, nil
}

func (p *ownedTaintPlatform) ForgetDegradedTaintUnknown(_ context.Context, node string) error {
	delete(p.unknown, node)
	return nil
}

func (p *ownedTaintPlatform) ClearDegradedTaintRecords(context.Context) ([]string, error) {
	var cleared []string
	for _, records := range []map[string]bool{p.recorded, p.unknown} {
		for node := range records {
			if _, ok := p.marked(node); ok || slices.Contains(cleared, node) {
				continue
			}
			cleared = append(cleared, node)
		}
	}
	for _, node := range cleared {
		p.forget(node)
	}
	p.cleared = append(p.cleared, cleared...)
	return cleared, nil
}

func (p *ownedTaintPlatform) forget(node string) {
	delete(p.owners, node)
	delete(p.recorded, node)
	delete(p.unknown, node)
}

func (p *ownedTaintPlatform) removeByHand(node string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.marks, node)
}

func ownedTaintFixture(t *testing.T, policy DegradedTaintPolicy) (*Controller, *sqlite.Store, *ownedTaintPlatform, *recordingNotifier) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	p := newOwnedTaintPlatform()
	notifier := &recordingNotifier{}
	c := New(st, st, nil, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2}), nil, p, nil, notifier,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetDegradedTaintPolicy(policy)
	return c, st, p, notifier
}

func enabledTaint() DegradedTaintPolicy {
	return DegradedTaintPolicy{Enabled: true, Effect: "PreferNoSchedule"}
}

// The taint records the incident that placed it, so a later store can tell.
func TestDegradedTaintRecordsTheIncidentThatPlacedIt(t *testing.T) {
	c, st, p, _ := ownedTaintFixture(t, enabledTaint())
	inc := openIncident(t, st, "inc-1", "n1", types.StateOpen)
	if err := c.transition(context.Background(), inc, types.StateObserving, "system", "observe", "", nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.owners["n1"], []string{"inc-1"}) {
		t.Fatalf("owners = %v, want the incident that placed the taint", p.owners["n1"])
	}
}

// A reinstall with a new store: the taint is kept and reported once, both
// with the feature on and with it switched off.
func TestTaintOfAnIncidentTheStoreNeverKnewIsKeptAndReportedOnce(t *testing.T) {
	for _, policy := range []DegradedTaintPolicy{enabledTaint(), {}} {
		c, _, p, notifier := ownedTaintFixture(t, policy)
		p.mark("n1", "PreferNoSchedule")
		p.owners["n1"], p.recorded["n1"] = []string{"inc-old"}, true

		for range 3 {
			c.reconcileDegradedTaints(context.Background())
		}
		if _, ok := p.marked("n1"); !ok {
			t.Fatalf("enabled=%v: the taint of an incident the store never knew was removed", policy.Enabled)
		}
		if len(notifier.events) != 1 {
			t.Fatalf("enabled=%v: notifications = %d, want exactly one", policy.Enabled, len(notifier.events))
		}
		ev := notifier.events[0]
		if ev.Kind != notify.EventNodeKept || ev.Node != "n1" || ev.Incident != nil ||
			ev.Message != unknownTaintMessage("n1") ||
			!strings.Contains(ev.Message, "run kubectl taint nodes n1 kubeneuron.io/degraded-") {
			t.Fatalf("enabled=%v: notice = %+v", policy.Enabled, ev)
		}
	}
}

// A taint placed before owners were recorded, the three cases of the first
// pass after the upgrade.
func TestTaintWithoutRecordedOwnersAfterTheUpgrade(t *testing.T) {
	t.Run("an open incident on the node is recorded as the owner", func(t *testing.T) {
		c, st, p, notifier := ownedTaintFixture(t, enabledTaint())
		p.mark("n1", "PreferNoSchedule")
		inc := openIncident(t, st, "inc-1", "n1", types.StateEvaluating)

		c.reconcileDegradedTaints(context.Background())
		if !slices.Equal(p.owners["n1"], []string{"inc-1"}) {
			t.Fatalf("owners = %v, want the open incident recorded", p.owners["n1"])
		}
		inc.State = types.StateResolved
		if err := st.UpdateIncident(context.Background(), inc); err != nil {
			t.Fatal(err)
		}
		c.reconcileDegradedTaints(context.Background())
		if _, ok := p.marked("n1"); ok || len(notifier.events) != 0 {
			t.Fatalf("taint kept or reported (%d) after its recorded incident resolved", len(notifier.events))
		}
	})
	t.Run("a closed incident on the node lets the taint go", func(t *testing.T) {
		c, st, p, notifier := ownedTaintFixture(t, enabledTaint())
		p.mark("n1", "PreferNoSchedule")
		openIncident(t, st, "inc-1", "n1", types.StateResolved)

		c.reconcileDegradedTaints(context.Background())
		if _, ok := p.marked("n1"); ok || len(notifier.events) != 0 {
			t.Fatal("a taint whose node the store has a closed incident for was kept")
		}
	})
	t.Run("no record of the node keeps the taint", func(t *testing.T) {
		c, st, p, notifier := ownedTaintFixture(t, enabledTaint())
		p.mark("n1", "PreferNoSchedule")
		openIncident(t, st, "inc-other", "n2", types.StateResolved)

		c.reconcileDegradedTaints(context.Background())
		c.reconcileDegradedTaints(context.Background())
		if _, ok := p.marked("n1"); !ok || !p.unknown["n1"] || len(notifier.events) != 1 {
			t.Fatalf("marked unknown = %v, notices = %d; want the taint kept and reported once", p.unknown["n1"], len(notifier.events))
		}
	})
}

// A halted owner is dropped while another incident keeps the taint, so
// retention pruning it does not later read as an unknown incident.
func TestHaltedOwnersAreDroppedWhileTheTaintStays(t *testing.T) {
	c, st, p, notifier := ownedTaintFixture(t, enabledTaint())
	ctx := context.Background()
	first := openIncident(t, st, "inc-1", "n1", types.StateEvaluating)
	openIncident(t, st, "inc-2", "n1", types.StateEvaluating, types.ClassECCDBE)
	c.reconcileDegradedTaints(ctx)
	c.reconcileDegradedTaints(ctx)
	if got := p.owners["n1"]; len(got) != 2 {
		t.Fatalf("owners = %v, want both open incidents", got)
	}
	first.State = types.StateResolved
	if err := st.UpdateIncident(ctx, first); err != nil {
		t.Fatal(err)
	}
	c.reconcileDegradedTaints(ctx)
	if !slices.Equal(p.owners["n1"], []string{"inc-2"}) {
		t.Fatalf("owners = %v, want the resolved incident dropped", p.owners["n1"])
	}
	if len(notifier.events) != 0 {
		t.Fatalf("notices = %d", len(notifier.events))
	}
}

// Another incident halting on a node whose taint is kept for a human does not
// take the taint off.
func TestHaltingIncidentLeavesAKeptTaint(t *testing.T) {
	c, st, p, _ := ownedTaintFixture(t, enabledTaint())
	ctx := context.Background()
	p.mark("n1", "PreferNoSchedule")
	p.owners["n1"], p.recorded["n1"] = []string{"inc-old"}, true
	c.reconcileDegradedTaints(ctx)

	inc := openIncident(t, st, "inc-1", "n1", types.StateEvaluating)
	if err := c.transition(ctx, inc, types.StateResolved, "system", "resolve", "", nil); err != nil {
		t.Fatal(err)
	}
	c.reconcileDegradedTaints(ctx)
	if _, ok := p.marked("n1"); !ok {
		t.Fatal("an incident halting removed a taint kept for a human")
	}
}

// Removing a kept taint by hand: the next pass clears the records and does
// not notify again.
func TestHandRemovalOfAKeptTaintClearsItsRecords(t *testing.T) {
	c, _, p, notifier := ownedTaintFixture(t, enabledTaint())
	ctx := context.Background()
	p.mark("n1", "PreferNoSchedule")
	c.reconcileDegradedTaints(ctx)

	p.removeByHand("n1") // kubectl taint nodes n1 kubeneuron.io/degraded-
	c.reconcileDegradedTaints(ctx)
	c.reconcileDegradedTaints(ctx)
	if !slices.Equal(p.cleared, []string{"n1"}) || p.unknown["n1"] {
		t.Fatalf("cleared = %v, want n1 cleaned once", p.cleared)
	}
	if len(notifier.events) != 1 {
		t.Fatalf("notices = %d, want no second notice after the hand removal", len(notifier.events))
	}
}

// cordonTaintPlatform is a node that carries both a kept cordon and a taint.
type cordonTaintPlatform struct {
	*unknownPlatform
	taints *ownedTaintPlatform
}

func (p *cordonTaintPlatform) ApplyDegradedTaint(ctx context.Context, node, value, effect string) error {
	return p.taints.ApplyDegradedTaint(ctx, node, value, effect)
}

func (p *cordonTaintPlatform) RemoveDegradedTaint(ctx context.Context, node string) error {
	return p.taints.RemoveDegradedTaint(ctx, node)
}

func (p *cordonTaintPlatform) DegradedTaintedNodes(ctx context.Context) ([]string, error) {
	return p.taints.DegradedTaintedNodes(ctx)
}

func (p *cordonTaintPlatform) UpdateDegradedTaintOwners(ctx context.Context, node string, add, drop []string) error {
	return p.taints.UpdateDegradedTaintOwners(ctx, node, add, drop)
}

func (p *cordonTaintPlatform) DegradedTaintOwners(ctx context.Context, node string) ([]string, bool, bool, error) {
	return p.taints.DegradedTaintOwners(ctx, node)
}

func (p *cordonTaintPlatform) MarkDegradedTaintUnknown(ctx context.Context, node string) (bool, error) {
	return p.taints.MarkDegradedTaintUnknown(ctx, node)
}

func (p *cordonTaintPlatform) ForgetDegradedTaintUnknown(ctx context.Context, node string) error {
	return p.taints.ForgetDegradedTaintUnknown(ctx, node)
}

func (p *cordonTaintPlatform) ClearDegradedTaintRecords(ctx context.Context) ([]string, error) {
	return p.taints.ClearDegradedTaintRecords(ctx)
}

// A cordon and a taint from the same unknown incident produce one notice that
// names both ways back into service.
func TestCordonAndTaintOfTheSameUnknownIncidentAreReportedOnce(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		p := &cordonTaintPlatform{unknownPlatform: &unknownPlatform{}, taints: newOwnedTaintPlatform()}
		c, _ := stackTestController(t, p)
		notifier := &recordingNotifier{}
		c.notifier = notifier
		c.SetDegradedTaintPolicy(enabledTaint())
		ctx := context.Background()
		p.cordon(ctx, t, sharedCordonIncident("inc-old", types.ClassECCDBE))
		p.taints.mark("node-a", "PreferNoSchedule")
		if !legacy {
			p.taints.owners["node-a"], p.taints.recorded["node-a"] = []string{"inc-old"}, true
		}

		for range 2 {
			c.reconcileCordonedNodes(ctx)
			c.reconcileDegradedTaints(ctx)
		}
		if _, ok := p.taints.marked("node-a"); !ok || !p.cordoned("node-a") {
			t.Fatalf("legacy=%v: the cordon or the taint was removed", legacy)
		}
		if len(notifier.events) != 1 {
			t.Fatalf("legacy=%v: notices = %d, want one for both", legacy, len(notifier.events))
		}
		if want := "run kubectl uncordon node-a and kubectl taint nodes node-a kubeneuron.io/degraded-"; !strings.HasSuffix(notifier.events[0].Message, want) {
			t.Fatalf("legacy=%v: message %q does not end with %q", legacy, notifier.events[0].Message, want)
		}
	}
}

// A mark that lost to a concurrent write was not written, so nothing is
// reported; the next pass writes it and reports once.
func TestUnknownTaintMarkLostToAConflictIsNotReported(t *testing.T) {
	c, _, p, notifier := ownedTaintFixture(t, enabledTaint())
	ctx := context.Background()
	p.mark("n1", "PreferNoSchedule")
	p.conflicts = 1

	c.reconcileDegradedTaints(ctx)
	if len(notifier.events) != 0 || p.unknown["n1"] {
		t.Fatalf("notices = %d after a mark that was not written", len(notifier.events))
	}
	c.reconcileDegradedTaints(ctx)
	c.reconcileDegradedTaints(ctx)
	if len(notifier.events) != 1 || !p.unknown["n1"] {
		t.Fatalf("notices = %d, unknown = %v; want the mark written and one notice", len(notifier.events), p.unknown["n1"])
	}
}

// As for the cordon: a failed notice clears the mark, and the next pass
// reports again. The combined notice clears both marks.
func TestKeptTaintIsReportedAgainAfterAFailedNotice(t *testing.T) {
	c, _, p, _ := ownedTaintFixture(t, enabledTaint())
	notifier := &failingNotifier{fail: 1}
	c.notifier = notifier
	ctx := context.Background()
	p.mark("n1", "PreferNoSchedule")

	c.reconcileDegradedTaints(ctx)
	if p.unknown["n1"] {
		t.Fatal("the mark stayed after a notice that was not sent")
	}
	c.reconcileDegradedTaints(ctx)
	c.reconcileDegradedTaints(ctx)
	if _, ok := p.marked("n1"); !ok || len(notifier.events) != 1 {
		t.Fatalf("notices = %d; want the taint kept and one notice on the retry", len(notifier.events))
	}

	both := &cordonTaintPlatform{unknownPlatform: &unknownPlatform{}, taints: newOwnedTaintPlatform()}
	c2, _ := stackTestController(t, both)
	notifier2 := &failingNotifier{fail: 1}
	c2.notifier = notifier2
	c2.SetDegradedTaintPolicy(enabledTaint())
	both.cordon(ctx, t, sharedCordonIncident("inc-old", types.ClassECCDBE))
	both.taints.mark("node-a", "PreferNoSchedule")
	c2.reconcileCordonedNodes(ctx)
	if both.taints.unknown["node-a"] || len(both.unknown["node-a"]) != 0 {
		t.Fatal("a failed combined notice left a mark")
	}
	for range 2 {
		c2.reconcileCordonedNodes(ctx)
		c2.reconcileDegradedTaints(ctx)
	}
	if len(notifier2.events) != 1 || !strings.HasSuffix(notifier2.events[0].Message, "kubeneuron.io/degraded-") {
		t.Fatalf("notices = %+v; want one combined notice on the retry", notifier2.events)
	}
}
