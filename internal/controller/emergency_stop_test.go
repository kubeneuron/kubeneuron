package controller

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// TestSwitchingToDryRunStopsIncidentsAlreadyOpen covers the emergency stop an
// operator reaches for under pressure, which used to do the opposite of what
// it says.
//
// The failure was three separately-defensible decisions composing:
//
//  1. inc.DryRun is stamped in openIncidentTx and never re-read.
//  2. The operator compiles spec.safety.destructiveExecution.nodeSelector only
//     for an Enabled install, so any other mode compiles an EMPTY selector.
//  3. An empty selector is read as "no confinement configured" and allowed.
//
// So: an Enabled install with a blast radius, ladders in flight, one of them
// on a node outside the radius and correctly refused every tick. The operator
// sees damage and sets executionMode: DryRun — which docs/operations.md offers
// and docs/upgrade.md reassures still honours confinement. Configuration
// reloads in place, so there is no restart to clear the stamped flags. The gate
// now says dry-run, the selector is now empty, and the step that was refused a
// second ago is allowed on any node in the cluster.
//
// The switch has to reach work already in flight, which means asking the live
// gate at execution time rather than trusting a flag stamped at open.
func TestSwitchingToDryRunStopsIncidentsAlreadyOpen(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	// n1 is OUTSIDE the declared blast radius: it does not carry blast=yes.
	if err := st.UpsertNode(ctx, &types.Node{
		Name: "n1", UID: "n1-uid",
		Labels:        map[string]string{"kubernetes.io/hostname": "n1"},
		AgentLastSeen: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2, DryRun: false})
	c := New(st, st, nil, gate, nil,
		&livePlatform{labels: map[string]string{"kubernetes.io/hostname": "n1"}},
		nil, &notify.Log{Logger: log}, log)
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) {
		rc.DestructiveSelector = map[string]string{"blast": "yes"}
	})

	// Opened while Enabled, so it is not a dry-run incident and never will be.
	inc := &types.Incident{
		ID: "inc-1", Target: types.Target{Node: "n1"}, Class: types.ClassFellOffBus,
		State: types.StateEvaluating, DryRun: false,
	}
	step := &playbook.Step{Name: "replace", Action: "platform.replace_node"}

	if _, got := c.destructiveStepConfinement(ctx, inc, step); got != confinementOutOfScope {
		t.Fatalf("baseline: confinement = %v, want confinementOutOfScope — "+
			"n1 is outside the blast radius, so this test proves nothing unless it is refused first", got)
	}

	// Exactly what cmd/kubeneuron-controller/reload.go does when the operator
	// sets executionMode: DryRun — the gate's limits are re-applied in place
	// and the compiled selector empties, with no restart between them.
	gate.ApplyLimits(safety.Limits{MaxConcurrentRemediations: 2, DryRun: true})
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.DestructiveSelector = nil })

	if !c.effectiveDryRun(inc) {
		t.Fatal("the gate is in dry-run but an incident opened before the switch still reports live execution; " +
			"the operator's emergency stop does not reach work already in flight")
	}

	// The step must simulate rather than reach the platform. This is the
	// assertion that matters: confinement legitimately reports "allowed" for a
	// no-op, so a green confinement verdict here proves nothing on its own —
	// what proves it is that nothing executes.
	res, err := c.executeStep(ctx, inc, step)
	if err != nil {
		t.Fatalf("executeStep: %v", err)
	}
	if !strings.HasPrefix(res.Output, "DRY-RUN:") {
		t.Fatalf("executeStep produced %q; a live replace_node ran after the operator asked for dry-run", res.Output)
	}
}

// TestDryRunIncidentsNeverBecomeLive pins the other direction, which must NOT
// follow the gate: an incident opened in DryRun stays a no-op for its whole
// life even if the installation is later Enabled. Stamping at open is the
// right rule here — a ladder a human watched decide as a simulation must not
// silently start acting halfway through.
func TestDryRunIncidentsNeverBecomeLive(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2, DryRun: true})
	c := New(st, st, nil, gate, nil, nil, nil, &notify.Log{Logger: log}, log)

	inc := &types.Incident{
		ID: "inc-2", Target: types.Target{Node: "n1"}, Class: types.ClassFellOffBus,
		State: types.StateEvaluating, DryRun: true,
	}
	gate.ApplyLimits(safety.Limits{MaxConcurrentRemediations: 2, DryRun: false})
	if !c.effectiveDryRun(inc) {
		t.Fatal("an incident opened in DryRun started executing for real when the installation was Enabled mid-ladder")
	}
}

// TestStoppedLaddersAreNotCountedAsRecovered pins the metric half of the
// emergency stop.
//
// kubeneuronctl report reads the audit trail and refuses to count a simulated
// ladder. The Prometheus counters are charged at the terminal transition,
// where that trail is not available, and they used to read the flag stamped
// when the incident opened — so the two disagreed in the worst direction: an
// operator who pressed stop, watched every remaining ladder simulate to a
// close, and then opened the dashboard was told the fleet had recovered those
// GPU-hours.
func TestStoppedLaddersAreNotCountedAsRecovered(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2, DryRun: false})
	c := New(st, st, nil, gate, nil, nil, nil, &notify.Log{Logger: log}, log)

	// Opened while Enabled, so its own flag says live for the rest of its life.
	inc := &types.Incident{
		ID: "inc-stopped", Target: types.Target{Node: "n1", GPUUUID: "GPU-1"},
		Class: types.ClassFellOffBus, DryRun: false,
		OpenedAt: time.Now().Add(-time.Hour),
	}

	before := testutilCounterValue(t, "kubeneuron_incidents_recovered_total")
	// The operator presses stop; the ladder then simulates to a close.
	gate.ApplyLimits(safety.Limits{MaxConcurrentRemediations: 2, DryRun: true})
	c.recordRecoveryOutcome(context.Background(), inc, types.StateResolved)
	after := testutilCounterValue(t, "kubeneuron_incidents_recovered_total")

	if after != before {
		t.Fatal("a ladder that simulated after the operator stopped execution was counted as " +
			"recovered capacity; the dashboard would report GPU-hours returned by a system " +
			"that had just been told to stop")
	}
}

// testutilCounterValue sums every series of a counter from the default
// registry. Summing rather than selecting one label set keeps the assertion
// about "did this counter move at all", which is the question.
func testutilCounterValue(t *testing.T, name string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}
