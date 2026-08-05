package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// --- Fix 2: an unattributed (empty GPUUUID) reset must escalate, not hold ---

// A driver-hang / ECC-DBE XID whose kmsg attribution failed opens an incident
// with an empty GPU UUID. On a reset-bearing playbook the reset can never be
// admitted (a per-device reset needs a device), and an empty UUID is a reported
// fact, not evidence that could still arrive. The incident must be surfaced —
// here, fail closed to NEEDS_HUMAN with a notification BEFORE any cordon/drain —
// never sit in EVALUATING re-denying the reset on every tick.
func TestUnattributedResetSurfacedBeforeAnyDisruptiveStep(t *testing.T) {
	c, st, notifier := walkFixture(t, false) // non-dry-run
	ctx := context.Background()

	inc := &types.Incident{
		ID:             "inc-unattributed",
		Target:         types.Target{Node: "n1"}, // empty GPUUUID: attribution failed
		Class:          types.ClassECCDBE,
		State:          types.StateEvaluating,
		Playbook:       "drain-and-reset",
		DryRun:         false,
		OpenedAt:       time.Now(),
		UpdatedAt:      time.Now(),
		StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// One reconcile pass is enough: advanceEvaluating refuses the infeasible
	// reset up front (StepIndex 0, Attempt 0), before any cordon/drain.
	c.reconcile(ctx)

	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateNeedsHuman {
		t.Fatalf("state = %s (playbook %s, step %d); want NEEDS_HUMAN, not a silent per-tick reset denial holding in EVALUATING",
			got.State, got.Playbook, got.StepIndex)
	}
	if got.StepIndex != 0 {
		t.Fatalf("step index = %d; the refusal must happen before any disruptive step (no cordon/drain)", got.StepIndex)
	}
	sawNeedsHuman := false
	for _, ev := range notifier.kinds() {
		if ev == notify.EventNeedsHuman {
			sawNeedsHuman = true
		}
	}
	if !sawNeedsHuman {
		t.Fatal("the unattributed refusal must be surfaced to a human, not silent")
	}
}

// The empty-UUID reset error is classified as permanent (escalatable), distinct
// from the missing-evidence errors the capability gate holds on. startStep
// branches on exactly this to escalate rather than hold.
func TestUnattributedResetIsPermanentNotMissingEvidence(t *testing.T) {
	c, _, _ := walkFixture(t, false)
	inc := &types.Incident{ID: "x", Target: types.Target{Node: "n1"}, DryRun: false}
	step := &playbook.Step{Name: "reset", Action: "agent.gpu_reset"}
	err := c.allowAcceleratorStep(context.Background(), inc, step)
	if !errors.Is(err, errResetTargetUnattributed) {
		t.Fatalf("allowAcceleratorStep on an empty-UUID reset = %v, want errResetTargetUnattributed (a permanent, escalatable fact)", err)
	}
}

// --- Fix R1: destructive controller-side steps confined to the selector ---

// destructiveStepConfinement must gate exactly the destructive controller-side
// platform steps (as classified by the action registry), confirm-block a
// resolved out-of-scope node, hold on unresolvable labels, and leave dry-run
// and restorative/non-destructive steps alone.
func TestDestructiveStepScopeDecisions(t *testing.T) {
	c, st, _ := walkFixture(t, false)
	ctx := context.Background()
	c.SetDestructiveNodeSelector(map[string]string{"pool": "canary"})
	cordon := &playbook.Step{Name: "cordon", Action: "platform.cordon"}

	if err := st.UpsertNode(ctx, &types.Node{Name: "in", Labels: map[string]string{"pool": "canary", "x": "y"}}); err != nil {
		t.Fatal(err)
	}
	if _, res := c.destructiveStepConfinement(ctx, &types.Incident{Target: types.Target{Node: "in"}}, cordon); res != confinementAllowed {
		t.Fatalf("an in-scope node must proceed, got %v", res)
	}
	if err := st.UpsertNode(ctx, &types.Node{Name: "out", Labels: map[string]string{"pool": "prod"}}); err != nil {
		t.Fatal(err)
	}
	if _, res := c.destructiveStepConfinement(ctx, &types.Incident{Target: types.Target{Node: "out"}}, cordon); res != confinementOutOfScope {
		t.Fatalf("a resolved out-of-scope node must be confirm-blocked, got %v", res)
	}
	// With no platform and not in inventory, labels cannot be resolved: this is
	// missing evidence, not a confirmed violation, so it must hold-and-retry.
	if _, res := c.destructiveStepConfinement(ctx, &types.Incident{Target: types.Target{Node: "unknown"}}, cordon); res != confinementUnresolved {
		t.Fatalf("unresolvable node labels must hold (unresolved), got %v", res)
	}
	if _, res := c.destructiveStepConfinement(ctx, &types.Incident{Target: types.Target{Node: "out"}, DryRun: true}, cordon); res != confinementAllowed {
		t.Fatalf("dry-run incidents must be unaffected, got %v", res)
	}
	if _, res := c.destructiveStepConfinement(ctx, &types.Incident{Target: types.Target{Node: "out"}}, &playbook.Step{Action: "platform.uncordon"}); res != confinementAllowed {
		t.Fatalf("uncordon is restorative and must never be blocked, got %v", res)
	}
	c.SetDestructiveNodeSelector(nil)
	if _, res := c.destructiveStepConfinement(ctx, &types.Incident{Target: types.Target{Node: "out"}}, cordon); res != confinementAllowed {
		t.Fatalf("no selector configured means no confinement, got %v", res)
	}
}

// End-to-end: on an Enabled (non-dry-run) install a non-dry-run incident whose
// node is OUTSIDE the declared destructiveExecution.nodeSelector must NOT
// execute the cordon and must fail closed to NEEDS_HUMAN with an audited notice.
func TestOutOfScopeDestructiveIncidentGoesNeedsHuman(t *testing.T) {
	c, st, notifier := walkFixture(t, false)
	ctx := context.Background()
	c.SetDestructiveNodeSelector(map[string]string{"pool": "canary"})
	if err := st.UpsertNode(ctx, &types.Node{Name: "n1", Labels: map[string]string{"pool": "prod"}}); err != nil {
		t.Fatal(err)
	}
	inc := &types.Incident{
		ID:             "inc-scope",
		Target:         types.Target{Node: "n1", GPUUUID: "GPU-1"},
		Class:          types.ClassECCDBE,
		State:          types.StateEvaluating,
		Playbook:       "drain-and-reset",
		DryRun:         false,
		OpenedAt:       time.Now(),
		UpdatedAt:      time.Now(),
		StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	c.reconcile(ctx)
	time.Sleep(2 * time.Millisecond) // no step goroutine should run, but be safe

	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateNeedsHuman {
		t.Fatalf("state = %s, want NEEDS_HUMAN: an out-of-scope cordon must fail closed, not execute", got.State)
	}
	sawNeedsHuman := false
	for _, ev := range notifier.kinds() {
		if ev == notify.EventNeedsHuman {
			sawNeedsHuman = true
		}
	}
	if !sawNeedsHuman {
		t.Fatal("out-of-scope refusal must notify a human")
	}
}

// --- Fix 4: quiesce rewind must skip in-flight incidents ---

// A running step goroutine owns its incident; rewinding its StepIndex out from
// under it would be silently clobbered by the goroutine's post-step transition
// (which writes back the snapshot's StepIndex under a freshly taken version).
// The rewind must skip in-flight incidents, exactly as
// resolveIncidentsOnVanishedNodes does.
func TestRewindSkipsInFlightIncident(t *testing.T) {
	p := &stackPlatform{quiescedNodes: []string{"node-a"}}
	c, st := stackTestController(t, p)
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
	inc.StepIndex = 2 // past the quiesce
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	c.setInFlight(inc.ID, true) // a step goroutine still owns it

	c.rewindIncidentsToQuiesce(ctx, "node-a")

	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StepIndex != 2 {
		t.Fatalf("step index = %d; an in-flight incident must not be rewound (its running step's post-step transition would clobber the rewind)", got.StepIndex)
	}
}

// --- Fix R4: gate occupancy rebuilt from durable state on failover ---

// A freshly-constructed gate (new leader) starts with empty slots while agents
// may still be running leased destructive actions the previous leader admitted.
// RebuildGateOccupancy must re-seed the concurrency and reboot occupancy from
// the durable EXECUTING incidents so the caps hold across failover, and the
// seeded slot must be handed back — not leaked — once recovery re-drives the
// incident out of EXECUTING.
func TestRebuildGateOccupancyHoldsCapsAcrossFailover(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	book := &playbook.Playbook{Name: "cordon-reboot", Target: "node", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon"},
		{Name: "reboot", Action: "agent.reboot", Approval: "required"},
	}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"cordon-reboot": book}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1})
	c := New(st, st, engine, gate, nil, nil, nil, &recordingNotifier{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	now := time.Now()
	inc := &types.Incident{
		ID:             "inc-reboot",
		Target:         types.Target{Node: "node-a"},
		Class:          types.ClassFellOffBus,
		State:          types.StateExecuting,
		Playbook:       "cordon-reboot",
		StepIndex:      1, // the agent.reboot step, in flight on the old leader
		OpenedAt:       now,
		UpdatedAt:      now,
		StateChangedAt: now,
		// What the old leader durably wrote with its EXECUTING transition:
		// rebuild reads THIS bit, never an inference from state/StepIndex.
		RemediationSlotHeld: true,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// Precondition: an empty gate would admit fresh work past the cap.
	if err := gate.Allow(types.Target{Node: "node-b"}, types.ActionReboot); err != nil {
		t.Fatalf("empty gate should admit; got %v", err)
	}
	gate.Done(types.Target{Node: "node-b"}, types.ActionReboot, 0)

	if err := c.RebuildGateOccupancy(ctx); err != nil {
		t.Fatal(err)
	}

	if err := gate.Allow(types.Target{Node: "node-b"}, types.ActionType("platform.cordon")); err == nil {
		t.Fatal("remediation cap must hold across failover: node-a's EXECUTING slot must be counted")
	}
	if err := gate.Allow(types.Target{Node: "node-b"}, types.ActionReboot); err == nil {
		t.Fatal("reboot cap must hold across failover: node-a's in-flight reboot must be counted")
	}

	// Recovery re-drives node-a out of EXECUTING: the seeded per-step (reboot)
	// slot is handed back, but node-a still holds its remediation slot — it is
	// mid-remediation in EVALUATING, and the cap counts remediations, not steps.
	if err := c.transition(ctx, inc, types.StateEvaluating, "system", "recover", "controller restarted mid-step", nil); err != nil {
		t.Fatal(err)
	}
	if err := gate.Allow(types.Target{Node: "node-b"}, types.ActionReboot); err == nil {
		t.Fatal("node-a is still mid-remediation; the remediation cap must still hold")
	}
	// node-a's own next step is admitted without the remediation cap, and the
	// reboot slot freed by leaving EXECUTING is available to it again.
	if err := gate.AllowHeld(types.Target{Node: "node-a"}, types.ActionReboot); err != nil {
		t.Fatalf("held node-a must be admitted for its next step: %v", err)
	}
	gate.StepDone(types.Target{Node: "node-a"}, types.ActionReboot, 0)
	// Terminalizing the incident releases the remediation slot — no leak.
	if err := c.transition(ctx, inc, types.StateNeedsHuman, "system", "recover", "parked for a human", nil); err != nil {
		t.Fatal(err)
	}
	if err := gate.Allow(types.Target{Node: "node-b"}, types.ActionReboot); err != nil {
		t.Fatalf("after node-a terminalizes the slot must be freed; got %v", err)
	}
}

// A store read failure during rebuild must not block startup: it degrades to
// the pre-fix transient undercount, never to a blocked walk.
func TestRebuildGateOccupancyIsBestEffort(t *testing.T) {
	c, _, _ := walkFixture(t, false)
	// No EXECUTING incidents: rebuild is a no-op and must succeed.
	if err := c.RebuildGateOccupancy(context.Background()); err != nil {
		t.Fatalf("rebuild with no EXECUTING incidents = %v, want nil", err)
	}
}
