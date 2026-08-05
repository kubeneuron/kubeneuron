package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/actuator"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// recordingNotifier captures notifications for assertions.
type recordingNotifier struct {
	mu        sync.Mutex
	events    []notify.NotifyEvent
	approvals []string
}

func (r *recordingNotifier) Notify(_ context.Context, ev notify.NotifyEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingNotifier) RequestApproval(_ context.Context, inc *types.Incident, step string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.approvals = append(r.approvals, inc.ID+"/"+step)
	return nil
}

func (r *recordingNotifier) kinds() []notify.EventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]notify.EventKind, len(r.events))
	for i, ev := range r.events {
		out[i] = ev.Kind
	}
	return out
}

// walkFixture wires a controller with the shipped playbooks, an in-memory
// store, and a recording notifier. dryRun controls the per-incident stamp.
func walkFixture(t *testing.T, dryRun bool) (*Controller, *sqlite.Store, *recordingNotifier) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	books, err := playbook.LoadDir("../../configs/playbooks")
	if err != nil {
		t.Fatal(err)
	}
	policies := []playbook.Policy{
		{Class: types.ClassFellOffBus, Playbook: "fell-off-bus"},
		{Class: types.ClassECCDBE, Playbook: "drain-and-reset"},
		{Class: types.ClassXIDApp, Playbook: "observe-suspect", Params: map[string]string{"threshold": "3", "window": "1h"}},
		{Class: types.ClassThermal, Playbook: "observe-suspect"},
	}
	engine, err := playbook.NewEngine(books, policies)
	if err != nil {
		t.Fatal(err)
	}
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1, DryRun: dryRun})
	notifier := &recordingNotifier{}
	c := New(st, st, engine, gate, safety.NewFlapDetector(3, 24*time.Hour), nil, nil, notifier,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c, st, notifier
}

// drive runs reconcile passes until the incident reaches want or attempts
// run out, waiting for in-flight steps between passes.
func drive(t *testing.T, c *Controller, st *sqlite.Store, id string, want types.IncidentState) *types.Incident {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		c.reconcile(ctx)
		time.Sleep(2 * time.Millisecond) // let step goroutines finish
		inc, err := st.GetIncident(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if inc.State == want {
			return inc
		}
	}
	inc, _ := st.GetIncident(ctx, id)
	t.Fatalf("incident never reached %s; stuck at %s (playbook %s, step %d, attempt %d)",
		want, inc.State, inc.Playbook, inc.StepIndex, inc.Attempt)
	return nil
}

func openIncidentFor(t *testing.T, c *Controller, class types.ProblemClass) string {
	t.Helper()
	ctx := context.Background()
	if err := c.ingest(ctx, signal(class, "n1", "GPU-1")); err != nil {
		t.Fatal(err)
	}
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil || len(incidents) == 0 {
		t.Fatalf("no incident opened: %v", err)
	}
	return incidents[0].ID
}

func TestWalkDryRunFullLadderToVerifying(t *testing.T) {
	c, st, notifier := walkFixture(t, true)
	id := openIncidentFor(t, c, types.ClassECCDBE) // drain-and-reset: 5 steps

	inc := drive(t, c, st, id, types.StateVerifying)
	if inc.StepIndex != 5 {
		t.Fatalf("StepIndex = %d, want 5 (all steps executed)", inc.StepIndex)
	}

	trail, err := st.AuditTrail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var dryRunSteps int
	for _, e := range trail {
		if e.DryRun && strings.Contains(e.Result, "DRY-RUN") {
			dryRunSteps++
		}
	}
	if dryRunSteps < 5 {
		t.Fatalf("dry-run step audits = %d, want >= 5; trail: %+v", dryRunSteps, trail)
	}
	for _, k := range notifier.kinds() {
		if k == notify.EventResolved {
			t.Fatal("must not resolve before the quiet window")
		}
	}
}

func TestWalkVerifyingResolvesAfterQuietWindow(t *testing.T) {
	c, st, notifier := walkFixture(t, true)
	c.SetTimings(time.Millisecond, 0) // tiny quiet window for the test
	id := openIncidentFor(t, c, types.ClassECCDBE)

	inc := drive(t, c, st, id, types.StateResolved)
	if inc.ResolvedAt == nil {
		t.Fatal("ResolvedAt must be set on resolution")
	}
	resolved := false
	for _, k := range notifier.kinds() {
		if k == notify.EventResolved {
			resolved = true
		}
	}
	if !resolved {
		t.Fatal("resolution must be notified")
	}
	// The playbook cooldown must now be recorded on the gate.
	if c.gate.CooldownRemaining(types.Target{Node: "n1", GPUUUID: "GPU-1"}, playbookCooldownAction("drain-and-reset")) == 0 {
		t.Fatal("playbook cooldown must be recorded after resolve")
	}
}

func TestWalkVerificationFailureEscalates(t *testing.T) {
	c, st, _ := walkFixture(t, true)
	id := openIncidentFor(t, c, types.ClassECCDBE)

	drive(t, c, st, id, types.StateVerifying)
	// A recurring signal during the quiet window fails verification.
	if err := c.ingest(context.Background(), signal(types.ClassECCDBE, "n1", "GPU-1")); err != nil {
		t.Fatal(err)
	}
	c.reconcile(context.Background())
	inc, err := st.GetIncident(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != types.StateEvaluating {
		t.Fatalf("state = %s, want EVALUATING (escalation)", inc.State)
	}
	if inc.Playbook != "reboot" || inc.Attempt != 1 || inc.StepIndex != 0 {
		t.Fatalf("escalation = playbook %s attempt %d step %d, want reboot/1/0",
			inc.Playbook, inc.Attempt, inc.StepIndex)
	}
}

func TestWalkApprovalParkAndApprove(t *testing.T) {
	c, st, notifier := walkFixture(t, true)
	id := openIncidentFor(t, c, types.ClassFellOffBus) // fell-off-bus: reboot needs approval

	drive(t, c, st, id, types.StateAwaitingApproval)
	if len(notifier.approvals) == 0 {
		t.Fatal("approval request must be delivered")
	}

	// A human approves through the real decision API, which binds the decision
	// to the current approval round's request; the incident resumes and
	// finishes the ladder.
	if err := c.DecideApproval(context.Background(), id, "alice", "cli", types.ApprovalApproved, 0, ""); err != nil {
		t.Fatal(err)
	}
	inc := drive(t, c, st, id, types.StateVerifying)
	if inc.State != types.StateVerifying {
		t.Fatalf("state = %s", inc.State)
	}
}

func TestWalkApprovalRejectionQuarantines(t *testing.T) {
	c, st, notifier := walkFixture(t, true)
	id := openIncidentFor(t, c, types.ClassFellOffBus)

	drive(t, c, st, id, types.StateAwaitingApproval)
	if err := c.DecideApproval(context.Background(), id, "bob", "cli", types.ApprovalRejected, 0, ""); err != nil {
		t.Fatal(err)
	}
	drive(t, c, st, id, types.StateNeedsHuman)
	needsHuman := false
	for _, k := range notifier.kinds() {
		if k == notify.EventNeedsHuman {
			needsHuman = true
		}
	}
	if !needsHuman {
		t.Fatal("rejection must notify needs-human")
	}
}

func TestWalkApprovalExpiresAfterTTL(t *testing.T) {
	c, st, notifier := walkFixture(t, true)
	c.SetTimings(0, time.Millisecond)
	id := openIncidentFor(t, c, types.ClassFellOffBus)

	drive(t, c, st, id, types.StateAwaitingApproval)
	time.Sleep(5 * time.Millisecond)
	inc := drive(t, c, st, id, types.StateExpired)
	if inc.State != types.StateExpired {
		t.Fatalf("state = %s", inc.State)
	}
	expired := false
	for _, k := range notifier.kinds() {
		if k == notify.EventExpired {
			expired = true
		}
	}
	if !expired {
		t.Fatal("expiry must be notified")
	}
}

func TestWalkObserveThresholdEscalates(t *testing.T) {
	c, st, _ := walkFixture(t, true)
	ctx := context.Background()
	id := openIncidentFor(t, c, types.ClassXIDApp) // observe-suspect, threshold 3

	drive(t, c, st, id, types.StateObserving)
	// Two more signals cross the threshold of 3.
	for i := 0; i < 2; i++ {
		if err := c.ingest(ctx, signal(types.ClassXIDApp, "n1", "GPU-1")); err != nil {
			t.Fatal(err)
		}
	}
	inc := drive(t, c, st, id, types.StateEvaluating)
	if inc.SignalSeen < 3 {
		t.Fatalf("SignalSeen = %d", inc.SignalSeen)
	}
}

func TestWalkObserveQuietResolves(t *testing.T) {
	c, st, _ := walkFixture(t, true)
	id := openIncidentFor(t, c, types.ClassThermal) // observe, no threshold params

	drive(t, c, st, id, types.StateObserving)
	// Age the incident past the observation window.
	ctx := context.Background()
	inc, err := st.GetIncident(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	inc.UpdatedAt = time.Now().Add(-25 * time.Hour)
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	drive(t, c, st, id, types.StateResolved)
}

func TestWalkFlapQuarantine(t *testing.T) {
	c, st, _ := walkFixture(t, true)
	c.SetTimings(time.Millisecond, 0)
	ctx := context.Background()

	// Resolve the same (target, class) three times, then reopen a fourth
	// time: three resolve→reopen cycles hit the flap threshold (3/24h) and
	// the detector must quarantine the reopened run. The first-ever open is
	// not a reopen, and opens without a prior resolution never count.
	for i := 0; i < 3; i++ {
		id := openIncidentFor(t, c, types.ClassECCDBE)
		drive(t, c, st, id, types.StateResolved)
		// Clear the playbook cooldown so the next run is not merely delayed.
		c.gate = safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1, DryRun: true})
	}
	if err := c.ingest(ctx, signal(types.ClassECCDBE, "n1", "GPU-1")); err != nil {
		t.Fatal(err)
	}
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{States: []types.IncidentState{types.StateOpen}})
	if err != nil || len(incidents) != 1 {
		t.Fatalf("open incidents = %d, %v", len(incidents), err)
	}
	drive(t, c, st, incidents[0].ID, types.StateNeedsHuman)
}

func TestWalkGateDenialHoldsPosition(t *testing.T) {
	c, st, _ := walkFixture(t, true)
	c.gate.Pause()
	id := openIncidentFor(t, c, types.ClassECCDBE)

	drive(t, c, st, id, types.StateEvaluating)
	// Paused gate: several passes must not advance past EVALUATING.
	for i := 0; i < 3; i++ {
		c.reconcile(context.Background())
	}
	inc, err := st.GetIncident(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != types.StateEvaluating || inc.StepIndex != 0 {
		t.Fatalf("paused gate must hold: state %s step %d", inc.State, inc.StepIndex)
	}
	c.gate.Resume()
	drive(t, c, st, id, types.StateVerifying)
}

func TestWalkOrphanedExecutionRecovers(t *testing.T) {
	c, st, _ := walkFixture(t, true)
	id := openIncidentFor(t, c, types.ClassECCDBE)
	ctx := context.Background()

	inc := drive(t, c, st, id, types.StateEvaluating)
	// Simulate a controller crash mid-step: EXECUTING persisted, no goroutine.
	if err := c.transition(ctx, inc, types.StateExecuting, "system", "cordon", "executing", nil); err != nil {
		t.Fatal(err)
	}
	drive(t, c, st, id, types.StateVerifying) // recovers and finishes
}

// failingActuator makes every agent step fail, driving escalation in
// non-dry-run mode.
type failingActuator struct{}

func (failingActuator) Name() string { return "failing" }
func (failingActuator) Capabilities() []types.ActionType {
	return []types.ActionType{types.ActionGPUReset}
}
func (failingActuator) Healthy(context.Context, types.Node) error { return nil }
func (failingActuator) Execute(context.Context, types.Node, types.Action) (*types.ActionResult, error) {
	return nil, errors.New("agent unreachable")
}

func TestWalkStepFailureEscalatesLadder(t *testing.T) {
	c, st, _ := walkFixture(t, false) // NOT dry-run: steps really dispatch
	c.actuator = &actuator.Chain{Actuators: []actuator.Actuator{failingActuator{}}}
	c.platform = nil

	id := openIncidentFor(t, c, types.ClassECCDBE)
	// drain-and-reset step 1 is platform.cordon; with no platform it fails
	// and escalates: drain-and-reset -> reboot -> ... until NEEDS_HUMAN.
	inc := drive(t, c, st, id, types.StateNeedsHuman)
	if inc.Attempt == 0 {
		t.Fatal("failure path must consume escalation attempts")
	}
	trail, err := st.AuditTrail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	sawFailure := false
	for _, e := range trail {
		if strings.HasPrefix(e.Result, "FAILED:") {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Fatal("step failures must be audited")
	}
}

func TestWalkHoldsDuringMaintenanceWindow(t *testing.T) {
	c, st, _ := walkFixture(t, true)
	ctx := context.Background()

	// A global window (no selector) covering now holds every incident.
	c.SetMaintenanceWindows([]config.MaintenanceWindow{{
		Name:     "global-pm",
		StartsAt: time.Now().Add(-time.Hour),
		EndsAt:   time.Now().Add(time.Hour),
	}})
	id := openIncidentFor(t, c, types.ClassECCDBE)
	drive(t, c, st, id, types.StateEvaluating)
	for i := 0; i < 3; i++ {
		c.reconcile(ctx)
	}
	inc, err := st.GetIncident(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != types.StateEvaluating || inc.StepIndex != 0 {
		t.Fatalf("window must hold: state %s step %d", inc.State, inc.StepIndex)
	}

	// Window ends: the incident proceeds.
	c.SetMaintenanceWindows([]config.MaintenanceWindow{{
		Name:     "global-pm",
		StartsAt: time.Now().Add(-2 * time.Hour),
		EndsAt:   time.Now().Add(-time.Hour),
	}})
	drive(t, c, st, id, types.StateVerifying)
}

func TestWalkSelectorWindowMatchesNodeLabels(t *testing.T) {
	c, st, _ := walkFixture(t, true)
	ctx := context.Background()

	// Node n1 is in rack 42; the active window selects rack 42.
	if err := st.UpsertNode(ctx, &types.Node{
		Name: "n1", Platform: "kubernetes", Labels: map[string]string{"rack": "42"},
	}); err != nil {
		t.Fatal(err)
	}
	c.SetMaintenanceWindows([]config.MaintenanceWindow{{
		Name:        "rack-42-pm",
		StartsAt:    time.Now().Add(-time.Hour),
		EndsAt:      time.Now().Add(time.Hour),
		MatchLabels: map[string]string{"rack": "42"},
	}})
	id := openIncidentFor(t, c, types.ClassECCDBE)
	drive(t, c, st, id, types.StateEvaluating)
	for i := 0; i < 3; i++ {
		c.reconcile(ctx)
	}
	inc, _ := st.GetIncident(ctx, id)
	if inc.State != types.StateEvaluating {
		t.Fatalf("selector window must hold matching node: %s", inc.State)
	}

	// A window for a different rack does not hold n1.
	c.SetMaintenanceWindows([]config.MaintenanceWindow{{
		Name:        "rack-7-pm",
		StartsAt:    time.Now().Add(-time.Hour),
		EndsAt:      time.Now().Add(time.Hour),
		MatchLabels: map[string]string{"rack": "7"},
	}})
	drive(t, c, st, id, types.StateVerifying)
}

func TestWalkHoldsOnNodeConfigPause(t *testing.T) {
	c, st, _ := walkFixture(t, true)
	ctx := context.Background()

	if err := c.ApplyNodeConfigs(ctx, []config.NodeConfig{{NodeName: "n1", Paused: true}}); err != nil {
		t.Fatal(err)
	}
	id := openIncidentFor(t, c, types.ClassECCDBE)
	drive(t, c, st, id, types.StateEvaluating)
	for i := 0; i < 3; i++ {
		c.reconcile(ctx)
	}
	inc, err := st.GetIncident(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != types.StateEvaluating || inc.StepIndex != 0 {
		t.Fatalf("config-paused node must hold: state %s step %d", inc.State, inc.StepIndex)
	}

	// Removing the node config unpauses and the incident proceeds.
	if err := c.ApplyNodeConfigs(ctx, nil); err != nil {
		t.Fatal(err)
	}
	drive(t, c, st, id, types.StateVerifying)
}

func TestWalkUsesSignalCatalogOverrides(t *testing.T) {
	c, st, _ := walkFixture(t, true)
	ctx := context.Background()

	// Override XID 79 from critical fell-off-bus to an observe-only class.
	catalog, err := detect.NewCatalog([]config.SignalOverride{{
		Name: "xid-79-observe", XIDCodes: []int{79},
		Class: types.ClassThermal, Severity: types.SeverityWarning,
	}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetSignalCatalog(catalog)

	req := httptest.NewRequest("POST", "/api/v1/events", nil)
	if err := c.HandleAgentEvent(req, types.AgentEvent{
		EventID: "ov-1", Node: "n1", GPUUUID: "GPU-1", XID: 79, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	c.drainEventOutbox(ctx)
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil || len(incidents) != 1 {
		t.Fatalf("incidents = %v, %v", incidents, err)
	}
	if incidents[0].Playbook != "observe-suspect" {
		t.Fatalf("playbook = %s, want observe-suspect via thermal policy", incidents[0].Playbook)
	}
}
