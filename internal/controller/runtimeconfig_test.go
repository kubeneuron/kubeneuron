package controller

// Round-7 item B: the reloadable runtime configuration is one immutable
// snapshot behind an atomic pointer. Before it, eight independently-locked
// fields were installed by eight sequential setters — mixed generations were
// observable mid-pass, and the two timing fields were guarded by nothing
// (a real data race under -race).

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Under -race this test fails on the pre-snapshot code: SetTimings wrote the
// unguarded timing fields while the walk read them.
func TestRuntimeConfigInstallRacesReconcile(t *testing.T) {
	book := &playbook.Playbook{Name: "pb", Target: "gpu",
		Steps: []playbook.Step{{Name: "observe", Action: "notify.observe"}}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"pb": book}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	c := New(st, st, engine, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
		nil, nil, nil, &recordingNotifier{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	now := time.Now()
	inc := &types.Incident{
		ID: "inc-race", Target: types.Target{Node: "n1", GPUUUID: "GPU-1"},
		Class: types.ClassECCDBE, State: types.StateVerifying, Playbook: "pb",
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := c.InstallRuntimeConfig(RuntimeConfig{
				Engine:      engine,
				VerifyQuiet: time.Duration(i%10+1) * time.Minute,
				ApprovalTTL: time.Duration(i%5+1) * time.Hour,
				Windows:     []config.MaintenanceWindow{{Name: "w"}},
			}); err != nil {
				t.Error(err)
				return
			}
			c.SetTimings(time.Minute, time.Hour)
		}
	}()
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			c.reconcile(ctx)
		}
		close(stop)
	}()
	wg.Wait()
}

// Every reader observes one coherent snapshot: an install replaces all fields
// together, and zero timings keep their current values (the SetTimings
// contract, now at the snapshot level).
func TestInstallRuntimeConfigIsAtomicAndZeroTimingsKeepCurrent(t *testing.T) {
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := New(st, st, engine, nil, nil, nil, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := c.InstallRuntimeConfig(RuntimeConfig{
		Engine:              engine,
		QuiesceForbidden:    []string{"dcp"},
		DestructiveSelector: map[string]string{"pool": "canary"},
		VerifyQuiet:         5 * time.Minute,
		ApprovalTTL:         2 * time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	rc := c.runtimeConfig(context.Background())
	if rc.VerifyQuiet != 5*time.Minute || rc.ApprovalTTL != 2*time.Hour ||
		len(rc.QuiesceForbidden) != 1 || rc.DestructiveSelector["pool"] != "canary" {
		t.Fatalf("snapshot = %+v, want the installed values", rc)
	}

	// Zero timings keep current; other fields are replaced wholesale.
	if err := c.InstallRuntimeConfig(RuntimeConfig{Engine: engine}); err != nil {
		t.Fatal(err)
	}
	rc = c.runtimeConfig(context.Background())
	if rc.VerifyQuiet != 5*time.Minute || rc.ApprovalTTL != 2*time.Hour {
		t.Fatalf("zero timings must keep current values, got %+v", rc)
	}
	if len(rc.QuiesceForbidden) != 0 || rc.DestructiveSelector != nil {
		t.Fatalf("non-timing fields are replaced wholesale, got %+v", rc)
	}

	// A per-field test hook copies-on-write without disturbing the rest.
	c.SetQuiesceForbiddenHolders([]string{"x"})
	rc2 := c.runtimeConfig(context.Background())
	if len(rc2.QuiesceForbidden) != 1 || rc2.VerifyQuiet != 5*time.Minute {
		t.Fatalf("CoW hook must change one field only, got %+v", rc2)
	}
	if rc == rc2 {
		t.Fatal("a mutation must install a NEW snapshot, never edit the old one")
	}
}

// A long-running step completes against the engine that ADMITTED it, even if
// the configuration was hot-swapped mid-step; the next advance re-reads fresh.
func TestStepCompletesAgainstTheAdmittingEngine(t *testing.T) {
	twoStep := &playbook.Playbook{Name: "pb", Target: "node",
		Steps: []playbook.Step{
			{Name: "diag", Action: "agent.run_diag"},
			{Name: "collect", Action: "agent.collect_bundle"},
		}}
	admitting, err := playbook.NewEngine(map[string]*playbook.Playbook{"pb": twoStep}, nil)
	if err != nil {
		t.Fatal(err)
	}
	oneStep := &playbook.Playbook{Name: "pb", Target: "node",
		Steps: []playbook.Step{{Name: "diag", Action: "agent.run_diag"}}}
	swapped, err := playbook.NewEngine(map[string]*playbook.Playbook{"pb": oneStep}, nil)
	if err != nil {
		t.Fatal(err)
	}

	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	release := make(chan struct{})
	act := &gatedActuator{release: release}
	c := New(st, st, admitting, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
		nil, nil, act, &recordingNotifier{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	now := time.Now()
	inc := &types.Incident{
		ID: "inc-pin", Target: types.Target{Node: "n1"},
		Class: types.ClassFellOffBus, State: types.StateEvaluating, Playbook: "pb",
		DryRun: false, OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	step := &twoStep.Steps[0]
	if err := c.startStep(ctx, inc, step, "system"); err != nil {
		t.Fatal(err)
	}
	// Hot-swap the engine while the step is blocked mid-execution.
	c.SetEngine(swapped)
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := st.GetIncident(ctx, inc.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Under the ADMITTING engine the playbook has a second step, so the
		// post-step decision must be EVALUATING (more work), not VERIFYING
		// (done) — which is what the swapped one-step engine would answer.
		if got.State == types.StateEvaluating && got.StepIndex == 1 {
			break
		}
		if got.State == types.StateVerifying {
			t.Fatal("post-step decision used the hot-swapped engine, not the one that admitted the step")
		}
		if time.Now().After(deadline) {
			t.Fatalf("step never completed; state=%s step=%d", got.State, got.StepIndex)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// gatedActuator blocks each Execute until released, then succeeds.
type gatedActuator struct{ release chan struct{} }

func (a *gatedActuator) Name() string { return "gated-test" }
func (a *gatedActuator) Capabilities() []types.ActionType {
	return []types.ActionType{types.ActionRunDiag, types.ActionCollectBundle}
}
func (a *gatedActuator) Healthy(context.Context, types.Node) error { return nil }
func (a *gatedActuator) Execute(ctx context.Context, _ types.Node, act types.Action) (*types.ActionResult, error) {
	select {
	case <-a.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &types.ActionResult{ActionID: act.ID, OK: true, Output: "ok"}, nil
}
