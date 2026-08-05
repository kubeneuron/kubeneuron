package controller

// Round-7 item C: arming as data. The controller refuses an agent-destructive
// step at admission only on a FRESH, EXPLICIT "unarmed" declaration; unknown
// (old agents, v1 registrations) and stale declarations change nothing — the
// agent executor's own boundary remains the enforcement.

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

func TestUnarmedAgentRefusalBeforeApproval(t *testing.T) {
	book := &playbook.Playbook{
		Name: "reboot", Target: "node",
		Steps: []playbook.Step{{Name: "reboot", Action: "agent.reboot", Approval: "required"}},
	}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"reboot": book}, nil)
	if err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, arming types.AgentArming, lastSeen time.Time, dryRun bool) (*types.Incident, *recordingNotifier) {
		t.Helper()
		st, err := sqlite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		ctx := context.Background()
		notifier := &recordingNotifier{}
		c := New(st, st, engine, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
			nil, nil, nil, notifier, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := st.UpsertAgentRegistration(ctx, &types.Node{
			Name: "n1", AgentLastSeen: lastSeen, AgentArming: arming,
		}); err != nil {
			t.Fatal(err)
		}
		inc := &types.Incident{
			ID: "inc-arming", Target: types.Target{Node: "n1"},
			Class: types.ClassFellOffBus, State: types.StateEvaluating,
			Playbook: "reboot", DryRun: dryRun,
			OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
		}
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := c.advanceEvaluating(ctx, inc); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetIncident(ctx, inc.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got, notifier
	}

	t.Run("fresh unarmed escalates before any approval", func(t *testing.T) {
		inc, notifier := run(t, types.AgentArmingUnarmed, time.Now(), false)
		if inc.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN (escalate with no ladder quarantines), never AWAITING_APPROVAL", inc.State)
		}
		if len(notifier.approvals) != 0 {
			t.Fatal("no approval may be requested for a step the node's agent will provably refuse")
		}
	})
	t.Run("armed parks for approval", func(t *testing.T) {
		inc, _ := run(t, types.AgentArmingArmed, time.Now(), false)
		if inc.State != types.StateAwaitingApproval {
			t.Fatalf("state = %s, want AWAITING_APPROVAL", inc.State)
		}
	})
	t.Run("unknown arming parks (old agent, never a declared value)", func(t *testing.T) {
		inc, _ := run(t, types.AgentArmingUnknown, time.Now(), false)
		if inc.State != types.StateAwaitingApproval {
			t.Fatalf("state = %s, want AWAITING_APPROVAL", inc.State)
		}
	})
	t.Run("stale unarmed parks (the pod may have been replaced by an armed one)", func(t *testing.T) {
		inc, _ := run(t, types.AgentArmingUnarmed, time.Now().Add(-time.Hour), false)
		if inc.State != types.StateAwaitingApproval {
			t.Fatalf("state = %s, want AWAITING_APPROVAL", inc.State)
		}
	})
	t.Run("dry-run is never refused", func(t *testing.T) {
		inc, _ := run(t, types.AgentArmingUnarmed, time.Now(), true)
		if inc.State != types.StateAwaitingApproval {
			t.Fatalf("state = %s, want AWAITING_APPROVAL (dry-run executes nothing to refuse)", inc.State)
		}
	})
}

// The playbook-scope companion: a ladder whose LATER rung is agent-destructive
// must be refused before its first disruptive step, not after the node has
// been cordoned and drained.
func TestUnarmedAgentRefusedBeforeTheFirstDisruptiveStep(t *testing.T) {
	book := &playbook.Playbook{
		Name: "cordon-reboot", Target: "node",
		Steps: []playbook.Step{
			{Name: "cordon", Action: "platform.cordon"},
			{Name: "reboot", Action: "agent.reboot", Approval: "required"},
		},
	}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"cordon-reboot": book}, nil)
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
	if err := st.UpsertAgentRegistration(ctx, &types.Node{
		Name: "n1", AgentLastSeen: time.Now(), AgentArming: types.AgentArmingUnarmed,
	}); err != nil {
		t.Fatal(err)
	}
	inc := &types.Incident{
		ID: "inc-ladder", Target: types.Target{Node: "n1"},
		Class: types.ClassFellOffBus, State: types.StateEvaluating,
		Playbook: "cordon-reboot", DryRun: false,
		OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := c.advanceEvaluating(ctx, inc); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateNeedsHuman || got.StepIndex != 0 {
		t.Fatalf("state=%s step=%d, want refusal at step 0 — never cordon a node for a reboot its agent will refuse", got.State, got.StepIndex)
	}
	trail, err := st.AuditTrail(ctx, inc.ID)
	if err != nil || len(trail) == 0 {
		t.Fatalf("audit trail = %v, %v", trail, err)
	}
	_ = notify.EventNeedsHuman
}

// Round-8 (N4 retirement): the controller SERVES arming — the same selector
// computation as blast-radius confinement — so the two boundaries can never
// disagree, and every uncertainty answers unarmed.
func TestServedArmingMatchesTheDeclaredBlastRadius(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	c := New(st, st, nil, nil, nil, nil, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := st.UpsertNode(ctx, &types.Node{Name: "in", Labels: map[string]string{"pool": "canary"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNode(ctx, &types.Node{Name: "out", Labels: map[string]string{"pool": "prod"}}); err != nil {
		t.Fatal(err)
	}

	// No selector (not an Enabled install): everyone unarmed.
	if got := c.servedArming(ctx, "in"); got != types.AgentArmingUnarmed {
		t.Fatalf("no selector: served %q, want unarmed", got)
	}
	c.SetDestructiveNodeSelector(map[string]string{"pool": "canary"})
	if got := c.servedArming(ctx, "in"); got != types.AgentArmingArmed {
		t.Fatalf("in-scope node: served %q, want armed", got)
	}
	if got := c.servedArming(ctx, "out"); got != types.AgentArmingUnarmed {
		t.Fatalf("out-of-scope node: served %q, want unarmed", got)
	}
	// Unresolvable labels fail closed.
	if got := c.servedArming(ctx, "unknown-node"); got != types.AgentArmingUnarmed {
		t.Fatalf("unresolvable node: served %q, want unarmed", got)
	}
}

// R10.2/R11.1: a node INSIDE the destructive-execution blast radius whose
// agent freshly registers unarmed is arming-in-flight, not out of scope. The
// walk holds, and the propagation grace is measured from the FIRST hold
// observation — never from StateChangedAt, which pre-ages while an incident
// sits in EVALUATING behind a pause, maintenance window, or cooldown (review
// F1: the old anchor expired a grace that never ran). Past the real grace the
// declaration is a verdict: the agent can never adopt served arming and the
// incident escalates early instead of wasting a human approval.
func TestInScopeUnarmedAgentHoldsThenEscalates(t *testing.T) {
	book := &playbook.Playbook{
		Name: "reboot", Target: "node",
		Steps: []playbook.Step{{Name: "reboot", Action: "agent.reboot", Approval: "required"}},
	}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"reboot": book}, nil)
	if err != nil {
		t.Fatal(err)
	}

	fixture := func(t *testing.T, stateChanged time.Time) (*Controller, *sqlite.Store, *recordingNotifier, string) {
		t.Helper()
		st, err := sqlite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		ctx := context.Background()
		notifier := &recordingNotifier{}
		c := New(st, st, engine, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
			nil, nil, nil, notifier, slog.New(slog.NewTextHandler(io.Discard, nil)))
		c.SetDestructiveNodeSelector(map[string]string{"pool": "canary"})
		if err := st.UpsertNode(ctx, &types.Node{Name: "n1", Labels: map[string]string{"pool": "canary"}}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertAgentRegistration(ctx, &types.Node{
			Name: "n1", AgentLastSeen: time.Now(), AgentArming: types.AgentArmingUnarmed,
		}); err != nil {
			t.Fatal(err)
		}
		inc := &types.Incident{
			ID: "inc-inflight", Target: types.Target{Node: "n1"},
			Class: types.ClassFellOffBus, State: types.StateEvaluating,
			Playbook: "reboot",
			OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: stateChanged,
		}
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		return c, st, notifier, inc.ID
	}

	advance := func(t *testing.T, c *Controller, st *sqlite.Store, id string) *types.Incident {
		t.Helper()
		ctx := context.Background()
		inc, err := st.GetIncident(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.advanceEvaluating(ctx, inc); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetIncident(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("within the grace the walk holds", func(t *testing.T) {
		c, st, notifier, id := fixture(t, time.Now())
		if inc := advance(t, c, st, id); inc.State != types.StateEvaluating {
			t.Fatalf("state = %s, want still EVALUATING (held, re-checked next pass)", inc.State)
		}
		if len(notifier.approvals) != 0 {
			t.Fatal("no approval may be requested while arming is still propagating")
		}
	})
	t.Run("a pre-aged incident still gets its full grace (F1 regression)", func(t *testing.T) {
		// The incident sat in EVALUATING for 10 minutes behind a maintenance
		// window; StateChangedAt is ancient. The FIRST arming check must
		// still hold — the grace runs from first observation, not history.
		c, st, notifier, id := fixture(t, time.Now().Add(-10*time.Minute))
		if inc := advance(t, c, st, id); inc.State != types.StateEvaluating {
			t.Fatalf("state = %s, want EVALUATING (first observation must open a fresh grace)", inc.State)
		}
		if len(notifier.approvals) != 0 {
			t.Fatal("no approval may be requested while arming is still propagating")
		}
	})
	t.Run("a multi-step ladder still expires the grace (H1 regression)", func(t *testing.T) {
		// The standard cordon->drain->reboot shape: the preflight loop
		// checks EVERY step each pass, and the non-destructive cordon/drain
		// verdicts must NOT reset the reboot rung's hold anchor — that made
		// the grace unexpirable and a never-armable node hold forever.
		ladder := &playbook.Playbook{
			Name: "ladder", Target: "node",
			Steps: []playbook.Step{
				{Name: "Cordon", Action: "platform.cordon"},
				{Name: "Drain", Action: "platform.drain"},
				{Name: "Reboot", Action: "agent.reboot", Approval: "required"},
			},
		}
		ladderEngine, err := playbook.NewEngine(map[string]*playbook.Playbook{"ladder": ladder}, nil)
		if err != nil {
			t.Fatal(err)
		}
		st, err := sqlite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		ctx := context.Background()
		notifier := &recordingNotifier{}
		c := New(st, st, ladderEngine, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
			nil, nil, nil, notifier, slog.New(slog.NewTextHandler(io.Discard, nil)))
		c.SetDestructiveNodeSelector(map[string]string{"pool": "canary"})
		if err := st.UpsertNode(ctx, &types.Node{Name: "n1", Labels: map[string]string{"pool": "canary"}}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertAgentRegistration(ctx, &types.Node{
			Name: "n1", AgentLastSeen: time.Now(), AgentArming: types.AgentArmingUnarmed,
		}); err != nil {
			t.Fatal(err)
		}
		inc := &types.Incident{
			ID: "inc-ladder", Target: types.Target{Node: "n1"},
			Class: types.ClassFellOffBus, State: types.StateEvaluating,
			Playbook: "ladder",
			OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
		}
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		// First pass: hold recorded.
		got, err := st.GetIncident(ctx, inc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.advanceEvaluating(ctx, got); err != nil {
			t.Fatal(err)
		}
		if got, _ = st.GetIncident(ctx, inc.ID); got.State != types.StateEvaluating {
			t.Fatalf("first pass state = %s, want a hold", got.State)
		}
		// Age the anchor, then run TWO more passes: the cordon/drain proceed
		// verdicts in each pass must not reset it.
		c.armingHoldMu.Lock()
		c.armingHoldSince[inc.ID] = time.Now().Add(-armingPropagationGrace - time.Minute)
		c.armingHoldMu.Unlock()
		got, _ = st.GetIncident(ctx, inc.ID)
		if err := c.advanceEvaluating(ctx, got); err != nil {
			t.Fatal(err)
		}
		if got, _ = st.GetIncident(ctx, inc.ID); got.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN — the multi-step ladder must still expire the grace", got.State)
		}
	})

	t.Run("past the observed grace the node is never-armable and escalates", func(t *testing.T) {
		c, st, notifier, id := fixture(t, time.Now())
		if inc := advance(t, c, st, id); inc.State != types.StateEvaluating {
			t.Fatalf("first pass state = %s, want a hold", inc.State)
		}
		// Age the hold anchor itself past the grace: the next pass treats the
		// fresh-unarmed declaration as a verdict.
		c.armingHoldMu.Lock()
		c.armingHoldSince[id] = time.Now().Add(-armingPropagationGrace - time.Minute)
		c.armingHoldMu.Unlock()
		if inc := advance(t, c, st, id); inc.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN (early escalation, no approval round)", inc.State)
		}
		if len(notifier.approvals) != 0 {
			t.Fatal("a never-armable node must escalate without asking for approval")
		}
		// The transition must have cleared the hold anchor (no leak).
		c.armingHoldMu.Lock()
		_, still := c.armingHoldSince[id]
		c.armingHoldMu.Unlock()
		if still {
			t.Fatal("the escalating transition must clear the arming-hold anchor")
		}
	})
}
