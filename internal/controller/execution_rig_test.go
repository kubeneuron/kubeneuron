package controller

// Round-7 item 7.3: the assembled execution/recovery rig. The ingestion half
// of the pipeline got its system-level tests in round 6; this is the other
// half, through the REAL wiring end to end: the controller's own backend
// methods (NextAction/CompleteAction with boot identity and lease tokens),
// the real durable queue in a real sqlite store, and the real agentrpc
// actuator — no fakes between the walk and the store. An execution-side
// analogue of the round-5 outbox bug (something dropped between the
// EXECUTING transition and the enqueue, or between claim and completion
// accounting) fails loudly here.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/actuator/agentrpc"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// rigAgent drives the controller's REAL httpapi.Backend methods the way a
// node agent does: poll NextAction with a boot header, execute, post the
// result with the lease token and the same boot identity.
type rigAgent struct {
	c      *Controller
	node   string
	bootID string

	mu       sync.Mutex
	executed []types.Action
}

func (a *rigAgent) pollOnce(t *testing.T) bool {
	t.Helper()
	claimed, err := a.poll()
	if err != nil {
		t.Fatalf("rig agent poll: %v", err)
	}
	return claimed
}

// poll performs one claim/complete cycle; a nil error with claimed=false
// means the queue was empty.
func (a *rigAgent) poll() (bool, error) {
	req := httptest.NewRequest("GET", "/api/v1/agents/actions", nil)
	req.Header.Set(types.AgentBootIDHeader, a.bootID)
	queued, err := a.c.NextAction(req, a.node)
	if err != nil {
		return false, err
	}
	if queued == nil {
		return false, nil
	}
	a.mu.Lock()
	a.executed = append(a.executed, queued.Action)
	a.mu.Unlock()
	post := httptest.NewRequest("POST", "/api/v1/agents/actions/result", nil)
	post.Header.Set(types.AgentBootIDHeader, a.bootID)
	return true, a.c.CompleteAction(post, a.node, queued.Action.ID, queued.LeaseToken, types.ActionResult{
		ActionID: queued.Action.ID, OK: true, Output: "rig: " + string(queued.Action.Type),
	})
}

// serve polls until the test cleans up, like the real agent's action loop.
// Shutdown is SYNCHRONOUS (cleanup waits for the goroutine), so a straggling
// poll can never hit the store after the test closed it; poll errors during
// the run surface as test errors, never as post-test Fatals from a goroutine.
func (a *rigAgent) serve(t *testing.T) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := a.poll(); err != nil {
				select {
				case <-stop: // shutting down; the store may already be closed
				default:
					t.Errorf("rig agent poll: %v", err)
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

func executionRig(t *testing.T, steps []playbook.Step) (*Controller, *sqlite.Store, *recordingNotifier) {
	t.Helper()
	book := &playbook.Playbook{Name: "rig", Target: "node", Steps: steps}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"rig": book}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	notifier := &recordingNotifier{}
	c := New(st, st, engine, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
		nil, nil, agentrpc.New(st, 0), notifier, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := st.UpsertNode(context.Background(), &types.Node{Name: "n1", AgentLastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return c, st, notifier
}

func rigIncident() *types.Incident {
	now := time.Now()
	return &types.Incident{
		ID: "inc-rig", Target: types.Target{Node: "n1"},
		Class: types.ClassFellOffBus, State: types.StateEvaluating,
		Playbook: "rig", DryRun: false,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
}

func waitForIncident(t *testing.T, st *sqlite.Store, id string, ok func(*types.Incident) bool) *types.Incident {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		inc, err := st.GetIncident(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if ok(inc) {
			return inc
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for incident condition; state=%s step=%d", inc.State, inc.StepIndex)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Dispatch → real durable claim → real completion → post-step advance: the
// whole chain through one process, no fakes.
func TestRigDispatchClaimCompleteAdvance(t *testing.T) {
	c, st, _ := executionRig(t, []playbook.Step{
		{Name: "diag", Action: "agent.run_diag"},
	})
	ctx := context.Background()
	inc := rigIncident()
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	agent := &rigAgent{c: c, node: "n1", bootID: "boot-1"}
	agent.serve(t)

	c.reconcile(ctx)

	got := waitForIncident(t, st, inc.ID, func(i *types.Incident) bool {
		return i.State == types.StateVerifying
	})
	if got.StepIndex != 1 {
		t.Fatalf("step index = %d, want 1", got.StepIndex)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.executed) != 1 || agent.executed[0].Type != types.ActionRunDiag {
		t.Fatalf("agent executed %+v, want exactly one run_diag", agent.executed)
	}
	// The remediation slot's durable bit survives into VERIFYING.
	if !got.RemediationSlotHeld {
		t.Fatal("the durable slot bit must be held through VERIFYING")
	}
}

// Controller dies after the EXECUTING transition, before any result: the next
// controller re-drives through recovery and the replay re-attaches to the
// SAME queued action (deterministic ID) — the agent executes exactly once.
func TestRigControllerDeathMidStepReattachesNotDuplicates(t *testing.T) {
	c, st, _ := executionRig(t, []playbook.Step{
		{Name: "diag", Action: "agent.run_diag"},
	})
	ctx := context.Background()
	inc := rigIncident()
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// First controller: start the step with NO agent serving, then "die"
	// (cancel its step context so the goroutine exits without a result).
	stepCtx, kill := context.WithCancel(ctx)
	c.reconcile(stepCtx)
	waitForIncident(t, st, inc.ID, func(i *types.Incident) bool {
		return i.State == types.StateExecuting
	})
	kill()
	// Wait until the dead controller's step goroutine has exited.
	deadline := time.Now().Add(5 * time.Second)
	for c.isInFlight(inc.ID) {
		if time.Now().After(deadline) {
			t.Fatal("killed step goroutine never exited")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Second controller over the same store: rebuild occupancy from the
	// durable bit, recover the orphaned EXECUTING incident, re-drive.
	book := &playbook.Playbook{Name: "rig", Target: "node", Steps: []playbook.Step{{Name: "diag", Action: "agent.run_diag"}}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"rig": book}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c2 := New(st, st, engine, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
		nil, nil, agentrpc.New(st, 0), &recordingNotifier{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c2.RebuildGateOccupancy(ctx); err != nil {
		t.Fatal(err)
	}
	agent := &rigAgent{c: c2, node: "n1", bootID: "boot-1"}
	agent.serve(t)

	// Recovery pass moves EXECUTING -> EVALUATING; the next pass re-dispatches.
	for range 3 {
		c2.reconcile(ctx)
		time.Sleep(20 * time.Millisecond)
	}
	waitForIncident(t, st, inc.ID, func(i *types.Incident) bool {
		return i.State == types.StateVerifying
	})

	agent.mu.Lock()
	executions := len(agent.executed)
	agent.mu.Unlock()
	if executions != 1 {
		t.Fatalf("agent executed %d times across the failover, want exactly once (idempotent re-attach)", executions)
	}
}

// Escalation cancels the pending action before the agent claims it: the
// in-flight Execute observes the cancellation and the agent's next poll finds
// an empty queue — the escalated-away step never runs.
func TestRigEscalationCancelsPendingActionBeforeClaim(t *testing.T) {
	c, st, _ := executionRig(t, []playbook.Step{
		{Name: "diag", Action: "agent.run_diag"},
	})
	ctx := context.Background()
	inc := rigIncident()
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// Dispatch with no agent serving: the action sits pending.
	c.reconcile(ctx)
	waitForIncident(t, st, inc.ID, func(i *types.Incident) bool {
		return i.State == types.StateExecuting
	})

	// The incident escalates (no ladder -> quarantine) while the step's
	// Execute is still polling; its pending action must be tombstoned. The
	// enqueue happens inside the step goroutine after the EXECUTING commit,
	// so wait for the action row before cancelling.
	fresh, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	enqueueDeadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := st.GetAction(ctx, actionID(fresh)); err == nil {
			break
		}
		if time.Now().After(enqueueDeadline) {
			t.Fatal("the step goroutine never enqueued its action")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.cancelPendingActions(ctx, fresh)

	agent := &rigAgent{c: c, node: "n1", bootID: "boot-1"}
	if agent.pollOnce(t) {
		t.Fatal("a cancelled pending action must never be handed to the agent")
	}
	// The blocked Execute observes the cancellation and fails the step.
	waitForIncident(t, st, inc.ID, func(i *types.Incident) bool {
		return i.State.Halted() || i.State == types.StateEvaluating
	})
}

// Approval park → decision → resume → execution, over the same real wiring.
func TestRigApprovalParkDecideResumeExecute(t *testing.T) {
	c, st, notifier := executionRig(t, []playbook.Step{
		{Name: "reboot", Action: "agent.reboot", Approval: "required"},
	})
	ctx := context.Background()
	inc := rigIncident()
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	agent := &rigAgent{c: c, node: "n1", bootID: "boot-1"}
	agent.serve(t)

	c.reconcile(ctx)
	waitForIncident(t, st, inc.ID, func(i *types.Incident) bool {
		return i.State == types.StateAwaitingApproval
	})
	if len(notifier.approvals) != 1 {
		t.Fatalf("approval requests = %d, want 1", len(notifier.approvals))
	}
	// The park recorded the request-time step identity.
	requested, err := st.LatestApproval(ctx, inc.ID)
	if err != nil || requested.Decision != types.ApprovalRequested {
		t.Fatalf("park request row = %+v, %v", requested, err)
	}

	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, 0, ""); err != nil {
		t.Fatal(err)
	}
	c.reconcile(ctx)
	waitForIncident(t, st, inc.ID, func(i *types.Incident) bool {
		return i.State == types.StateVerifying
	})
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.executed) != 1 || agent.executed[0].Type != types.ActionReboot {
		t.Fatalf("agent executed %+v, want exactly the approved reboot", agent.executed)
	}
}

// Lease and boot-identity discipline through the real backend: an expired
// lease is reclaimable by a new boot, the old boot's completion is refused as
// lease-lost, and the new boot's completion lands.
func TestRigLeaseExpiryRebindsToTheNewBoot(t *testing.T) {
	c, st, _ := executionRig(t, []playbook.Step{
		{Name: "diag", Action: "agent.run_diag"},
	})
	ctx := context.Background()
	inc := rigIncident()
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	c.reconcile(ctx)
	waitForIncident(t, st, inc.ID, func(i *types.Incident) bool {
		return i.State == types.StateExecuting
	})

	// Boot-1 claims through the real backend, then the node reboots (lease
	// expires while the result was never posted). The EXECUTING transition
	// commits BEFORE the step goroutine enqueues, so poll for the claim.
	req := httptest.NewRequest("GET", "/api/v1/agents/actions", nil)
	req.Header.Set(types.AgentBootIDHeader, "boot-1")
	var first *types.QueuedAction
	deadline := time.Now().Add(10 * time.Second)
	for first == nil {
		var err error
		if first, err = c.NextAction(req, "n1"); err != nil {
			t.Fatal(err)
		}
		if first == nil && time.Now().After(deadline) {
			t.Fatal("boot-1 never saw the dispatched action")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := st.SQL.ExecContext(ctx,
		`UPDATE actions SET lease_expires_at_ns=? WHERE id=?`,
		time.Now().Add(-time.Second).UnixNano(), first.Action.ID); err != nil {
		t.Fatal(err)
	}

	req2 := httptest.NewRequest("GET", "/api/v1/agents/actions", nil)
	req2.Header.Set(types.AgentBootIDHeader, "boot-2")
	second, err := c.NextAction(req2, "n1")
	if err != nil || second == nil || second.Action.ID != first.Action.ID {
		t.Fatalf("boot-2 reclaim = %+v, %v; want the same action", second, err)
	}

	// The pre-reboot boot's completion must be refused.
	stale := httptest.NewRequest("POST", "/api/v1/agents/actions/result", nil)
	stale.Header.Set(types.AgentBootIDHeader, "boot-1")
	if err := c.CompleteAction(stale, "n1", first.Action.ID, first.LeaseToken,
		types.ActionResult{ActionID: first.Action.ID, OK: true}); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("stale boot completion = %v, want ErrLeaseLost", err)
	}
	// The current boot's completion lands and the step advances.
	current := httptest.NewRequest("POST", "/api/v1/agents/actions/result", nil)
	current.Header.Set(types.AgentBootIDHeader, "boot-2")
	if err := c.CompleteAction(current, "n1", second.Action.ID, second.LeaseToken,
		types.ActionResult{ActionID: second.Action.ID, OK: true, Output: "done"}); err != nil {
		t.Fatal(err)
	}
	waitForIncident(t, st, inc.ID, func(i *types.Incident) bool {
		return i.State == types.StateVerifying
	})
}
