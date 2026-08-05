package controller

import (
	"context"
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

// miniController wires a controller around a caller-supplied engine, an
// in-memory store, and a recording notifier — for tests that need a bespoke
// playbook graph rather than the shipped set.
func miniController(t *testing.T, engine *playbook.Engine) (*Controller, *sqlite.Store, *recordingNotifier) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1})
	notifier := &recordingNotifier{}
	c := New(st, st, engine, gate, nil, nil, nil, notifier,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c, st, notifier
}

// --- Fix 4: an obstructed reset must escalate, never loop in EVALUATING ---

// holderInjectingStore returns the persisted report with DeviceHolders attached.
// Holder persistence in the store is owned by a separate change landing in the
// same batch; this decorator supplies the holders so this worktree can exercise
// the obstruction path end to end without touching the report scan.
type holderInjectingStore struct {
	*sqlite.Store
	holders []types.AgentDeviceHolder
}

func (h *holderInjectingStore) GetAcceleratorReport(ctx context.Context, node string, vendor types.AcceleratorVendor) (*types.AgentAcceleratorReport, error) {
	report, err := h.Store.GetAcceleratorReport(ctx, node, vendor)
	if err != nil {
		return nil, err
	}
	report.DeviceHolders = h.holders
	return report, nil
}

// A reset playbook whose target device IS attributed but is held by a process
// KubeNeuron cannot release (here datadog-agent) is infeasible for a reported
// reason, not missing evidence. Before this fix, refusing it from EVALUATING
// tried an EVALUATING -> EVALUATING transition, which was illegal, so the
// refusal errored and re-fired every reconcile forever. It must instead advance
// the ladder in place to the next rung (reboot), which needs no cleared device.
func TestObstructedResetEscalatesInsteadOfLoopingInEvaluating(t *testing.T) {
	base, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	st := &holderInjectingStore{
		Store:   base,
		holders: []types.AgentDeviceHolder{{PID: 555, Command: "datadog-agent", Device: "/dev/nvidia0"}},
	}

	books, err := playbook.LoadDir("../../configs/playbooks")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := playbook.NewEngine(books, []playbook.Policy{{Class: types.ClassECCDBE, Playbook: "drain-and-reset"}})
	if err != nil {
		t.Fatal(err)
	}
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}) // non-dry-run
	notifier := &recordingNotifier{}
	c := New(st, st, engine, gate, nil, nil, nil, notifier,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if err := base.UpsertNode(ctx, &types.Node{Name: "n1", UID: "uid-n1"}); err != nil {
		t.Fatal(err)
	}
	report := readyNVIDIAResetReport(time.Now(), "digest")
	report.Node = "n1"
	report.NodeUID = "uid-n1"
	report.Devices = []types.AgentAcceleratorDevice{{
		ID: "GPU-1", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU,
	}}
	if err := base.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}

	inc := &types.Incident{
		ID:             "inc-obstructed",
		Target:         types.Target{Node: "n1", GPUUUID: "GPU-1"}, // attributed
		Class:          types.ClassECCDBE,
		State:          types.StateEvaluating,
		Playbook:       "drain-and-reset", // resets a GPU; escalates to reboot
		OpenedAt:       time.Now(),
		UpdatedAt:      time.Now(),
		StateChangedAt: time.Now(),
	}
	if err := base.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	c.reconcile(ctx)

	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == types.StateEvaluating && got.Playbook == "drain-and-reset" {
		t.Fatalf("incident stuck in EVALUATING on the reset playbook (step %d, attempt %d): the obstruction refusal looped instead of escalating",
			got.StepIndex, got.Attempt)
	}
	if got.Playbook != "reboot" || got.State != types.StateEvaluating || got.Attempt != 1 || got.StepIndex != 0 {
		t.Fatalf("escalation = playbook %q state %q attempt %d step %d, want reboot/EVALUATING/1/0",
			got.Playbook, got.State, got.Attempt, got.StepIndex)
	}
	if len(notifier.events) == 0 {
		t.Fatal("an escalation must notify, not silently re-deny each tick")
	}
}

// --- Fix 6: a persistent fault reaches NEEDS_HUMAN at the escalation cap ---

// A self-escalating ladder (A -> A) is rejected at compile and load, but the
// runtime cap is the backstop for any engine assembled without that validation.
// A persistent fault must climb the ladder only maxEscalationAttempts times and
// then fail closed to NEEDS_HUMAN, rather than driving destructive rungs forever.
func TestEscalationCapFailsClosedToNeedsHuman(t *testing.T) {
	pbA := &playbook.Playbook{
		Name: "A", Target: "gpu",
		Steps:     []playbook.Step{{Name: "reset", Action: "agent.gpu_reset"}},
		OnFailure: playbook.OnFailure{EscalateTo: "A"},
	}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"A": pbA}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, st, notifier := miniController(t, engine)
	ctx := context.Background()

	inc := &types.Incident{
		ID:             "inc-cap",
		Target:         types.Target{Node: "n1", GPUUUID: "GPU-1"},
		Class:          types.ClassECCDBE,
		State:          types.StateEvaluating,
		Playbook:       "A",
		OpenedAt:       time.Now(),
		UpdatedAt:      time.Now(),
		StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < maxEscalationAttempts+3; i++ {
		cur, err := st.GetIncident(ctx, inc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.State == types.StateNeedsHuman {
			break
		}
		if err := c.escalate(ctx, cur, "persistent fault"); err != nil {
			t.Fatalf("escalate iteration %d: %v", i, err)
		}
	}

	final, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != types.StateNeedsHuman {
		t.Fatalf("state = %s, want NEEDS_HUMAN after the escalation cap, not an unbounded loop", final.State)
	}
	if final.Attempt != maxEscalationAttempts {
		t.Fatalf("attempt = %d, want the cap of %d", final.Attempt, maxEscalationAttempts)
	}
	sawNeedsHuman := false
	for _, ev := range notifier.events {
		if ev.Kind == notify.EventNeedsHuman {
			sawNeedsHuman = true
		}
	}
	if !sawNeedsHuman {
		t.Fatal("reaching the cap must notify needs-human")
	}
}

// --- Fix 7 (re-encoded in round 8): an approval round asks about one step ---

// The request-identity matcher: the exact requested step matches, a step
// hot-swapped to a different action at the same index does not, and a
// params-only edit is caught by the content hash. There is deliberately NO
// legacy waiver here: every approval round has a request record with full
// identity, and a round without one is re-parked, never waved through.
func TestRequestMismatchDetectsHotSwap(t *testing.T) {
	reboot := &playbook.Step{Name: "reboot", Action: "agent.reboot", Approval: "required"}
	request := &types.Approval{
		Decision:     types.ApprovalRequested,
		PlaybookName: "pb", StepName: "reboot", StepAction: "agent.reboot",
		StepHash: stepContentHash("pb", reboot),
	}
	if m := requestMismatch(request, "pb", reboot); m != "" {
		t.Fatalf("identical step must match, got mismatch %q", m)
	}
	replace := &playbook.Step{Name: "reboot", Action: "platform.replace_node", Approval: "required"}
	if m := requestMismatch(request, "pb", replace); m == "" {
		t.Fatal("a step swapped to replace_node at the same index must be a mismatch")
	}
	// A params-only edit at the same name/action is still a mismatch (hash covers it).
	tweaked := &playbook.Step{Name: "reboot", Action: "agent.reboot", Approval: "required", Params: map[string]string{"force": "true"}}
	if m := requestMismatch(request, "pb", tweaked); m == "" {
		t.Fatal("a params edit at the same index must be a mismatch")
	}
}

// End to end: an approval granted for "reboot" must not execute a hot-swapped
// "replace_node" that lands at the same index before the reconcile resumes; the
// incident re-parks for a fresh approval and the stale decision never resumes.
func TestApprovalDoesNotExecuteHotSwappedStep(t *testing.T) {
	pbReboot := &playbook.Playbook{
		Name: "pb", Target: "node",
		Steps: []playbook.Step{{Name: "reboot", Action: "agent.reboot", Approval: "required"}},
	}
	engineV1, err := playbook.NewEngine(map[string]*playbook.Playbook{"pb": pbReboot}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, st, notifier := miniController(t, engineV1)
	ctx := context.Background()

	if err := st.UpsertNode(ctx, &types.Node{Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	inc := &types.Incident{
		ID:             "inc-approve",
		Target:         types.Target{Node: "n1"},
		Class:          types.ClassFellOffBus,
		State:          types.StateEvaluating,
		Playbook:       "pb",
		OpenedAt:       time.Now(),
		UpdatedAt:      time.Now(),
		StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// Park for approval.
	c.reconcile(ctx)
	if parked, _ := st.GetIncident(ctx, inc.ID); parked.State != types.StateAwaitingApproval {
		t.Fatalf("state = %s, want AWAITING_APPROVAL", parked.State)
	}

	// The human approves the reboot they were shown; the decision binds to it.
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, 0); err != nil {
		t.Fatal(err)
	}
	approvalsBefore := len(notifier.approvals)

	// A playbook hot-swap replaces reboot with replace_node at the same index.
	pbReplace := &playbook.Playbook{
		Name: "pb", Target: "node",
		Steps: []playbook.Step{{Name: "reboot", Action: "platform.replace_node", Approval: "required"}},
	}
	engineV2, err := playbook.NewEngine(map[string]*playbook.Playbook{"pb": pbReplace}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engineV2)

	// Resume: must re-park, never execute the unapproved replace_node.
	c.reconcile(ctx)
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateAwaitingApproval {
		t.Fatalf("state = %s, want a re-park in AWAITING_APPROVAL, not execution of an unapproved action", got.State)
	}
	if got.StepIndex != 0 {
		t.Fatalf("step index = %d, want it unchanged at 0", got.StepIndex)
	}
	if len(notifier.approvals) <= approvalsBefore {
		t.Fatal("a re-park must re-request approval for the step that would actually run")
	}

	// The now-stale decision must not resume on a later pass either.
	c.reconcile(ctx)
	if still, _ := st.GetIncident(ctx, inc.ID); still.State != types.StateAwaitingApproval {
		t.Fatalf("stale approval must not resume; state = %s", still.State)
	}
}

// --- Fix 14: a recycle wait with no step timeout is bounded, not infinite ---

// A recycle_node step that declares no timeout previously handed waitNodeReady an
// unbounded context, so a node that never rejoins (e.g. a managed node group that
// replaced it) held the incident in EXECUTING until the controller restarted. The
// wait must fall back to a default bound and fail the step instead.
func TestRecycleWaitIsBoundedWithoutStepTimeout(t *testing.T) {
	origTimeout, origPoll := defaultRecycleWaitTimeout, recycleReadyPollInterval
	defaultRecycleWaitTimeout = 30 * time.Millisecond
	recycleReadyPollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		defaultRecycleWaitTimeout = origTimeout
		recycleReadyPollInterval = origPoll
	})

	p := &recyclePlatform{configured: true, nodeReady: false} // never becomes Ready
	c := recycleController(t, p)
	inc := &types.Incident{ID: "i", Target: types.Target{Node: "node-a"}}

	done := make(chan error, 1)
	go func() {
		// context.Background(): no deadline at all — the bound must come from the
		// default the step applies, not from the caller.
		_, err := c.recycleNodeStep(context.Background(), inc, false)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an unbounded recycle wait must fail the step, not report success on a node that never rejoined")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recycleNodeStep did not return: the recycle wait is still unbounded")
	}
}
