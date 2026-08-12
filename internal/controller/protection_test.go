package controller

// §3.1 of docs/definition-plan.md: the system protects workloads constantly and
// none of it was countable. These tests assert the counters move on the REAL
// refusal paths — one per guard, driven through the same functions the walk
// calls — and, just as importantly, that an execution which disrupts nothing
// unusual leaves them all alone. A protection metric that also counts normal
// operation says nothing.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kubeneuron/kubeneuron/internal/actuator"
	"github.com/kubeneuron/kubeneuron/internal/cloud"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// allDeferralReasons is every label the protection counter may carry. The
// snapshot/assert helpers below walk it, so a test that expects one reason also
// proves no OTHER reason fired — which is how a mislabelled path gets caught.
var allDeferralReasons = []string{
	metrics.DeferNotIdle, metrics.DeferDeviceHolders, metrics.DeferMaintenanceWindow,
	metrics.DeferNodePaused, metrics.DeferConcurrencyCap, metrics.DeferPlaybookCooldown,
	metrics.DeferUnarmedAgent, metrics.DeferConfinement, metrics.DeferRecycleNotViable,
	metrics.DeferGlobalPause, metrics.DeferAcceleratorEvidence,
}

func snapshotDeferrals() map[string]float64 {
	out := make(map[string]float64, len(allDeferralReasons))
	for _, reason := range allDeferralReasons {
		out[reason] = testutil.ToFloat64(metrics.DestructiveStepsDeferred.WithLabelValues(reason))
	}
	return out
}

// assertDeferred requires the named reason to have moved by at least one and
// every other reason to be untouched.
func assertDeferred(t *testing.T, before map[string]float64, want string) {
	t.Helper()
	after := snapshotDeferrals()
	for _, reason := range allDeferralReasons {
		delta := after[reason] - before[reason]
		switch {
		case reason == want && delta < 1:
			t.Fatalf("deferrals[%s] did not move (delta %v); this guard is uncounted", reason, delta)
		case reason != want && delta != 0:
			t.Fatalf("deferrals[%s] moved by %v; only %s should have", reason, delta, want)
		}
	}
}

func assertNoDeferrals(t *testing.T, before map[string]float64) {
	t.Helper()
	after := snapshotDeferrals()
	for _, reason := range allDeferralReasons {
		if delta := after[reason] - before[reason]; delta != 0 {
			t.Fatalf("deferrals[%s] moved by %v; a normal run must not look like protection", reason, delta)
		}
	}
}

// disruptivePlaybook is a two-rung ladder whose first step really would take
// workloads away, so every guard under test has something to protect.
func disruptivePlaybook() *playbook.Playbook {
	return &playbook.Playbook{Name: "prot", Target: "node", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon"},
		{Name: "drain", Action: "platform.drain"},
	}}
}

// protectionFixture wires a controller around one playbook, with a node in
// inventory and an EVALUATING incident bound to it.
func protectionFixture(t *testing.T, book *playbook.Playbook, gate *safety.Gate, plat platform.Platform, act actuator.Actuator) (*Controller, *sqlite.Store, *types.Incident) {
	t.Helper()
	return protectionFixtureBooks(t, book, map[string]*playbook.Playbook{book.Name: book}, gate, plat, act)
}

// protectionFixtureBooks is protectionFixture with a whole playbook set, so a
// test can give the ladder a real escalation target.
func protectionFixtureBooks(t *testing.T, book *playbook.Playbook, books map[string]*playbook.Playbook, gate *safety.Gate, plat platform.Platform, act actuator.Actuator) (*Controller, *sqlite.Store, *types.Incident) {
	t.Helper()
	engine, err := playbook.NewEngine(books, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if gate == nil {
		gate = safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1})
	}
	c := New(st, st, engine, gate, nil, plat, act, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{
		Name: "n1", UID: "n1-uid", Labels: map[string]string{"pool": "prod"},
		AgentLastSeen: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	inc := &types.Incident{
		ID: "inc-prot", Target: types.Target{Node: "n1", GPUUUID: "GPU-1"},
		Class: types.ClassFellOffBus, State: types.StateEvaluating, Playbook: book.Name,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	return c, st, inc
}

// A paused node is an operator saying "leave this one alone". Holding there is
// the product working, and until now it left no trace at all.
func TestNodePauseIsCountedAsProtection(t *testing.T) {
	c, st, inc := protectionFixture(t, disruptivePlaybook(), nil, nil, nil)
	ctx := context.Background()
	if err := st.ApplyNodeConfigPauses(ctx, []string{"n1"}); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeferrals()
	if err := c.advanceEvaluating(ctx, inc); err != nil {
		t.Fatal(err)
	}
	assertDeferred(t, before, metrics.DeferNodePaused)
}

func TestMaintenanceWindowIsCountedAsProtection(t *testing.T) {
	c, _, inc := protectionFixture(t, disruptivePlaybook(), nil, nil, nil)
	c.SetMaintenanceWindows([]config.MaintenanceWindow{{
		Name:     "now",
		StartsAt: time.Now().Add(-time.Hour),
		EndsAt:   time.Now().Add(time.Hour),
	}})
	before := snapshotDeferrals()
	if err := c.advanceEvaluating(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	assertDeferred(t, before, metrics.DeferMaintenanceWindow)
}

func TestPlaybookCooldownIsCountedAsProtection(t *testing.T) {
	c, _, inc := protectionFixture(t, disruptivePlaybook(), nil, nil, nil)
	c.gate.RecordCooldown(inc.Target, playbookCooldownAction(inc.Playbook), time.Hour)
	before := snapshotDeferrals()
	if err := c.advanceEvaluating(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	assertDeferred(t, before, metrics.DeferPlaybookCooldown)
}

// The cap that stops a fleet-wide fault from draining half the cluster is the
// single most valuable thing in this metric.
func TestConcurrencyCapIsCountedAsProtection(t *testing.T) {
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1})
	gate.OccupyRemediation(types.Target{Node: "someone-else"})
	c, _, inc := protectionFixture(t, disruptivePlaybook(), gate, nil, nil)
	before := snapshotDeferrals()
	if err := c.advanceEvaluating(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	assertDeferred(t, before, metrics.DeferConcurrencyCap)
}

// The big red button is fleet-wide and must not be confused with one node's
// pause: an operator reading the metric has to know which lever is holding.
func TestGlobalPauseIsCountedSeparatelyFromANodePause(t *testing.T) {
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1})
	gate.Pause()
	c, _, inc := protectionFixture(t, disruptivePlaybook(), gate, nil, nil)
	before := snapshotDeferrals()
	if err := c.advanceEvaluating(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	assertDeferred(t, before, metrics.DeferGlobalPause)
}

func TestBlastRadiusConfinementIsCountedAsProtection(t *testing.T) {
	c, _, inc := protectionFixture(t, disruptivePlaybook(), nil, nil, nil)
	c.SetDestructiveNodeSelector(map[string]string{"pool": "canary"}) // n1 is pool=prod
	before := snapshotDeferrals()
	if err := c.advanceEvaluating(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	assertDeferred(t, before, metrics.DeferConfinement)
}

func TestUnarmedAgentRefusalIsCountedAsProtection(t *testing.T) {
	book := &playbook.Playbook{Name: "prot", Target: "node", Steps: []playbook.Step{
		{Name: "reboot", Action: "agent.reboot", Approval: "required"},
	}}
	c, st, inc := protectionFixture(t, book, nil, nil, nil)
	ctx := context.Background()
	if err := st.UpsertAgentRegistration(ctx, &types.Node{
		Name: "n1", AgentLastSeen: time.Now(), AgentArming: types.AgentArmingUnarmed,
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeferrals()
	if err := c.advanceEvaluating(ctx, inc); err != nil {
		t.Fatal(err)
	}
	assertDeferred(t, before, metrics.DeferUnarmedAgent)
}

func TestUnviableRecycleIsCountedAsProtection(t *testing.T) {
	book := &playbook.Playbook{Name: "prot", Target: "node", Steps: []playbook.Step{
		{Name: "recycle", Action: "platform.recycle_node", Approval: "required"},
	}}
	plat := &recyclePlatform{configured: true, checkErr: cloud.ErrRecycleNotViable}
	c, _, inc := protectionFixture(t, book, nil, plat, nil)
	before := snapshotDeferrals()
	if err := c.advanceEvaluating(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	assertDeferred(t, before, metrics.DeferRecycleNotViable)
}

// Processes hold the GPU that KubeNeuron cannot release, so the reset ladder is
// refused before it cordons and drains a node for nothing.
func TestDeviceHoldersAreCountedAsProtection(t *testing.T) {
	book := &playbook.Playbook{Name: "prot", Target: "gpu", Steps: []playbook.Step{
		{Name: "reset", Action: "agent.gpu_reset"},
	}}
	c, st, inc := protectionFixture(t, book, nil, nil, nil)
	ctx := context.Background()
	report := readyNVIDIAResetReport(time.Now().UTC(), "")
	report.Node, report.NodeUID = "n1", "n1-uid"
	report.Devices[0].ID = inc.Target.GPUUUID
	// A process KubeNeuron has no way to release: the node-side preflight would
	// find it eventually, but only after a cordon and a drain spent for nothing.
	report.DeviceHolders = []types.AgentDeviceHolder{
		{Command: "nv-fabricmanager", PID: 42, Device: "/dev/nvidia0"},
	}
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeferrals()
	if err := c.advanceEvaluating(ctx, inc); err != nil {
		t.Fatal(err)
	}
	assertDeferred(t, before, metrics.DeferDeviceHolders)
}

// Missing runtime evidence HOLDS the reset — the device keeps running whatever
// it is running — which is the most common reason a reset never happens.
func TestMissingAcceleratorEvidenceIsCountedAsProtection(t *testing.T) {
	book := &playbook.Playbook{Name: "prot", Target: "gpu", Steps: []playbook.Step{
		{Name: "reset", Action: "agent.gpu_reset"},
	}}
	c, _, inc := protectionFixture(t, book, nil, nil, nil)
	before := snapshotDeferrals()
	if err := c.startStep(context.Background(), inc, &book.Steps[0], "system"); err != nil {
		t.Fatal(err)
	}
	assertDeferred(t, before, metrics.DeferAcceleratorEvidence)
}

// refusingActuator stands in for an agent whose idle guard reports back. The
// refusal code is what distinguishes "the device is busy" from "the probe
// broke": both fail the step, only the first spared a workload.
type refusingActuator struct {
	err     error
	refusal string
}

func (a refusingActuator) Name() string                              { return "refusing" }
func (a refusingActuator) Capabilities() []types.ActionType          { return nil }
func (a refusingActuator) Healthy(context.Context, types.Node) error { return nil }
func (a refusingActuator) Execute(context.Context, types.Node, types.Action) (*types.ActionResult, error) {
	return &types.ActionResult{OK: false, Error: a.err.Error(), Refusal: a.refusal}, nil
}

// idleRefusalFixture wires a guarded playbook that HAS somewhere to escalate
// to. The escalation target is load-bearing: without it the ladder quarantines
// on exhaustion anyway, and a test built on such a playbook passes whether the
// guard stops the ladder or not — proving nothing about the thing it names.
func idleRefusalFixture(t *testing.T, refusal string) (*Controller, *types.Incident, *playbook.Playbook) {
	t.Helper()
	guarded := &playbook.Playbook{
		Name: "prot", Target: "gpu",
		Steps: []playbook.Step{
			{Name: "idle", Action: "agent.idle_check"},
			{Name: "reset", Action: "agent.gpu_reset"},
		},
		OnFailure: playbook.OnFailure{EscalateTo: "hammer"},
	}
	// Strictly more destructive than the rung the guard protects — which is
	// what makes escalating past a guard the wrong answer.
	hammer := &playbook.Playbook{Name: "hammer", Target: "node", Steps: []playbook.Step{
		{Name: "reboot", Action: "agent.reboot"},
	}}
	act := refusingActuator{err: errors.New("GPU 0 is still held by trainer(11621)"), refusal: refusal}
	c, st, inc := protectionFixtureBooks(t, guarded, map[string]*playbook.Playbook{
		guarded.Name: guarded, hammer.Name: hammer,
	}, nil, nil, act)
	inc.State = types.StateExecuting
	if err := st.UpdateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	return c, inc, guarded
}

// An idle guard that refuses is not a malfunction: it is the device saying it
// is still working, and the destructive rung behind it not running.
func TestIdleGuardRefusalIsCountedAsProtection(t *testing.T) {
	c, inc, book := idleRefusalFixture(t, types.RefusalNotIdle)
	before := snapshotDeferrals()
	c.runStep(context.Background(), c.currentEngine(), inc, &book.Steps[0])
	assertDeferred(t, before, metrics.DeferNotIdle)
}

// A broken idle probe fails the same step and fails just as closed, but it is
// not evidence that anybody's job was spared. Counting it would report a
// missing nvidia-smi as value delivered.
func TestABrokenIdleProbeIsNotCountedAsProtection(t *testing.T) {
	c, inc, book := idleRefusalFixture(t, "") // no refusal code: the probe could not run
	before := snapshotDeferrals()
	c.runStep(context.Background(), c.currentEngine(), inc, &book.Steps[0])
	if delta := snapshotDeferrals()[metrics.DeferNotIdle] - before[metrics.DeferNotIdle]; delta != 0 {
		t.Fatalf("deferrals[not_idle] moved by %v for a probe that never ran; "+
			"a broken driver must not be reported as a protected workload", delta)
	}
}

// The guard exists to stop the destructive rung. Escalating past it reaches for
// a BIGGER hammer than the one just refused — which turns the guard into a
// trigger. Whether to end a running job is a human's decision.
//
// The table is the point: the refusal CODE must not decide this. An agent that
// predates the field sends none, and docs/upgrade.md mandates controller-first,
// so every not-yet-upgraded node in a rolling upgrade lands in the second row.
// Reading that absence as "not a refusal" escalated to a more destructive rung
// on a device the guard had just failed to clear.
func TestAnyIdleGuardFailureHandsOverInsteadOfEscalating(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal string
	}{
		{"agent reports the device is busy", types.RefusalNotIdle},
		{"agent predates the refusal field", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, inc, book := idleRefusalFixture(t, tc.refusal)
			ctx := context.Background()
			c.runStep(ctx, c.currentEngine(), inc, &book.Steps[0])

			got, err := c.store.GetIncident(ctx, inc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != types.StateNeedsHuman {
				t.Fatalf("state = %s, want NEEDS_HUMAN: a guard that did not clear the device "+
					"must never let the ladder climb to a more destructive step", got.State)
			}
		})
	}
}

// A step failure that is NOT an idle guard is a failure, full stop. Counting it
// as protection would let a broken reset masquerade as a spared workload.
func TestAnOrdinaryStepFailureIsNotProtection(t *testing.T) {
	book := &playbook.Playbook{Name: "prot", Target: "gpu", Steps: []playbook.Step{
		{Name: "diag", Action: "agent.run_diag"},
	}}
	act := refusingActuator{err: errors.New("agent unreachable")}
	c, st, inc := protectionFixture(t, book, nil, nil, act)
	ctx := context.Background()
	inc.State = types.StateExecuting
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeferrals()
	c.runStep(ctx, c.currentEngine(), inc, &book.Steps[0])
	assertNoDeferrals(t, before)
}

// A dry-run incident executes nothing, so holding it protected nothing. If
// dry-run deferrals counted, every dry-run install — which is the default —
// would report protection it never performed.
func TestDryRunDeferralsAreNotCounted(t *testing.T) {
	c, st, inc := protectionFixture(t, disruptivePlaybook(), nil, nil, nil)
	ctx := context.Background()
	inc.DryRun = true
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyNodeConfigPauses(ctx, []string{"n1"}); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeferrals()
	if err := c.advanceEvaluating(ctx, inc); err != nil {
		t.Fatal(err)
	}
	assertNoDeferrals(t, before)
}

// An observe-only ladder was never going to touch a workload, so holding it is
// not protection either.
func TestObserveOnlyPlaybookDeferralsAreNotCounted(t *testing.T) {
	book := &playbook.Playbook{Name: "prot", Target: "gpu", Steps: []playbook.Step{
		{Name: "note", Action: "notify.observe"},
	}}
	c, st, inc := protectionFixture(t, book, nil, nil, nil)
	ctx := context.Background()
	if err := st.ApplyNodeConfigPauses(ctx, []string{"n1"}); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeferrals()
	if err := c.advanceEvaluating(ctx, inc); err != nil {
		t.Fatal(err)
	}
	assertNoDeferrals(t, before)
}

// evictPlatform reports one GPU pod and one CPU pod on the node.
type evictPlatform struct {
	platform.Platform
	evicted []string
}

func (p *evictPlatform) Name() string { return "evict-test" }

func (p *evictPlatform) NodeWorkloads(context.Context, string) ([]platform.Workload, error) {
	return []platform.Workload{
		{Name: "trainer", Namespace: "ml", Kind: "Pod", UsesGPU: true},
		{Name: "sidecar-metrics", Namespace: "ml", Kind: "Pod", UsesGPU: false},
	}, nil
}

func (p *evictPlatform) EvictWorkload(_ context.Context, w platform.Workload) error {
	p.evicted = append(p.evicted, w.Namespace+"/"+w.Name)
	return nil
}

// Evictions are the visible half of "protects workloads": what the remediation
// cost, per node and per fault class.
func TestEvictedGPUWorkloadsAreCounted(t *testing.T) {
	plat := &evictPlatform{}
	c, _, inc := protectionFixture(t, disruptivePlaybook(), nil, plat, nil)
	ctx := context.Background()
	before := testutil.ToFloat64(metrics.WorkloadsEvicted.WithLabelValues(string(types.ClassFellOffBus)))
	nonGPU := testutil.ToFloat64(metrics.WorkloadsEvicted.WithLabelValues("cpu"))

	if _, err := c.executePlatformStep(ctx, inc, "evict_gpu_workload",
		&playbook.Step{Name: "evict", Action: "platform.evict_gpu_workload"}); err != nil {
		t.Fatal(err)
	}
	if len(plat.evicted) != 1 || plat.evicted[0] != "ml/trainer" {
		t.Fatalf("evicted %v, want only the GPU workload", plat.evicted)
	}
	after := testutil.ToFloat64(metrics.WorkloadsEvicted.WithLabelValues(string(types.ClassFellOffBus)))
	if after-before != 1 {
		t.Fatalf("evictions counted = %v, want exactly the one GPU workload", after-before)
	}
	if got := testutil.ToFloat64(metrics.WorkloadsEvicted.WithLabelValues("cpu")); got != nonGPU {
		t.Fatal("a workload that holds no GPU must not appear in the eviction count")
	}
}

// The control case, and the one that makes the metric mean anything: a step
// admitted and executed normally is not a deferral.
func TestNormalExecutionDefersNothing(t *testing.T) {
	book := &playbook.Playbook{Name: "prot", Target: "node", Steps: []playbook.Step{
		{Name: "diag", Action: "agent.run_diag"},
	}}
	c, st, inc := protectionFixture(t, book, nil, nil, &hostActuator{output: "ok"})
	ctx := context.Background()
	before := snapshotDeferrals()
	if err := c.advanceEvaluating(ctx, inc); err != nil {
		t.Fatal(err)
	}
	waitForIncident(t, st, inc.ID, func(i *types.Incident) bool { return !c.isInFlight(i.ID) && i.State != types.StateEvaluating })
	assertNoDeferrals(t, before)
}
