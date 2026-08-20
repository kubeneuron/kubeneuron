package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/actuator"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// stackPlatform is a platform that can quiesce, and records what happened.
type stackPlatform struct {
	platform.Platform
	quiesced      []string
	restored      []string
	quiescedNodes []string
	quiesceErr    error
	nodeExists    map[string]bool
	nodeErr       error
}

func (p *stackPlatform) Name() string { return "stack-test" }

func (p *stackPlatform) ListNodes(context.Context) ([]types.Node, error) {
	return []types.Node{{
		Name: "node-a", UID: "node-uid-a", Labels: map[string]string{"accelerator": "nvidia-h100"},
	}}, nil
}

func (p *stackPlatform) NodeExists(_ context.Context, node string) (bool, error) {
	if p.nodeErr != nil {
		return false, p.nodeErr
	}
	if p.nodeExists != nil {
		return p.nodeExists[node], nil
	}
	return node == "node-a", nil
}

func (p *stackPlatform) QuiesceAcceleratorStack(context.Context, string) ([]string, error) {
	if p.quiesceErr != nil {
		return nil, p.quiesceErr
	}
	p.quiesced = append(p.quiesced, "dcgm")
	p.quiescedNodes = []string{"node-a"}
	return []string{"dcgm"}, nil
}

func (p *stackPlatform) RestoreAcceleratorStack(context.Context, string) ([]string, error) {
	p.restored = append(p.restored, "dcgm")
	p.quiescedNodes = nil
	return []string{"dcgm"}, nil
}

func (p *stackPlatform) QuiescedNodes(context.Context) ([]string, error) {
	return p.quiescedNodes, nil
}

// hostActuator answers the agent-side half of a quiesce: the node reports what
// its own process table says, which is what replaced the controller's guess.
type hostActuator struct {
	actions []types.ActionType
	output  string
	err     error
}

func (a *hostActuator) Name() string { return "host-test" }

func (a *hostActuator) Capabilities() []types.ActionType {
	return []types.ActionType{types.ActionQuiesceAcceleratorHost, types.ActionRestoreAcceleratorHost}
}

func (a *hostActuator) Healthy(context.Context, types.Node) error { return nil }

func (a *hostActuator) Execute(_ context.Context, _ types.Node, act types.Action) (*types.ActionResult, error) {
	a.actions = append(a.actions, act.Type)
	if a.err != nil {
		return &types.ActionResult{OK: false, Error: a.err.Error()}, nil
	}
	return &types.ActionResult{OK: true, Output: a.output}, nil
}

func stackTestController(t *testing.T, p platform.Platform) (*Controller, *storesqlite.Store) {
	return stackTestControllerWithActuator(t, p, &hostActuator{output: "GPU 0 released"})
}

func stackTestControllerWithActuator(t *testing.T, p platform.Platform, act actuator.Actuator) (*Controller, *storesqlite.Store) {
	t.Helper()
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// A real gate, not nil: executeStep asks the LIVE gate whether this step
	// must be simulated, and a controller with no gate at all fails closed —
	// so a fixture that omits it turns every step into a no-op and the tests
	// below would pass without reaching the actuator they exist to observe.
	c := New(st, nil, nil, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2}),
		nil, p, act, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{
		Name: "node-a", UID: "node-uid-a", Labels: map[string]string{"accelerator": "nvidia-h100"},
	}); err != nil {
		t.Fatal(err)
	}
	profile := testNVIDIAResetProfile()
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{profile}); err != nil {
		t.Fatal(err)
	}
	report := readyNVIDIAResetReport(time.Now().UTC(), profile.ProfileDigest)
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}
	return c, st
}

func resetIncident() *types.Incident {
	return &types.Incident{
		ID:     "inc-reset",
		Target: types.Target{Node: "node-a", GPUUUID: "GPU-a"},
		State:  types.StateExecuting,
	}
}

// This is the deadlock the whole change exists for: quiescing DCGM is what
// makes a reset possible, and it is also what destroys the attestation the
// reset gate reads. Evidence captured at quiesce time keeps the gate honest
// without making it impossible to satisfy.
func TestQuiescePinsEvidenceSoTheResetGateSurvivesDCGMGoingAway(t *testing.T) {
	p := &stackPlatform{}
	c, st := stackTestController(t, p)
	ctx := context.Background()
	inc := resetIncident()

	res, err := c.quiesceAcceleratorStack(ctx, inc, &playbook.Step{Name: "quiesce"})
	if err != nil {
		t.Fatalf("quiesce = %v", err)
	}
	if !strings.Contains(res.Output, "dcgm") {
		t.Fatalf("message = %q, want the stopped components named", res.Output)
	}

	// The agent's next report has no attested runtime version, because there is
	// no engine left to attest against. That is exactly what happened on real
	// hardware, and it denied the reset the quiesce was performed for.
	degraded := readyNVIDIAResetReport(time.Now().UTC(), testNVIDIAResetProfile().ProfileDigest)
	degraded.RuntimeVersion = ""
	degraded.Readiness = types.AcceleratorReadinessDegraded
	degraded.ReadinessReasons = []string{"DCGM runtime version probe failed: no host engine"}
	if err := st.UpsertAcceleratorReport(ctx, &degraded); err != nil {
		t.Fatal(err)
	}

	if err := c.allowNVIDIAReset(ctx, inc, inc.Target); err != nil {
		t.Fatalf("reset gate after quiesce = %v, want the pinned evidence to be accepted", err)
	}

	// A different incident gets no benefit from this incident's pin.
	other := &types.Incident{ID: "inc-other", Target: inc.Target}
	if err := c.allowNVIDIAReset(ctx, other, other.Target); err == nil {
		t.Fatal("a pin must be scoped to the incident that took it")
	}
}

func TestQuiesceRefusesToStopMonitoringWithoutResetGradeEvidence(t *testing.T) {
	p := &stackPlatform{}
	c, st := stackTestController(t, p)
	ctx := context.Background()

	degraded := readyNVIDIAResetReport(time.Now().UTC(), testNVIDIAResetProfile().ProfileDigest)
	degraded.Readiness = types.AcceleratorReadinessDegraded
	degraded.ReadinessReasons = []string{"NVIDIA runtime is not attested"}
	if err := st.UpsertAcceleratorReport(ctx, &degraded); err != nil {
		t.Fatal(err)
	}

	if _, err := c.quiesceAcceleratorStack(ctx, resetIncident(), &playbook.Step{Name: "quiesce"}); err == nil {
		t.Fatal("must not stop monitoring when the reset it enables could never be admitted")
	}
	if len(p.quiesced) != 0 {
		t.Fatal("nothing may be switched off when the evidence is not good enough")
	}
}

func TestPinnedEvidenceExpiresWithTheReportItCameFrom(t *testing.T) {
	c, _ := stackTestController(t, &stackPlatform{})
	inc := resetIncident()
	report := readyNVIDIAResetReport(time.Now().UTC().Add(-time.Hour), "digest")
	c.pinAcceleratorEvidence(inc.ID, pinnedAcceleratorEvidence{
		node: "node-a", nodeUID: "node-uid-a", report: report, pinnedAt: time.Now(),
	})
	if _, ok := c.takePinnedAcceleratorEvidence(inc.ID, time.Now()); ok {
		t.Fatal("stale evidence must not be usable just because it was pinned")
	}
}

// A playbook that quiesces and then fails must not leave the cluster's GPU
// monitoring switched off.
func TestAbandonedQuiesceIsRestoredOnceTheIncidentStopsRunning(t *testing.T) {
	p := &stackPlatform{}
	c, st := stackTestController(t, p)
	ctx := context.Background()
	inc := resetIncident()
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if _, err := c.quiesceAcceleratorStack(ctx, inc, &playbook.Step{Name: "quiesce"}); err != nil {
		t.Fatal(err)
	}

	// Still running: monitoring stays off, the playbook owns it.
	c.restoreAbandonedAcceleratorStacks(ctx)
	if len(p.restored) != 0 {
		t.Fatal("must not restore under a running playbook")
	}

	inc.State = types.StateNeedsHuman
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	c.restoreAbandonedAcceleratorStacks(ctx)
	if len(p.restored) != 1 {
		t.Fatalf("restored = %v, want monitoring put back once the incident stopped", p.restored)
	}
	// Both halves, not just the Kubernetes one: a node left with its
	// persistence daemon stopped is a node quietly changed by a playbook that
	// failed, with nothing left running to notice.
	act := c.actuator.(*hostActuator)
	restoredHost := false
	for _, a := range act.actions {
		if a == types.ActionRestoreAcceleratorHost {
			restoredHost = true
		}
	}
	if !restoredHost {
		t.Fatalf("agent actions = %v, want the node's own state restored too", act.actions)
	}

	// And it does not thrash: the pin is dropped after a successful restore.
	c.restoreAbandonedAcceleratorStacks(ctx)
	if len(p.restored) != 1 {
		t.Fatalf("restored = %v, want exactly one restore", p.restored)
	}
}

// The bug this replaced: the controller inferred "the stack has settled" from
// pod labels, and on a real cluster a device plugin installed by the machine
// image carried labels it did not recognise. It reported a quiet stack while
// the plugin still held /dev/nvidia0, and the reset that followed was doomed.
// The node now answers from its own process table, and a node that says the
// device is still held must fail the step.
func TestQuiesceFailsWhenTheNodeSaysHoldersRemain(t *testing.T) {
	act := &hostActuator{err: errors.New(
		"quiesce_accelerator_host: GPU 0 is still held by 1 process(es): nvidia-device-p(11567) holds /dev/nvidia0")}
	c, _ := stackTestControllerWithActuator(t, &stackPlatform{}, act)

	_, err := c.quiesceAcceleratorStack(context.Background(), resetIncident(), &playbook.Step{Name: "quiesce"})
	if err == nil {
		t.Fatal("a node reporting remaining holders must fail the quiesce, not report success")
	}
	if !strings.Contains(err.Error(), "nvidia-device-p") {
		t.Fatalf("err = %v, want the holder the node named", err)
	}
}

func TestRestorePutsBackBothHalves(t *testing.T) {
	act := &hostActuator{output: "nvidia-persistenced started"}
	p := &stackPlatform{}
	c, _ := stackTestControllerWithActuator(t, p, act)
	inc := resetIncident()

	res, err := c.restoreAcceleratorStackStep(context.Background(), inc, &playbook.Step{Name: "restore"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.restored) != 1 {
		t.Fatalf("platform restores = %v, want the vendor components put back", p.restored)
	}
	if len(act.actions) != 1 || act.actions[0] != types.ActionRestoreAcceleratorHost {
		// The node's own holders (the persistence daemon) are the other half;
		// restoring only the Kubernetes side would leave the node changed.
		t.Fatalf("agent actions = %v, want the host restore too", act.actions)
	}
	if !strings.Contains(res.Output, "nvidia-persistenced") {
		t.Fatalf("output = %q, want the node's own result reported", res.Output)
	}
}

// The Kubernetes marker is the durable recovery queue. It must remain until
// the node has confirmed that the host-side state was restored; clearing it
// first would make a transient agent outage strand a stopped persistence
// daemon forever.
func TestFailedHostRestoreRetainsPlatformRecoveryMarker(t *testing.T) {
	act := &hostActuator{err: errors.New("agent unavailable")}
	p := &stackPlatform{quiescedNodes: []string{"node-a"}}
	c, _ := stackTestControllerWithActuator(t, p, act)

	if _, err := c.restoreAcceleratorStackStep(context.Background(), resetIncident(), &playbook.Step{Name: "restore"}); err == nil {
		t.Fatal("host restore failure must fail the step")
	}
	if len(p.restored) != 0 || len(p.quiescedNodes) != 1 {
		t.Fatalf("platform state = restored %v, pending %v; marker must stay for recovery", p.restored, p.quiescedNodes)
	}

	act.err = nil
	if _, err := c.restoreAcceleratorStackStep(context.Background(), resetIncident(), &playbook.Step{Name: "restore"}); err != nil {
		t.Fatalf("retry restore = %v", err)
	}
	if len(p.restored) != 1 || len(p.quiescedNodes) != 0 {
		t.Fatalf("platform state = restored %v, pending %v; successful retry must clear marker", p.restored, p.quiescedNodes)
	}
}

// A step that ends in the agent's own failure must report that failure, not the
// controller's impatience. With identical deadlines the controller always won
// the race, and a precise "GPU 0 is still held by nvidia-device-plugin(...)"
// reached the operator as a bare "context deadline exceeded".
func TestControllerOutwaitsTheAgentsOwnActionBudget(t *testing.T) {
	if agentResultGrace <= 0 {
		t.Fatal("the controller must allow the agent to report before giving up")
	}
	step := &playbook.Step{Name: "quiesce", Timeout: playbook.Duration(5 * time.Minute)}
	deadline := step.Timeout.Std() + agentResultGrace
	if deadline <= step.Timeout.Std() {
		t.Fatalf("controller deadline %s must exceed the agent's %s", deadline, step.Timeout.Std())
	}
}

// A controller that restarts mid-playbook has no pinned evidence and no memory
// of which nodes it left quiesced. Recovery therefore starts from the platform,
// not from this process: monitoring comes back, and the incident is sent back to
// its quiesce step so the whole sequence re-attests from scratch.
//
// Persisting the pin instead would have made a snapshot taken before a crash
// durable, which is exactly what a fail-closed gate must not accept.
func TestRestartedControllerRecoversAQuiescedNodeAndRewindsTheIncident(t *testing.T) {
	// A fresh controller: empty pin map, but the platform still reports the
	// node as quiesced by whoever ran before.
	p := &stackPlatform{quiescedNodes: []string{"node-a"}}
	act := &hostActuator{output: "restored"}
	c, st := stackTestControllerWithActuator(t, p, act)
	ctx := context.Background()

	book := &playbook.Playbook{Name: "reset", Target: "gpu", Steps: []playbook.Step{
		{Name: "drain", Action: "platform.drain"},
		{Name: "quiesce", Action: "platform.quiesce_accelerator_stack"},
		{Name: "reset", Action: "agent.gpu_reset"},
	}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"reset": book}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)

	inc := resetIncident()
	inc.Playbook = "reset"
	inc.StepIndex = 2 // past the quiesce, about to reset
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	c.restoreAbandonedAcceleratorStacks(ctx)

	if len(p.restored) != 1 {
		t.Fatalf("restored = %v, want the node put back", p.restored)
	}
	restoredHost := false
	for _, a := range act.actions {
		if a == types.ActionRestoreAcceleratorHost {
			restoredHost = true
		}
	}
	if !restoredHost {
		t.Fatalf("agent actions = %v, want the node's own state restored too", act.actions)
	}

	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StepIndex != 1 {
		// Resetting a node whose stack came back up would simply fail; the
		// quiesce has to run again.
		t.Fatalf("step index = %d, want the incident rewound to its quiesce step", got.StepIndex)
	}
}

// Rewinding a completed quiesce must give it a fresh action identity. Action IDs
// are hash(incident, stepIndex, attempt); if the rewind reused attempt 0 the
// rewound quiesce would recompute the same ID as the original, already-'done'
// action and the executor would replay the stale success without standing the
// vendor stack down — then the reset would run against a node the janitor just
// brought back up. Bumping Attempt gives the rewound step a non-colliding ID so
// it actually re-executes.
func TestRewoundQuiesceGetsAFreshActionIDSoItReExecutes(t *testing.T) {
	p := &stackPlatform{quiescedNodes: []string{"node-a"}}
	act := &hostActuator{output: "restored"}
	c, st := stackTestControllerWithActuator(t, p, act)
	ctx := context.Background()

	book := &playbook.Playbook{Name: "reset", Target: "gpu", Steps: []playbook.Step{
		{Name: "drain", Action: "platform.drain"},
		{Name: "quiesce", Action: "platform.quiesce_accelerator_stack"},
		{Name: "reset", Action: "agent.gpu_reset"},
	}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"reset": book}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)

	const quiesceIndex = 1
	inc := resetIncident()
	inc.Playbook = "reset"
	inc.StepIndex = 2 // quiesce already completed, about to reset
	inc.Attempt = 0
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// The identity the original, already-'done' quiesce action computed.
	originalQuiesceID := actionID(&types.Incident{ID: inc.ID, StepIndex: quiesceIndex, Attempt: 0})

	c.rewindIncidentsToQuiesce(ctx, "node-a")

	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StepIndex != quiesceIndex {
		t.Fatalf("step index = %d, want rewound to quiesce step %d", got.StepIndex, quiesceIndex)
	}
	if got.Attempt == 0 {
		t.Fatalf("attempt = 0 after rewind; the rewound step must not reuse the completed action's identity")
	}
	if id := actionID(got); id == originalQuiesceID {
		t.Fatalf("rewound quiesce action ID %s collides with the completed original; it would replay the stale 'done' result instead of re-executing", id)
	}
}

// TestRestoreAttemptGetsAFreshIdentity covers the half of the janitor's restore
// that rounds 18 and 19 fixed in the controller and left broken in the agent.
//
// A per-node deterministic ID is right while an attempt is in flight — a
// janitor pass that returns before the agent finishes must re-attach rather
// than stack a second action. It is wrong once that attempt has terminated,
// because the AGENT keeps a durable journal keyed on the same ID and never
// forgets: re-dispatching a reported ID is refused with "cannot claim reported
// action", so the restore silently never runs again. Clearing the queue row
// and leaving the journal made the fix correct in intent and inert in fact.
func TestRestoreAttemptGetsAFreshIdentity(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(st, st, nil, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2}),
		nil, nil, nil, &notify.Log{Logger: log}, log)
	ctx := context.Background()

	first, err := c.restoreActionID(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}

	// While that attempt is in flight, a later pass must re-attach to it.
	if err := st.EnqueueAction(ctx, "n1", types.Action{
		ID: first, Type: types.ActionRestoreAcceleratorHost, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	again, err := c.restoreActionID(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("a pass while the attempt is still in flight minted a new action (%s != %s); "+
			"the janitor would stack a second restore beside the one the agent is running", again, first)
	}

	// Once it terminates, the next attempt must be a DIFFERENT action — the
	// agent's journal will refuse the old identity forever.
	claimed, err := st.ClaimNextAction(ctx, "n1", "boot-1", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claiming the first attempt: %v", err)
	}
	if err := st.CompleteClaimedAction(ctx, first, claimed.LeaseToken, "boot-1",
		types.ActionResult{ActionID: first, OK: false, Output: "attempt 1 failed"}); err != nil {
		t.Fatal(err)
	}
	next, err := c.restoreActionID(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if next == first {
		t.Fatal("the attempt after a terminated one reused its action ID; the agent's durable " +
			"journal refuses a reported ID, so this node's restore would never run again")
	}

	// And the finished row is gone, so a node needing many attempts does not
	// accumulate one row per attempt until retention.
	if _, err := st.GetAction(ctx, first); err == nil {
		t.Fatal("the terminated attempt's row was left behind")
	}
}
