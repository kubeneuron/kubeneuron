package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/actuator/agentrpc"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// --- Fix 1: enqueued actions carry the incident ID so cancellation works ---

// A step enqueued through the real reconcile->agentrpc path must stamp the
// incident ID onto the durable queue entry, and CancelPendingActionsForIncident
// must then match it. With the old empty-incident_id contract this cancel
// matched zero rows, so escalate/quarantine could never tombstone a superseded
// destructive action and a slow agent could run it hours later.
func TestEnqueuedAgentStepCarriesIncidentIDAndIsCancellable(t *testing.T) {
	c, st, _ := walkFixture(t, false)
	ctx := context.Background()
	c.actuator = agentrpc.New(st, 0) // the real store-backed actuator

	inc := &types.Incident{
		ID:             "inc-cancel",
		Target:         types.Target{Node: "n1"},
		Class:          types.ClassECCDBE,
		State:          types.StateExecuting,
		Playbook:       "reboot",
		OpenedAt:       time.Now(),
		UpdatedAt:      time.Now(),
		StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	step := &playbook.Step{Name: "collect", Action: "agent.collect_bundle"}

	// executeAgentStep blocks polling for a result; run it and capture its error.
	done := make(chan error, 1)
	go func() { _, err := c.executeAgentStep(ctx, inc, "collect_bundle", step); done <- err }()

	id := actionID(inc)
	var q *types.QueuedAction
	for i := 0; i < 500; i++ {
		got, err := st.GetAction(ctx, id)
		if err == nil && got != nil {
			q = got
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if q == nil {
		t.Fatal("action was never enqueued")
	}
	if q.IncidentID != inc.ID {
		t.Fatalf("queued action IncidentID = %q, want %q — an empty incident_id makes cancellation inert", q.IncidentID, inc.ID)
	}

	cancelled, err := st.CancelPendingActionsForIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled != 1 {
		t.Fatalf("CancelPendingActionsForIncident matched %d rows, want 1 (the tombstone must reach the enqueued action)", cancelled)
	}

	// The blocked step must observe the tombstone and fail rather than execute.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled action must fail the step, not succeed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("executeAgentStep did not return after cancellation")
	}
}

// --- Fix 3: OBSERVING threshold with no bound playbook must not livelock ---

// ClassECCSBERate has a built-in observe threshold but walkFixture binds no
// playbook for it. A threshold-crossed incident with no bound playbook must hold
// in OBSERVING (advanceEvaluating would bounce it straight back, and every
// transition rewrites the quiet-window anchor so it could never resolve — an
// audit-row flood). It must instead quiet-resolve.
func TestObservingThresholdWithNoPlaybookDoesNotLivelock(t *testing.T) {
	c, st, _ := walkFixture(t, false)
	ctx := context.Background()

	inc := &types.Incident{
		ID:             "inc-livelock",
		Target:         types.Target{Node: "n1", GPUUUID: "GPU-1"},
		Class:          types.ClassECCSBERate,
		State:          types.StateObserving,
		Playbook:       "", // no bound playbook
		SignalSeen:     10, // well past the built-in threshold
		OpenedAt:       time.Now(),
		UpdatedAt:      time.Now(),
		StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	if err := c.advanceObserving(ctx, inc); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateObserving {
		t.Fatalf("state = %s, want OBSERVING: a threshold-crossed incident with no bound playbook must not flip to EVALUATING", got.State)
	}

	// After the observation window with no recurrence it quiet-resolves.
	got.UpdatedAt = time.Now().Add(-48 * time.Hour)
	if err := st.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := c.advanceObserving(ctx, got); err != nil {
		t.Fatal(err)
	}
	final, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != types.StateResolved {
		t.Fatalf("state = %s, want RESOLVED after the quiet window", final.State)
	}
}

// --- Fix 4: DecideApproval must refuse an unresolvable step ---

// During a hot-reload gap the incident's playbook may be momentarily absent from
// the engine, so the current step cannot be resolved. Recording a decision then
// would mint an all-empty (unbound) approval that approvalStepMismatch later
// waves through as "legacy" for whatever step becomes current — the hot-swap
// hole the step-binding closed. DecideApproval must refuse and record nothing.
func TestDecideApprovalRefusesUnresolvableStep(t *testing.T) {
	c, st, _ := walkFixture(t, false)
	ctx := context.Background()

	inc := &types.Incident{
		ID:             "inc-hotswap",
		Target:         types.Target{Node: "n1"},
		Class:          types.ClassFellOffBus,
		State:          types.StateAwaitingApproval,
		Playbook:       "absent-during-reload", // not in the engine
		OpenedAt:       time.Now(),
		UpdatedAt:      time.Now(),
		StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, 0, ""); err == nil {
		t.Fatal("DecideApproval must refuse to record a decision when the current step is unresolvable")
	}
	if _, err := st.LatestApproval(ctx, inc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no approval must be recorded; LatestApproval err = %v, want ErrNotFound", err)
	}
}

// --- Fix 5: transient label failure holds; confirmed out-of-scope quarantines ---

type erroringPlatform struct {
	platform.Platform
	err error
}

func (p *erroringPlatform) Name() string { return "erroring" }

func (p *erroringPlatform) ListNodes(context.Context) ([]types.Node, error) {
	return nil, p.err
}

// A transient node-label resolution failure (an apiserver/ListNodes blip) at the
// moment a destructive step starts must HOLD in EVALUATING and retry — matching
// the accelerator evidence gate — not quarantine to the human-only NEEDS_HUMAN.
// Only when resolution stays impossible past the deadline does it fail closed.
func TestTransientLabelFailureHoldsThenQuarantines(t *testing.T) {
	c, st, _ := walkFixture(t, false)
	ctx := context.Background()
	c.platform = &erroringPlatform{err: errors.New("apiserver blip")}
	c.SetDestructiveNodeSelector(map[string]string{"pool": "canary"})

	inc := &types.Incident{
		ID:             "inc-transient",
		Target:         types.Target{Node: "ghost", GPUUUID: "GPU-1"}, // not in inventory
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
	step := &playbook.Step{Name: "cordon", Action: "platform.cordon"}

	// Within the deadline: hold in EVALUATING.
	if err := c.startStep(ctx, inc, step, "system"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateEvaluating {
		t.Fatalf("state = %s, want EVALUATING: a transient label failure must hold-and-retry, not quarantine", got.State)
	}

	// Past the confinement deadline with labels still unresolvable: fail closed.
	got.StateChangedAt = time.Now().Add(-24 * time.Hour)
	if err := st.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := c.startStep(ctx, got, step, "system"); err != nil {
		t.Fatal(err)
	}
	final, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != types.StateNeedsHuman {
		t.Fatalf("state = %s, want NEEDS_HUMAN after the deadline with labels still unresolvable", final.State)
	}
}
