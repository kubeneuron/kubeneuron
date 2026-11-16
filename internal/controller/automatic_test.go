// SPDX-License-Identifier: Apache-2.0

package controller

// #56: recovery modes. An automatic policy is an approval given in advance;
// its limits hold a step, and anything it does not cover goes to a person.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const autoPolicy = "auto-fell-off-bus"

func autoSettings() *recoverymode.Settings {
	return &recoverymode.Settings{
		NodeSelector:        map[string]string{"pool": "train"},
		PoolLabel:           "pool",
		MaxConcurrentNodes:  2,
		NodeCooldown:        time.Hour,
		MaxUnhealthyPercent: 50,
		EnabledBy:           "alice",
		EnabledAt:           time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC),
		Acknowledgement:     recoverymode.Acknowledgement,
	}
}

func modeEngine(t *testing.T, mode recoverymode.Mode, settings *recoverymode.Settings) *playbook.Engine {
	t.Helper()
	books, err := playbook.LoadDir("../../configs/playbooks")
	if err != nil {
		t.Fatal(err)
	}
	// The playbook's own cool-down would hide the policy's node cool-down,
	// and its reboot is a node-level alpha action an automatic policy may not
	// authorise: the fixture ladder recovers through the platform only.
	book := books["fell-off-bus"]
	book.Cooldown = 0
	var steps []playbook.Step
	for _, s := range book.Steps {
		if s.Action != "agent.reboot" {
			steps = append(steps, s)
		}
	}
	book.Steps = steps
	engine, err := playbook.NewEngine(books, []playbook.Policy{
		{Name: autoPolicy, Class: types.ClassFellOffBus, Playbook: "fell-off-bus", Mode: mode, Automatic: settings},
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

// autoFixture: a dry-run gate (steps simulate, so the whole ladder can run),
// a policy in the given mode, and pool nodes n1..nN labelled pool=train.
func autoFixture(t *testing.T, mode recoverymode.Mode, settings *recoverymode.Settings, poolNodes int) (*Controller, *sqlite.Store, *recordingNotifier) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for i := 1; i <= poolNodes; i++ {
		if err := st.UpsertNode(ctx, &types.Node{Name: nodeName(i), Labels: map[string]string{"pool": "train"}}); err != nil {
			t.Fatal(err)
		}
	}
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 8, MaxConcurrentReboots: 8, DryRun: true})
	notifier := &recordingNotifier{}
	c := New(st, st, modeEngine(t, mode, settings), gate, safety.NewFlapDetector(100, time.Hour), nil, nil, notifier,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAutomaticEligibility(func(context.Context, string) (bool, string) { return true, "" })
	c.SetTimings(time.Millisecond, 0) // tiny verification quiet window
	return c, st, notifier
}

func nodeName(i int) string { return "n" + string(rune('0'+i)) }

func openOn(t *testing.T, c *Controller, node string) string {
	t.Helper()
	ctx := context.Background()
	if err := c.ingest(ctx, signal(types.ClassFellOffBus, node, "GPU-"+node)); err != nil {
		t.Fatal(err)
	}
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{Node: node})
	if err != nil || len(incidents) == 0 {
		t.Fatalf("no incident opened on %s: %v", node, err)
	}
	return incidents[0].ID
}

// step runs reconcile passes and returns the incident; it never approves.
func step(t *testing.T, c *Controller, st *sqlite.Store, id string, passes int) *types.Incident {
	t.Helper()
	ctx := context.Background()
	var inc *types.Incident
	for i := 0; i < passes; i++ {
		c.reconcile(ctx)
		time.Sleep(2 * time.Millisecond)
		var err error
		if inc, err = st.GetIncident(ctx, id); err != nil {
			t.Fatal(err)
		}
		if inc.State == types.StateAwaitingApproval || inc.State.Halted() {
			return inc
		}
	}
	return inc
}

func auditActors(t *testing.T, st *sqlite.Store, id string) (actors []string, results []string) {
	t.Helper()
	trail, err := st.AuditTrail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range trail {
		actors = append(actors, e.Actor)
		results = append(results, e.Result)
	}
	return actors, results
}

func TestAutomaticPolicyRunsTheLadderAndRecordsThePolicy(t *testing.T) {
	c, st, notifier := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 200)
	if inc.State != types.StateResolved {
		t.Fatalf("state=%s, want RESOLVED without any approval", inc.State)
	}
	if len(notifier.approvals) != 0 {
		t.Fatalf("approval requests %v, want none under an automatic policy", notifier.approvals)
	}
	actors, results := auditActors(t, st, id)
	var byPolicy, note bool
	for i, a := range actors {
		if a == "policy:"+autoPolicy {
			byPolicy = true
			if strings.Contains(results[i], "authorised by policy "+autoPolicy+": automatic mode declared enabled by alice at 2026-11-15T09:00:00Z in generation 0, observed at ") {
				note = true
			}
		}
	}
	if !byPolicy || !note {
		t.Fatalf("audit actors %v results %v: want steps by policy:%s naming who enabled it", actors, results, autoPolicy)
	}
}

func TestApprovalModeStillParks(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Approval, nil, 4)
	if inc := step(t, c, st, openOn(t, c, "n1"), 50); inc.State != types.StateAwaitingApproval {
		t.Fatalf("state=%s, want AWAITING_APPROVAL in approval mode", inc.State)
	}
}

func TestDryRunPolicyStampsIncidentsDryRun(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	live := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 8, MaxConcurrentReboots: 8, DryRun: false})
	c := New(st, st, modeEngine(t, recoverymode.DryRun, nil), live, nil, nil, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	inc, err := st.GetIncident(context.Background(), openOn(t, c, "n1"))
	if err != nil {
		t.Fatal(err)
	}
	if !inc.DryRun {
		t.Fatal("a dry-run policy must open its incidents in dry-run, even on a live installation")
	}
}

func parkReason(t *testing.T, st *sqlite.Store, id string) string {
	t.Helper()
	_, results := auditActors(t, st, id)
	return strings.Join(results, " | ")
}

func TestAutomaticFallsBackToApproval(t *testing.T) {
	t.Run("node outside the pools", func(t *testing.T) {
		c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
		if err := st.UpsertNode(context.Background(), &types.Node{Name: "other", Labels: map[string]string{"pool": "infer"}}); err != nil {
			t.Fatal(err)
		}
		id := openOn(t, c, "other")
		if inc := step(t, c, st, id, 50); inc.State != types.StateAwaitingApproval || !strings.Contains(parkReason(t, st, id), "outside the policy's node pools") {
			t.Fatalf("state=%s audit=%s, want a park naming the pool", inc.State, parkReason(t, st, id))
		}
	})
	t.Run("circuit breaker", func(t *testing.T) {
		c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 2)
		first := openOn(t, c, "n1")
		setNodePaused(t, st, "n1", true) // keeps the first incident open
		step(t, c, st, first, 10)
		id := openOn(t, c, "n2") // 2 of 2 nodes unhealthy > 50%, at the floor of 2
		if inc := step(t, c, st, id, 50); inc.State != types.StateAwaitingApproval || !strings.Contains(parkReason(t, st, id), "circuit breaker") {
			t.Fatalf("state=%s audit=%s, want the breaker to hand the step to a person", inc.State, parkReason(t, st, id))
		}
	})
}

func TestAutomaticLimitsHoldTheStep(t *testing.T) {
	t.Run("concurrency", func(t *testing.T) {
		settings := autoSettings()
		settings.MaxConcurrentNodes = 1
		c, st, _ := autoFixture(t, recoverymode.Automatic, settings, 8)
		busy := openOn(t, c, "n1")
		setNodePaused(t, st, "n1", true) // keeps the first recovery open
		if err := c.appendAudit(context.Background(), mustIncident(t, st, busy), "policy:"+autoPolicy, "cordon", "running"); err != nil {
			t.Fatal(err)
		}
		id := openOn(t, c, "n2")
		inc := step(t, c, st, id, 20)
		if inc.State != types.StateEvaluating {
			t.Fatalf("state=%s, want the step held at EVALUATING while the limit is reached", inc.State)
		}
	})
	t.Run("node cool-down", func(t *testing.T) {
		c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 8)
		first := openOn(t, c, "n1")
		if inc := step(t, c, st, first, 200); inc.State != types.StateResolved {
			t.Fatalf("first recovery: state=%s", inc.State)
		}
		again := openOn(t, c, "n1")
		if again == first {
			t.Fatal("expected a new incident after the first resolved")
		}
		if inc := step(t, c, st, again, 20); inc.State != types.StateEvaluating {
			t.Fatalf("state=%s, want the second automatic recovery of the node held by the cool-down", inc.State)
		}
	})
	t.Run("node pause", func(t *testing.T) {
		c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
		setNodePaused(t, st, "n1", true)
		if inc := step(t, c, st, openOn(t, c, "n1"), 20); inc.State != types.StateEvaluating {
			t.Fatalf("state=%s, want a paused node held", inc.State)
		}
	})
}

func mustIncident(t *testing.T, st *sqlite.Store, id string) *types.Incident {
	t.Helper()
	inc, err := st.GetIncident(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return inc
}

// Switching the policy back to approval mid-incident parks the next step.
func TestSwitchingToApprovalParksTheNextStep(t *testing.T) {
	book := &playbook.Playbook{Name: "cloud", Target: "node", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon"},
		{Name: "drain", Action: "platform.drain", Approval: "required"},
		{Name: "replace", Action: "platform.replace_node", Approval: "required"},
	}}
	engineIn := func(mode recoverymode.Mode, settings *recoverymode.Settings) *playbook.Engine {
		engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"cloud": book}, []playbook.Policy{{
			Name: autoPolicy, Class: types.ClassFellOffBus, Playbook: "cloud", Mode: mode, Automatic: settings,
			ReplacementStrategy: recoverymode.RemoveFirst,
		}})
		if err != nil {
			t.Fatal(err)
		}
		return engine
	}
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	c.SetEngine(engineIn(recoverymode.Automatic, autoSettings()))
	id := openOn(t, c, "n1")
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		c.reconcile(ctx)
		time.Sleep(2 * time.Millisecond)
		if _, results := auditActors(t, st, id); strings.Contains(strings.Join(results, "|"), ": drain") {
			break
		}
	}
	c.SetEngine(engineIn(recoverymode.Approval, nil))
	if inc := step(t, c, st, id, 200); inc.State != types.StateAwaitingApproval {
		t.Fatalf("state=%s, want the replace parked after the switch", inc.State)
	}
}

// Defence in depth: a step that needs approval never starts as "system".
func TestStartStepRefusesAnUnauthorisedApprovalStep(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Approval, nil, 4)
	id := openOn(t, c, "n1")
	inc := mustIncident(t, st, id)
	inc.State = types.StateEvaluating
	if err := st.UpdateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	book, _ := c.runtimeConfig(context.Background()).Engine.Playbook("fell-off-bus")
	var gated *playbook.Step
	for i := range book.Steps {
		if book.Steps[i].NeedsApproval() {
			gated = &book.Steps[i]
			break
		}
	}
	if gated == nil {
		t.Fatal("fixture playbook has no approval step")
	}
	for _, actor := range []string{"system", "policy:" + autoPolicy} {
		fresh := mustIncident(t, st, id)
		if err := c.startStep(context.Background(), fresh, gated, actor); err != nil {
			t.Fatal(err)
		}
		if got := mustIncident(t, st, id); got.State == types.StateExecuting {
			t.Fatalf("actor %q started an approval step without a person or a live automatic policy", actor)
		}
	}
}

func TestCheckpointWaitIsCappedByTheAutomaticPolicy(t *testing.T) {
	base := checkpoint.Policy{Enabled: true, DefaultWait: 10 * time.Minute, MaxWait: 30 * time.Minute}
	got := capCheckpointWait(base, 5*time.Minute)
	if got.MaxWait != 5*time.Minute || got.DefaultWait != 5*time.Minute {
		t.Fatalf("capped policy = %+v, want both waits at 5m", got)
	}
	if got := capCheckpointWait(base, 0); got.MaxWait != base.MaxWait || got.DefaultWait != base.DefaultWait {
		t.Fatalf("zero checkpoint_wait changed the policy: %+v", got)
	}
	if got := capCheckpointWait(base, time.Hour); got.MaxWait != base.MaxWait {
		t.Fatalf("a longer checkpoint_wait must not extend the installation's ceiling: %+v", got)
	}
}

// A pool the provider has not confirmed is unavailable, not a question for a
// person: the step is held and no approval is asked.
func TestIneligiblePoolHoldsInsteadOfAskingAPerson(t *testing.T) {
	for name, eligibility := range map[string]AutomaticEligibility{
		"no provider answer": nil,
		"observe-only pool":  func(context.Context, string) (bool, string) { return false, "EKS Auto Mode repairs nodes on its own" },
	} {
		t.Run(name, func(t *testing.T) {
			c, st, notifier := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
			c.SetAutomaticEligibility(eligibility)
			if inc := step(t, c, st, openOn(t, c, "n1"), 20); inc.State != types.StateEvaluating {
				t.Fatalf("state=%s, want the step held", inc.State)
			}
			if len(notifier.approvals) != 0 {
				t.Fatalf("approval requested %v on an ineligible pool", notifier.approvals)
			}
		})
	}
}

// The breaker does not trip below its floor: the first fault in a two-node
// pool is recovered automatically.
func TestBreakerFloorKeepsSmallPoolsAutomatic(t *testing.T) {
	settings := autoSettings()
	settings.MaxUnhealthyPercent = 30 // one of two nodes is above the share; the floor decides
	c, st, notifier := autoFixture(t, recoverymode.Automatic, settings, 2)
	if inc := step(t, c, st, openOn(t, c, "n1"), 200); inc.State != types.StateResolved || len(notifier.approvals) != 0 {
		t.Fatalf("state=%s approvals=%v, want an automatic recovery of the first fault", inc.State, notifier.approvals)
	}
}

// A tripped breaker stays tripped for the policy revision, even after the
// pool recovers, until a person changes the policy.
func TestTrippedBreakerStaysUntilThePolicyChanges(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 2)
	first := openOn(t, c, "n1")
	setNodePaused(t, st, "n1", true)
	step(t, c, st, first, 10)
	tripping := openOn(t, c, "n2")
	step(t, c, st, tripping, 50)
	if !strings.Contains(parkReason(t, st, tripping), "circuit breaker") {
		t.Fatalf("precondition: breaker did not trip: %s", parkReason(t, st, tripping))
	}
	// The pool recovers: both incidents end.
	ctx := context.Background()
	for _, id := range []string{first, tripping} {
		if err := c.ResolveIncident(ctx, id, "token:alice", "fixed by hand"); err != nil {
			t.Fatal(err)
		}
	}
	setNodePaused(t, st, "n1", false)
	next := openOn(t, c, "n1")
	if inc := step(t, c, st, next, 50); inc.State != types.StateAwaitingApproval || !strings.Contains(parkReason(t, st, next), "tripped for this revision") {
		t.Fatalf("state=%s audit=%s, want the policy still in approval after the trip", inc.State, parkReason(t, st, next))
	}
	// A person changes the policy (a new generation): automatic again.
	changed := autoSettings()
	changed.Generation = 2
	c.SetEngine(modeEngine(t, recoverymode.Automatic, changed))
	if err := c.ResolveIncident(ctx, next, "token:alice", "re-enable"); err != nil {
		t.Fatal(err)
	}
	again := openOn(t, c, "n2")
	if inc := step(t, c, st, again, 200); inc.State != types.StateResolved {
		t.Fatalf("state=%s, want automatic recovery after the policy changed", inc.State)
	}
}

func replaceEngine(t *testing.T, strategy recoverymode.Strategy, recycle bool) *playbook.Engine {
	t.Helper()
	book := &playbook.Playbook{Name: "cloud", Target: "node", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon"},
		{Name: "recycle", Action: "platform.recycle_node", Approval: "required"},
		{Name: "replace", Action: "platform.replace_node", Approval: "required"},
	}}
	if !recycle {
		book.Steps = append(book.Steps[:1], book.Steps[2])
	}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"cloud": book}, []playbook.Policy{{
		Name: autoPolicy, Class: types.ClassFellOffBus, Playbook: "cloud", Mode: recoverymode.Automatic,
		Automatic: autoSettings(), ReplacementStrategy: strategy, RecycleEnabled: recycle,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestReplacementStrategyAndRecycleUnderAutomatic(t *testing.T) {
	t.Run("add-first unavailable holds the replace step", func(t *testing.T) {
		c, st, notifier := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
		c.SetEngine(replaceEngine(t, "", false))
		id := openOn(t, c, "n1")
		inc := step(t, c, st, id, 30)
		if inc.State != types.StateEvaluating || len(notifier.approvals) != 0 {
			t.Fatalf("state=%s approvals=%v, want the replace held, not done another way", inc.State, notifier.approvals)
		}
		if actors, results := auditActors(t, st, id); strings.Contains(strings.Join(results, "|"), "replace") && strings.Contains(strings.Join(actors, "|"), "policy:") {
			for i, r := range results {
				if strings.Contains(r, ": replace") {
					t.Fatalf("replace authorised under add-first: %s by %s", r, actors[i])
				}
			}
		}
	})
	t.Run("explicit remove-first runs", func(t *testing.T) {
		c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
		c.SetEngine(replaceEngine(t, recoverymode.RemoveFirst, false))
		id := openOn(t, c, "n1")
		step(t, c, st, id, 100)
		if _, results := auditActors(t, st, id); !strings.Contains(strings.Join(results, "|"), ": replace") {
			t.Fatalf("audit %v, want the replace authorised by the policy", results)
		}
	})
	t.Run("recycle without recycle.enabled is held, not offered to a person", func(t *testing.T) {
		c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
		engine := replaceEngine(t, recoverymode.RemoveFirst, true)
		pol, _ := engine.PolicyFor(types.ClassFellOffBus, "")
		pol.RecycleEnabled = false
		book, _ := engine.Playbook("cloud")
		disabled, err := playbook.NewEngine(map[string]*playbook.Playbook{"cloud": book}, []playbook.Policy{pol})
		if err != nil {
			t.Fatal(err)
		}
		c.SetEngine(disabled)
		id := openOn(t, c, "n1")
		if inc := step(t, c, st, id, 50); inc.State != types.StateEvaluating || !strings.Contains(parkReason(t, st, id), "recovery unavailable: the policy does not enable the restart") {
			t.Fatalf("state=%s audit=%s, want the recycle held as unavailable", inc.State, parkReason(t, st, id))
		}
	})
}

// The installation-wide dry-run overrides an automatic policy: every step is
// simulated and audited as dry-run.
func TestAutomaticUnderInstallationDryRunExecutesNothing(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4) // the fixture gate is dry-run
	id := openOn(t, c, "n1")
	if inc := step(t, c, st, id, 200); !inc.DryRun {
		t.Fatal("incident not dry-run under a dry-run installation")
	}
	trail, err := st.AuditTrail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range trail {
		if e.ToState == types.StateExecuting && !e.DryRun {
			t.Fatalf("a step executed for real under installation dry-run: %+v", e)
		}
	}
}

// The strategy and recycle rules hold in approval mode too: a person's
// approval does not replace another way or restart a disabled recycle.
func TestReplacementRulesHoldUnderApproval(t *testing.T) {
	for name, tc := range map[string]struct {
		book    playbook.Step
		recycle bool
		want    string
	}{
		"add-first replace": {playbook.Step{Name: "replace", Action: "platform.replace_node", Approval: "required"}, false, "add-first replacement is not available"},
		"disabled recycle":  {playbook.Step{Name: "recycle", Action: "platform.recycle_node", Approval: "required"}, false, "recycle.enabled"},
	} {
		t.Run(name, func(t *testing.T) {
			c, st, notifier := autoFixture(t, recoverymode.Approval, nil, 4)
			book := &playbook.Playbook{Name: "cloud", Target: "node", Steps: []playbook.Step{{Name: "cordon", Action: "platform.cordon"}, tc.book}}
			engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"cloud": book}, []playbook.Policy{{
				Name: "p", Class: types.ClassFellOffBus, Playbook: "cloud", Mode: recoverymode.Approval, RecycleEnabled: tc.recycle,
			}})
			if err != nil {
				t.Fatal(err)
			}
			c.SetEngine(engine)
			id := openOn(t, c, "n1")
			inc := step(t, c, st, id, 50)
			if inc.State != types.StateEvaluating || len(notifier.approvals) != 0 || !strings.Contains(parkReason(t, st, id), tc.want) {
				t.Fatalf("state=%s approvals=%v audit=%s, want the step held and no approval asked", inc.State, notifier.approvals, parkReason(t, st, id))
			}
			// Even an approval recorded some other way cannot start it.
			fresh := mustIncident(t, st, id)
			if err := c.startStep(context.Background(), fresh, &book.Steps[1], "token:alice"); err != nil {
				t.Fatal(err)
			}
			if got := mustIncident(t, st, id); got.State == types.StateExecuting {
				t.Fatal("a person's approval started a step the policy does not allow")
			}
		})
	}
}

// Node-level actions are alpha: an automatic policy never authorises them,
// the step goes to a person, and a policy actor cannot start one.
func TestAutomaticNeverAuthorisesNodeLevelActions(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	book := &playbook.Playbook{Name: "node", Target: "node", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon"},
		{Name: "reboot", Action: "agent.reboot", Approval: "required"},
	}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"node": book}, []playbook.Policy{{
		Name: autoPolicy, Class: types.ClassFellOffBus, Playbook: "node", Mode: recoverymode.Automatic, Automatic: autoSettings(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)
	id := openOn(t, c, "n1")
	if inc := step(t, c, st, id, 50); inc.State != types.StateAwaitingApproval || !strings.Contains(parkReason(t, st, id), "node-level actions are alpha") {
		t.Fatalf("state=%s audit=%s, want the reboot handed to a person", inc.State, parkReason(t, st, id))
	}
	fresh := mustIncident(t, st, id)
	fresh.State = types.StateEvaluating
	if err := st.UpdateIncident(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	if err := c.startStep(context.Background(), mustIncident(t, st, id), &book.Steps[1], "policy:"+autoPolicy); err != nil {
		t.Fatal(err)
	}
	if got := mustIncident(t, st, id); got.State == types.StateExecuting {
		t.Fatal("a policy actor started a node-level action")
	}
}
