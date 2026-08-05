package controller

// Regression tests for the fifth review round's controller findings.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/actuator/agentrpc"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// blockingActuator models an agent that never answers: like the real agentrpc
// actuator, Execute returns only when the caller's context dies.
type blockingActuator struct {
	captured []types.Action
}

func (a *blockingActuator) Name() string { return "blocking-test" }

func (a *blockingActuator) Capabilities() []types.ActionType {
	return []types.ActionType{types.ActionQuiesceAcceleratorHost, types.ActionRestoreAcceleratorHost}
}

func (a *blockingActuator) Healthy(context.Context, types.Node) error { return nil }

func (a *blockingActuator) Execute(ctx context.Context, _ types.Node, act types.Action) (*types.ActionResult, error) {
	a.captured = append(a.captured, act)
	<-ctx.Done()
	return nil, ctx.Err()
}

// Finding 2 (CRITICAL): the accelerator-stack janitor runs synchronously at
// the top of every reconcile and used the process context for its agent
// restore, so one quiesced node whose agent was down or busy blocked the
// single Run loop forever — halting all incident processing and signal
// ingestion cluster-wide. The janitor's wait must be bounded; the action stays
// queued and the next tick re-attaches to it.
func TestJanitorWaitIsBoundedWhenTheAgentNeverAnswers(t *testing.T) {
	saved := acceleratorHostRestoreWait
	acceleratorHostRestoreWait = 50 * time.Millisecond
	t.Cleanup(func() { acceleratorHostRestoreWait = saved })

	p := &stackPlatform{quiescedNodes: []string{"node-a"}}
	c, _ := stackTestControllerWithActuator(t, p, &blockingActuator{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.restoreAbandonedAcceleratorStacks(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the janitor blocked on an unresponsive agent: one dead node froze the whole reconcile loop")
	}
	// Nothing was restored — the wait expired — and nothing may pretend it was.
	if len(p.restored) != 0 {
		t.Fatalf("restored = %v, want nothing while the node's own state is not back", p.restored)
	}
}

// Finding 6, corrected by the round-6 review: the janitor originally stamped
// the queued restore's IncidentID with the NODE NAME; the round-5 fix stamped
// the owning incident — but the janitor acts precisely when that owner is
// HALTED, and the terminal-incident claim guard refuses any action stamped
// with a halted incident, so the restore became permanently unclaimable (the
// node's monitoring stayed down, and each tick burned the full bounded wait).
// The stamp must be empty — janitor work is owned by no active incident —
// with provenance carried in params, which the claim guard does not read.
func TestJanitorRestoreCarriesNoIncidentStampSoTheClaimGuardCannotBlockIt(t *testing.T) {
	saved := acceleratorHostRestoreWait
	acceleratorHostRestoreWait = 50 * time.Millisecond
	t.Cleanup(func() { acceleratorHostRestoreWait = saved })
	ctx := context.Background()

	// Unowned quiesce: no pin behind the platform's marker.
	{
		p := &stackPlatform{quiescedNodes: []string{"node-a"}}
		act := &blockingActuator{}
		c, _ := stackTestControllerWithActuator(t, p, act)
		c.restoreAbandonedAcceleratorStacks(ctx)
		if len(act.captured) == 0 {
			t.Fatal("the janitor never reached the agent")
		}
		if got := act.captured[0].IncidentID; got != "" {
			t.Fatalf("unowned restore IncidentID = %q, want empty", got)
		}
		if act.captured[0].ID == "node-a" {
			t.Fatal("the action ID must be derived, not the raw node name")
		}
	}

	// Owned quiesce whose incident halted: the stamp is STILL empty (the claim
	// guard blocks halted-incident actions); the orphaned owner is provenance
	// in params only.
	{
		p := &stackPlatform{quiescedNodes: []string{"node-a"}}
		act := &blockingActuator{}
		c, st := stackTestControllerWithActuator(t, p, act)
		inc := resetIncident()
		inc.State = types.StateNeedsHuman
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		c.pinAcceleratorEvidence(inc.ID, pinnedAcceleratorEvidence{
			node: "node-a", nodeUID: "node-uid-a",
			report:   readyNVIDIAResetReport(time.Now().UTC(), "digest"),
			pinnedAt: time.Now(),
		})
		c.restoreAbandonedAcceleratorStacks(ctx)
		if len(act.captured) == 0 {
			t.Fatal("the janitor never reached the agent")
		}
		if got := act.captured[0].IncidentID; got != "" {
			t.Fatalf("orphaned restore IncidentID = %q, want empty — a halted-incident stamp is unclaimable", got)
		}
		if got := act.captured[0].Params["orphaned_incident"]; got != inc.ID {
			t.Fatalf("provenance param = %q, want %q", got, inc.ID)
		}
	}
}

// The same defect, proven across the REAL durable seam instead of a fake
// actuator: the janitor's restore, enqueued through the real agentrpc
// actuator into the real store, must be claimable by the node's agent even
// though the quiesce's former owner sits in NEEDS_HUMAN. Before the fix,
// ClaimNextAction's terminal-incident guard matched the stamped owner and
// returned ErrNotFound forever.
func TestJanitorRestoreIsClaimableWhileItsFormerOwnerIsHalted(t *testing.T) {
	saved := acceleratorHostRestoreWait
	acceleratorHostRestoreWait = 50 * time.Millisecond
	t.Cleanup(func() { acceleratorHostRestoreWait = saved })
	ctx := context.Background()

	p := &stackPlatform{quiescedNodes: []string{"node-a"}}
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := New(st, nil, nil, nil, nil, p, agentrpc.New(st, 0), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid-a"}); err != nil {
		t.Fatal(err)
	}
	inc := resetIncident()
	inc.State = types.StateNeedsHuman
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	c.pinAcceleratorEvidence(inc.ID, pinnedAcceleratorEvidence{
		node: "node-a", nodeUID: "node-uid-a",
		report:   readyNVIDIAResetReport(time.Now().UTC(), "digest"),
		pinnedAt: time.Now(),
	})

	// The janitor enqueues the restore and times out waiting (no agent yet).
	c.restoreAbandonedAcceleratorStacks(ctx)

	claimed, err := st.ClaimNextAction(ctx, "node-a", "boot-1", time.Minute)
	if err != nil {
		t.Fatalf("the node's agent cannot claim the janitor's restore: %v — the halted former owner is blocking it", err)
	}
	if claimed.Action.Type != types.ActionRestoreAcceleratorHost {
		t.Fatalf("claimed %q, want the restore action", claimed.Action.Type)
	}
	if got := claimed.Action.Params["orphaned_incident"]; got != inc.ID {
		t.Fatalf("provenance param = %q, want %q", got, inc.ID)
	}
}

// deadlineCapturingActuator records whether Execute's context carried a
// deadline, and how far away it was.
type deadlineCapturingActuator struct {
	hadDeadline bool
	until       time.Duration
}

func (a *deadlineCapturingActuator) Name() string { return "deadline-test" }

func (a *deadlineCapturingActuator) Capabilities() []types.ActionType {
	return []types.ActionType{types.ActionRunDiag}
}

func (a *deadlineCapturingActuator) Healthy(context.Context, types.Node) error { return nil }

func (a *deadlineCapturingActuator) Execute(ctx context.Context, _ types.Node, _ types.Action) (*types.ActionResult, error) {
	if dl, ok := ctx.Deadline(); ok {
		a.hadDeadline = true
		a.until = time.Until(dl)
	}
	return &types.ActionResult{OK: true, Output: "ok"}, nil
}

// Finding 2: a step whose playbook declares no timeout inherited the process
// context. With an agent that never answers, that step's goroutine, inFlight
// mark, and gate slot lived until controller restart. Every step must carry an
// upper bound — the default when the playbook declares none.
func TestExecuteStepBoundsAStepWithNoTimeout(t *testing.T) {
	act := &deadlineCapturingActuator{}
	c, st := stackTestControllerWithActuator(t, &stackPlatform{}, act)
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a"}); err != nil {
		t.Fatal(err)
	}
	inc := &types.Incident{
		ID: "inc-unbounded", Target: types.Target{Node: "node-a"},
		State: types.StateExecuting, Playbook: "pb",
	}

	if _, err := c.executeStep(ctx, inc, &playbook.Step{Name: "diag", Action: "agent.run_diag"}); err != nil {
		t.Fatalf("executeStep = %v", err)
	}
	if !act.hadDeadline {
		t.Fatal("a step with no declared timeout must still get a bounded context")
	}
	if act.until <= 0 || act.until > defaultStepTimeout+agentResultGrace {
		t.Fatalf("deadline %s away, want within the default step budget %s", act.until, defaultStepTimeout+agentResultGrace)
	}
}

// Finding 5 (the request/decision TOCTOU): binding the approval to the step
// resolved at DECISION time reopened the hot-swap hole one step later — a
// playbook swap between the approval request (what the human was shown) and
// the click made the decision capture the swapped-in step, which then matched
// itself at resume and executed an action nobody approved. The decision must
// bind to the identity recorded when the incident parked.
func TestApprovalBindsToTheStepShownAtRequestTime(t *testing.T) {
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
		ID:             "inc-toctou",
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

	// Park: the human is asked to approve "reboot", and that identity is
	// durably recorded as the request.
	c.reconcile(ctx)
	if parked, _ := st.GetIncident(ctx, inc.ID); parked.State != types.StateAwaitingApproval {
		t.Fatalf("state = %s, want AWAITING_APPROVAL", parked.State)
	}
	requested, err := st.LatestApproval(ctx, inc.ID)
	if err != nil {
		t.Fatalf("no request row recorded at park: %v", err)
	}
	if requested.Decision != types.ApprovalRequested || requested.StepAction != "agent.reboot" {
		t.Fatalf("request row = %+v, want a requested record bound to agent.reboot", requested)
	}

	// Hot-swap between the notification and the click: the same index now
	// holds replace_node.
	pbReplace := &playbook.Playbook{
		Name: "pb", Target: "node",
		Steps: []playbook.Step{{Name: "reboot", Action: "platform.replace_node", Approval: "required"}},
	}
	engineV2, err := playbook.NewEngine(map[string]*playbook.Playbook{"pb": pbReplace}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engineV2)

	// The human clicks approve on the reboot they were shown.
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, 0); err != nil {
		t.Fatal(err)
	}
	decision, err := st.LatestApproval(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Decision != types.ApprovalApproved {
		t.Fatalf("latest row = %+v, want the approval", decision)
	}
	if decision.StepAction != "agent.reboot" {
		t.Fatalf("decision bound to %q, want the requested agent.reboot — binding at click time approves the swap", decision.StepAction)
	}

	// Resume: the decision mismatches the swapped-in step, so the incident
	// re-parks for a fresh approval instead of executing replace_node.
	approvalsBefore := len(notifier.approvals)
	c.reconcile(ctx)
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateAwaitingApproval {
		t.Fatalf("state = %s, want a re-park, not execution of the swapped-in step", got.State)
	}
	if len(notifier.approvals) <= approvalsBefore {
		t.Fatal("the re-park must re-request approval for the step that would actually run")
	}
	// The re-park recorded a fresh request bound to the step now current, so
	// the next decision approves what the human will actually be shown.
	rerequested, err := st.LatestApproval(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rerequested.Decision != types.ApprovalRequested || rerequested.StepAction != "platform.replace_node" {
		t.Fatalf("re-request row = %+v, want a requested record for platform.replace_node", rerequested)
	}
}

// R3 interim (round 7): the janitor's wait budget is per TICK, not per node.
// K quiesced nodes with dead agents must cost the reconcile loop one bounded
// wait, not K of them.
func TestJanitorTickSharesOneWaitBudgetAcrossNodes(t *testing.T) {
	saved := acceleratorHostRestoreWait
	acceleratorHostRestoreWait = 100 * time.Millisecond
	t.Cleanup(func() { acceleratorHostRestoreWait = saved })

	p := &stackPlatform{quiescedNodes: []string{"node-a", "node-b", "node-c"}}
	c, _ := stackTestControllerWithActuator(t, p, &blockingActuator{})

	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.restoreAbandonedAcceleratorStacks(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the janitor never returned")
	}
	if elapsed := time.Since(start); elapsed > 4*acceleratorHostRestoreWait {
		t.Fatalf("three dead nodes cost %s, want roughly ONE shared budget (%s)", elapsed, acceleratorHostRestoreWait)
	}
}
