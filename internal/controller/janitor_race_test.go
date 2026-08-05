package controller

// Round-9: the janitors run concurrently with the walk (round 8), and the
// StateChangedAt write-fence is what keeps a field-only janitor rewrite (the
// quiesce rewind) from being silently clobbered by a walk advance holding a
// pre-rewind snapshot. These tests execute that model — once as an exact
// interleaving, once truly concurrently under -race.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The exact clobber interleaving, single-threaded: the walk snapshots an
// incident past its quiesce step; the janitor rewinds it (bumping the
// StateChangedAt fence); the walk's transition from the stale snapshot must
// CONFLICT — never write its stale StepIndex back over the rewind.
func TestWalkTransitionConflictsWithAConcurrentRewind(t *testing.T) {
	book := &playbook.Playbook{Name: "reset", Target: "gpu", Steps: []playbook.Step{
		{Name: "quiesce", Action: "platform.quiesce_accelerator_stack"},
		{Name: "reset", Action: "agent.gpu_reset"},
	}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"reset": book}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &stackPlatform{quiescedNodes: []string{"node-a"}}
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	c := New(st, st, engine, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
		nil, p, &blockingActuator{}, &recordingNotifier{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	now := time.Now()
	inc := &types.Incident{
		ID: "inc-rewind", Target: types.Target{Node: "node-a", GPUUUID: "GPU-a"},
		Class: types.ClassECCDBE, State: types.StateEvaluating,
		Playbook: "reset", StepIndex: 1, // past the quiesce step
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// The walk's snapshot, listed BEFORE the janitor acts.
	snapshot, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The janitor rewinds (bumps the fence, StepIndex back to the quiesce).
	if ok := c.rewindIncidentsToQuiesce(ctx, "node-a"); !ok {
		t.Fatal("rewind must commit")
	}
	rewound, err := st.GetIncident(ctx, inc.ID)
	if err != nil || rewound.StepIndex != 0 {
		t.Fatalf("rewound = %+v, %v; want StepIndex 0", rewound, err)
	}

	// The walk, still holding the pre-rewind snapshot, tries to transition
	// (as startStep would toward EXECUTING the stale reset step). It must
	// conflict on the fence — and the rewind must survive untouched.
	err = c.transition(ctx, snapshot, types.StateExecuting, "system", "reset", "executing stale step", nil)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale-snapshot transition = %v, want ErrConflict (the fence)", err)
	}
	final, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.StepIndex != 0 || final.State != types.StateEvaluating {
		t.Fatalf("final = state %s step %d, want the rewind preserved (EVALUATING at 0)", final.State, final.StepIndex)
	}
}

// A failed or incomplete rewind must HOLD the stack restore: restoring
// deletes the durable quiesce marker that makes the rewind retryable.
func TestJanitorHoldsRestoreUntilRewindsCommit(t *testing.T) {
	book := &playbook.Playbook{Name: "reset", Target: "gpu", Steps: []playbook.Step{
		{Name: "quiesce", Action: "platform.quiesce_accelerator_stack"},
		{Name: "reset", Action: "agent.gpu_reset"},
	}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"reset": book}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &stackPlatform{quiescedNodes: []string{"node-a"}}
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	c := New(st, st, engine, nil, nil, p, &blockingActuator{}, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	now := time.Now()
	inc := &types.Incident{
		ID: "inc-held", Target: types.Target{Node: "node-a", GPUUUID: "GPU-a"},
		Class: types.ClassECCDBE, State: types.StateEvaluating,
		Playbook: "reset", StepIndex: 1,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	// Simulate an owning step goroutine: inFlight blocks the rewind.
	c.setInFlight(inc.ID, true)
	t.Cleanup(func() { c.setInFlight(inc.ID, false) })

	saved := acceleratorHostRestoreWait
	acceleratorHostRestoreWait = 50 * time.Millisecond
	t.Cleanup(func() { acceleratorHostRestoreWait = saved })

	c.restoreAbandonedAcceleratorStacks(ctx)
	if len(p.restored) != 0 {
		t.Fatal("the stack must NOT be restored while a rewind is pending — restoring deletes the retryable marker")
	}
}

// The concurrency model, executed AS a model under -race: the walk and the
// janitors run simultaneously against one store with a quiesced node and an
// incident past its quiesce step. The race detector holds the memory model;
// the final assertion holds the domain invariant: the incident never ends up
// past the quiesce step once everything quiesces.
func TestWalkAndJanitorsRunConcurrently(t *testing.T) {
	book := &playbook.Playbook{Name: "reset", Target: "gpu", Steps: []playbook.Step{
		{Name: "quiesce", Action: "platform.quiesce_accelerator_stack"},
		{Name: "reset", Action: "agent.gpu_reset", Approval: "required"},
	}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"reset": book}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &stackPlatform{quiescedNodes: []string{"node-a"}}
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	c := New(st, st, engine, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
		nil, p, &blockingActuator{}, &recordingNotifier{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	now := time.Now()
	inc := &types.Incident{
		ID: "inc-race", Target: types.Target{Node: "node-a", GPUUUID: "GPU-a"},
		Class: types.ClassECCDBE, State: types.StateEvaluating,
		Playbook: "reset", StepIndex: 1,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	saved := acceleratorHostRestoreWait
	acceleratorHostRestoreWait = 20 * time.Millisecond
	t.Cleanup(func() { acceleratorHostRestoreWait = saved })

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.reconcile(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			passCtx := c.pinRuntimeConfig(ctx)
			c.restoreAbandonedAcceleratorStacks(passCtx)
			c.reconcileCordonedNodes(passCtx)
			c.resolveIncidentsOnVanishedNodes(passCtx)
		}
	}()
	time.Sleep(400 * time.Millisecond)
	close(stop)
	wg.Wait()

	final, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Whatever interleavings occurred, the incident is never past the quiesce
	// step while the janitor considers the node unowned: either the rewind
	// stuck (StepIndex 0) or the walk legitimately parked/advanced FROM the
	// rewound position.
	if final.StepIndex > 0 && final.State != types.StateAwaitingApproval && final.State != types.StateExecuting {
		t.Fatalf("final = state %s step %d — a stale walk write survived past the rewind", final.State, final.StepIndex)
	}
}
