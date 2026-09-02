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
	liveLimits := safety.Limits{MaxConcurrentRemediations: 2, DryRun: false}
	if err := c.InstallRuntimeConfig(RuntimeConfig{
		SafetyLimits:        &liveLimits,
		DestructiveSelector: map[string]string{"blast": "yes"},
	}); err != nil {
		t.Fatal(err)
	}

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
	// sets executionMode: DryRun: one runtime snapshot replaces the mode and the
	// selector together, with no mixed generation exposed to a step.
	dryLimits := safety.Limits{MaxConcurrentRemediations: 2, DryRun: true}
	if err := c.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &dryLimits}); err != nil {
		t.Fatal(err)
	}

	if !c.effectiveDryRun(ctx, inc) {
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

// A reconcile pass can pin the former Enabled snapshot immediately before a
// reload.  DryRun must still be a one-way stop at the final dispatch boundary:
// an already-admitted action may finish, but a goroutine that has not reached
// the platform call must not begin it after the reload succeeded.
func TestEmergencyDryRunStopsAPreviouslyPinnedPlatformDispatch(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := &recyclePlatform{configured: true, nodeReady: true}
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2, DryRun: false})
	c := New(nil, nil, nil, gate, nil, p, nil, &notify.Log{Logger: log}, log)
	inc := &types.Incident{ID: "inc-pinned", Target: types.Target{Node: "node-a"}, DryRun: false}
	step := &playbook.Step{Name: "replace", Action: "platform.replace_node"}

	live := safety.Limits{MaxConcurrentRemediations: 2, DryRun: false}
	if err := c.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &live}); err != nil {
		t.Fatal(err)
	}
	ctx := c.pinRuntimeConfig(context.Background())
	ctx, simulated := c.pinSimulate(ctx, inc)
	if simulated {
		t.Fatal("test setup pinned DryRun instead of Enabled")
	}

	dry := safety.Limits{MaxConcurrentRemediations: 2, DryRun: true}
	if err := c.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &dry}); err != nil {
		t.Fatal(err)
	}
	result, err := c.executeStep(ctx, inc, step)
	if err != nil {
		t.Fatal(err)
	}
	if p.replaced != "" {
		t.Fatalf("reload-to-DryRun replaced %q after the stop succeeded", p.replaced)
	}
	if result == nil || !strings.Contains(result.Output, "DRY-RUN") {
		t.Fatalf("result = %+v, want a dry-run result", result)
	}
}

// Stopping new remediation must not strand a node whose GPU monitoring stack
// KubeNeuron itself already quiesced. This is the narrow inverse of the test
// above: only the recorded accelerator-stack restoration may pass the final
// DryRun boundary; replace, drain, reset, and every other action stay no-ops.
func TestEmergencyDryRunStillRestoresAcceleratorStack(t *testing.T) {
	act := &hostActuator{output: "nvidia-persistenced started"}
	p := &stackPlatform{quiescedNodes: []string{"node-a"}}
	c, _ := stackTestControllerWithActuator(t, p, act)
	inc := resetIncident()
	dry := safety.Limits{MaxConcurrentRemediations: 2, DryRun: true}
	if err := c.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &dry}); err != nil {
		t.Fatal(err)
	}

	result, err := c.executeStep(context.Background(), inc, &playbook.Step{
		Name: "restore", Action: "platform.restore_accelerator_stack",
	})
	if err != nil {
		t.Fatalf("execute compensating restore: %v", err)
	}
	if result == nil || strings.HasPrefix(result.Output, "DRY-RUN:") {
		t.Fatalf("restore result = %+v; want a real compensating action", result)
	}
	if len(p.restored) != 1 || len(p.quiescedNodes) != 0 {
		t.Fatalf("platform restore state = restored %v, pending %v; want one completed restore", p.restored, p.quiescedNodes)
	}
	if len(act.actions) != 1 || act.actions[0] != types.ActionRestoreAcceleratorHost {
		t.Fatalf("agent actions = %v; want only restore_accelerator_host", act.actions)
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
	// Even after the installation is Enabled, the stamped dry-run incident stays
	// simulated. Install through the runtime snapshot, not the raw gate.
	liveLimits := safety.Limits{MaxConcurrentRemediations: 2, DryRun: false}
	if err := c.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &liveLimits}); err != nil {
		t.Fatal(err)
	}
	if !c.effectiveDryRun(context.Background(), inc) {
		t.Fatal("an incident opened in DryRun started executing for real when the installation was Enabled mid-ladder")
	}
}

// The stop lever must change every live-only admission gate, not merely the
// final dispatcher. Otherwise a ladder that was live at open can wait for a
// missing reset profile or an unarmed agent and eventually quarantine a node
// after the operator explicitly requested no further automated action.
func TestEmergencyDryRunBypassesLiveOnlyEvidenceAndArmingGates(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{
		Name: "n1", AgentArming: types.AgentArmingUnarmed, AgentLastSeen: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2, DryRun: false})
	c := New(st, st, nil, gate, nil, nil, nil, &notify.Log{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	dryLimits := safety.Limits{MaxConcurrentRemediations: 2, DryRun: true}
	if err := c.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &dryLimits}); err != nil {
		t.Fatal(err)
	}
	inc := &types.Incident{ID: "inc-emergency-stop", Target: types.Target{Node: "n1", GPUUUID: "GPU-1"}, DryRun: false}
	if err := c.allowAcceleratorStep(ctx, inc, &playbook.Step{Action: "agent.gpu_reset"}); err != nil {
		t.Fatalf("dry-run reset must not wait for live accelerator evidence: %v", err)
	}
	if reason, verdict := c.refuseUnarmedAgent(ctx, inc, &playbook.Step{Action: "agent.reboot"}); verdict != armingProceed || reason != "" {
		t.Fatalf("dry-run reboot arming = %q/%v, want proceed; a stop must not park a live incident behind an unarmed agent", reason, verdict)
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
	dryLimits := safety.Limits{MaxConcurrentRemediations: 2, DryRun: true}
	if err := c.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &dryLimits}); err != nil {
		t.Fatal(err)
	}
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
