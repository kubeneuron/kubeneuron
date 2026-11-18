// SPDX-License-Identifier: Apache-2.0

package controller

// Cloud interruptions. The cloud announces that it will take a node; the
// workloads on it are asked to checkpoint before the deadline, and the node
// is cordoned. Nothing else runs: there is no drain and no replacement.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/interruption"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// interruptionPlatform announces scripted notices and records the cordons
// it is asked for and released.
type interruptionPlatform struct {
	checkpointPlatform
	nmu      sync.Mutex
	notices  []interruption.Notice
	readErr  error
	cordons  []string
	released []string
	gone     map[string]bool
}

func (p *interruptionPlatform) NodeExists(_ context.Context, node string) (bool, error) {
	p.nmu.Lock()
	defer p.nmu.Unlock()
	return !p.gone[node], nil
}

func (p *interruptionPlatform) Name() string { return "interruption-test" }

func (p *interruptionPlatform) NodeInterruptions(context.Context) ([]interruption.Notice, error) {
	p.nmu.Lock()
	defer p.nmu.Unlock()
	return append([]interruption.Notice(nil), p.notices...), p.readErr
}

func (p *interruptionPlatform) announce(n ...interruption.Notice) {
	p.nmu.Lock()
	defer p.nmu.Unlock()
	p.notices = n
}

func (p *interruptionPlatform) CordonForOwner(_ context.Context, node, owner, _ string) error {
	p.nmu.Lock()
	defer p.nmu.Unlock()
	p.cordons = append(p.cordons, node+"/"+owner)
	return nil
}

func (p *interruptionPlatform) ReleaseCordonOwners(_ context.Context, node string, owners []string) (bool, int, error) {
	p.nmu.Lock()
	defer p.nmu.Unlock()
	p.released = append(p.released, node+"/"+strings.Join(owners, ","))
	return true, 0, nil
}

func (p *interruptionPlatform) NodeReady(context.Context, string) (bool, error) { return true, nil }

func (p *interruptionPlatform) cordoned() []string {
	p.nmu.Lock()
	defer p.nmu.Unlock()
	return append([]string(nil), p.cordons...)
}

func spotNotice(node string, deadline time.Time) interruption.Notice {
	return interruption.Notice{Node: node, Interruption: cloud.Interruption{
		InstanceID: "aws:///zone/i-" + node, Kind: cloud.InterruptionSpotReclaim, Deadline: deadline,
		Source: interruption.SourceNTHSpot, Key: interruption.SourceNTHSpot + "/" + node + "/event-1",
	}}
}

const interruptionPolicy = "cloud-interruption"

// interruptionEngine is the cloud pack's playbook: the warning, which needs
// no approval, then the cordon, which does unless a policy authorises it.
func interruptionEngine(t *testing.T, mode recoverymode.Mode, settings *recoverymode.Settings, extra ...playbook.Policy) *playbook.Engine {
	t.Helper()
	books := map[string]*playbook.Playbook{
		"interruption": {Name: "interruption", Target: "node", Steps: []playbook.Step{
			{Name: "warn", Action: playbook.CheckpointWarningAction},
			{Name: "cordon", Action: "platform.cordon", Approval: "required"},
		}},
		"cloud-replace": {Name: "cloud-replace", Target: "node", Steps: []playbook.Step{
			{Name: "cordon", Action: "platform.cordon", Approval: "required"},
			{Name: "drain", Action: "platform.drain", Approval: "required"},
		}},
	}
	policies := append([]playbook.Policy{{
		Name: interruptionPolicy, Class: types.ClassCloudInterruption, Playbook: "interruption", Mode: mode, Automatic: settings,
		ReplacementStrategy: recoverymode.AddFirst,
	}}, extra...)
	engine, err := playbook.NewEngine(books, policies)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

// interruptionFixture: a live gate, a checkpoint policy that coordinates
// with the training namespace, and pool nodes n1..nN.
func interruptionFixture(t *testing.T, engine *playbook.Engine, limits safety.Limits, nodes int) (*Controller, *sqlite.Store, *interruptionPlatform, *fakeClock) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for i := 1; i <= nodes; i++ {
		if err := st.UpsertNode(ctx, &types.Node{Name: nodeName(i), Labels: map[string]string{"pool": "train"}}); err != nil {
			t.Fatal(err)
		}
	}
	plat := &interruptionPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	c := New(st, st, engine, safety.NewGate(limits), safety.NewFlapDetector(100, time.Hour), plat, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetTimings(time.Millisecond, 0)
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.Checkpoint = enabledPolicy() })
	clock := &fakeClock{now: time.Now()}
	c.checkpointNow = clock.Now
	return c, st, plat, clock
}

func liveLimits() safety.Limits {
	return safety.Limits{MaxConcurrentRemediations: 8, MaxConcurrentReboots: 8}
}

// interruptionIncident is the node's open cloud-interruption incident.
func interruptionIncident(t *testing.T, st *sqlite.Store, node string) *types.Incident {
	t.Helper()
	incidents, err := st.ListIncidents(context.Background(), store.IncidentFilter{Node: node})
	if err != nil {
		t.Fatal(err)
	}
	for _, inc := range incidents {
		if inc.Class == types.ClassCloudInterruption {
			return inc
		}
	}
	t.Fatalf("no cloud-interruption incident on %s", node)
	return nil
}

// settle runs reconcile passes until the incident stops moving.
func settle(t *testing.T, c *Controller, st *sqlite.Store, id string) *types.Incident {
	t.Helper()
	ctx := context.Background()
	var inc *types.Incident
	for i := 0; i < 20; i++ {
		c.reconcile(ctx)
		time.Sleep(2 * time.Millisecond)
		for c.isInFlight(id) {
			time.Sleep(time.Millisecond)
		}
		var err error
		if inc, err = st.GetIncident(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	return inc
}

func trailActions(t *testing.T, st *sqlite.Store, id string) (actions, results []string) {
	t.Helper()
	trail, err := st.AuditTrail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range trail {
		actions = append(actions, e.Action)
		results = append(results, e.Result)
	}
	return actions, results
}

func count(items []string, want string) int {
	n := 0
	for _, s := range items {
		if s == want {
			n++
		}
	}
	return n
}

func containsText(items []string, substr string) bool {
	for _, s := range items {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func TestInterruptionNoticeWarnsAndParksTheCordonForApproval(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Approval, nil), liveLimits(), 1)
	plat.announce(spotNotice("n1", time.Time{}))
	ctx := context.Background()

	c.pollInterruptions(ctx)
	inc := settle(t, c, st, interruptionIncident(t, st, "n1").ID)

	if plat.requests["trainer"] != 1 {
		t.Fatalf("the job was asked %d times, want once", plat.requests["trainer"])
	}
	if inc.State != types.StateAwaitingApproval {
		t.Fatalf("state %s, want the cordon parked for a person", inc.State)
	}
	if got := plat.cordoned(); len(got) != 0 {
		t.Fatalf("cordoned without approval: %v", got)
	}
	actions, results := trailActions(t, st, inc.ID)
	if count(actions, InterruptionAction) != 1 || !containsText(results, "asked 1 of 1 workload(s)") {
		t.Fatalf("trail does not record the notice and the warning:\n%v\n%v", actions, results)
	}
	if containsText(results, "drain") {
		t.Fatalf("an interruption never reaches a drain: %v", results)
	}
}

func TestInterruptionNoticeIsRecordedOnce(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Approval, nil), liveLimits(), 1)
	plat.announce(spotNotice("n1", time.Time{}))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		c.pollInterruptions(ctx)
	}
	// A restart forgets the keys; the trail still holds the notice.
	c.interruptions.keep(nil)
	c.pollInterruptions(ctx)

	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{Node: "n1"})
	if err != nil || len(incidents) != 1 {
		t.Fatalf("incidents %v, err %v; want one", len(incidents), err)
	}
	actions, _ := trailActions(t, st, incidents[0].ID)
	if n := count(actions, InterruptionAction); n != 1 {
		t.Fatalf("notice recorded %d times, want once", n)
	}
}

func TestInterruptionAutomaticCordonsWithoutApproval(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Automatic, autoSettings()), liveLimits(), 1)
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(context.Background())
	inc := settle(t, c, st, interruptionIncident(t, st, "n1").ID)

	if got := plat.cordoned(); len(got) != 1 || got[0] != "n1/"+inc.ID {
		t.Fatalf("cordons %v, want n1 owned by the incident", got)
	}
	if plat.requests["trainer"] != 1 {
		t.Fatalf("the job was asked %d times, want once", plat.requests["trainer"])
	}

	// The incident stays open, and the node cordoned, until the cloud acts:
	// quiet time, a further notice and the cordon janitor change nothing.
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.VerifyQuiet = 0 })
	later := spotNotice("n1", time.Time{})
	later.Key += "-again"
	plat.announce(spotNotice("n1", time.Time{}), later)
	c.pollInterruptions(context.Background())
	inc = settle(t, c, st, inc.ID)
	c.reconcileCordonedNodes(context.Background())
	if inc.State.Halted() || inc.State == types.StateNeedsHuman {
		t.Fatalf("state %s; the incident must wait for the cloud", inc.State)
	}
	if len(plat.released) != 0 {
		t.Fatalf("released the cordon before the cloud acted: %v", plat.released)
	}
}

func TestInterruptionDryRunOnlySaysWhatItWouldDo(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.DryRun, nil), liveLimits(), 1)
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(context.Background())
	inc := settle(t, c, st, interruptionIncident(t, st, "n1").ID)

	if len(plat.requests) != 0 || len(plat.cordoned()) != 0 {
		t.Fatalf("dry run asked %v and cordoned %v", plat.requests, plat.cordoned())
	}
	_, results := trailActions(t, st, inc.ID)
	if !containsText(results, "would warn the workloads on n1") {
		t.Fatalf("no would-warn row: %v", results)
	}
}

func TestInterruptionFarDeadlineWaitsForTheLead(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Automatic, autoSettings()), liveLimits(), 1)
	plat.announce(spotNotice("n1", time.Now().Add(2*time.Hour)))
	c.pollInterruptions(context.Background())
	inc := settle(t, c, st, interruptionIncident(t, st, "n1").ID)

	if len(plat.requests) != 0 || len(plat.cordoned()) != 0 {
		t.Fatalf("acted two hours ahead: asked %v, cordoned %v", plat.requests, plat.cordoned())
	}
	if inc.State.Halted() {
		t.Fatalf("incident closed early: %s", inc.State)
	}
}

func TestInterruptionPassedDeadlineAsksNobody(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Approval, nil), liveLimits(), 1)
	plat.announce(spotNotice("n1", time.Now().Add(-time.Minute)))
	c.pollInterruptions(context.Background())
	inc := settle(t, c, st, interruptionIncident(t, st, "n1").ID)

	if len(plat.requests) != 0 {
		t.Fatalf("asked after the deadline: %v", plat.requests)
	}
	_, results := trailActions(t, st, inc.ID)
	if !containsText(results, "leaves no time for a checkpoint") {
		t.Fatalf("no missed row: %v", results)
	}
}

func TestInterruptionWithoutJobsRecordsSo(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Approval, nil), liveLimits(), 1)
	plat.workloads = nil
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(context.Background())
	inc := settle(t, c, st, interruptionIncident(t, st, "n1").ID)

	_, results := trailActions(t, st, inc.ID)
	if !containsText(results, "no workload on n1 takes part in checkpoint coordination") {
		t.Fatalf("no no-jobs row: %v", results)
	}
}

func TestInterruptionRebalanceOnlyRecords(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Automatic, autoSettings()), liveLimits(), 1)
	n := spotNotice("n1", time.Time{})
	n.Kind, n.Key = cloud.InterruptionRebalance, "rebalance/n1"
	plat.announce(n)
	c.pollInterruptions(context.Background())
	inc := settle(t, c, st, interruptionIncident(t, st, "n1").ID)

	if len(plat.requests) != 0 || len(plat.cordoned()) != 0 {
		t.Fatalf("a rebalance hint asked %v and cordoned %v", plat.requests, plat.cordoned())
	}
	if inc.State.Halted() {
		t.Fatalf("incident closed: %s", inc.State)
	}
}

// A mass interruption reaches every node: the warning does not wait for the
// breaker, the concurrency limits or the cooldowns. Those hold the cordon.
func TestInterruptionWarningIgnoresBreakerAndLimits(t *testing.T) {
	settings := autoSettings()
	settings.MaxConcurrentNodes = 1
	limits := safety.Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1}
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Automatic, settings), limits, 5)
	var notices []interruption.Notice
	for i := 1; i <= 5; i++ {
		notices = append(notices, spotNotice(nodeName(i), time.Time{}))
	}
	plat.announce(notices...)
	c.pollInterruptions(context.Background())
	for i := 1; i <= 5; i++ {
		settle(t, c, st, interruptionIncident(t, st, nodeName(i)).ID)
	}

	if got := plat.requests["trainer"]; got != 5 {
		t.Fatalf("warned %d nodes, want all 5", got)
	}
	// Every node is unhealthy at once: the breaker trips and no cordon is
	// authorised by the policy.
	if got := plat.cordoned(); len(got) > 1 {
		t.Fatalf("cordons %v passed the breaker and the limits", got)
	}
}

// The replacement gates (a qualified profile pair, the fault located on the
// node) guard a replacement; an interruption has none, so they do not hold it.
func TestInterruptionIgnoresReplacementGates(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Automatic, autoSettings()), liveLimits(), 1)
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(context.Background())
	inc := settle(t, c, st, interruptionIncident(t, st, "n1").ID)

	if got := plat.cordoned(); len(got) != 1 {
		t.Fatalf("cordons %v; the add-first policy without evidence or qualified profiles must still cordon", got)
	}
	if inc.State == types.StateAwaitingApproval {
		t.Fatalf("parked for approval: %s", inc.State)
	}
}

// A node that already has an open incident gets the warning and a note in
// that incident; its cordon is left to that incident.
func TestInterruptionOnANodeWithAnotherIncident(t *testing.T) {
	other := playbook.Policy{Name: "fell-off-bus", Class: types.ClassFellOffBus, Playbook: "cloud-replace", Mode: recoverymode.Approval}
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Automatic, autoSettings(), other), liveLimits(), 1)
	ctx := context.Background()
	if err := c.ingest(ctx, signal(types.ClassFellOffBus, "n1", "GPU-n1")); err != nil {
		t.Fatal(err)
	}
	first, err := st.ListIncidents(ctx, store.IncidentFilter{Node: "n1"})
	if err != nil || len(first) != 1 {
		t.Fatalf("incidents %d, err %v", len(first), err)
	}
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(ctx)
	inc := settle(t, c, st, interruptionIncident(t, st, "n1").ID)

	if plat.requests["trainer"] != 1 {
		t.Fatalf("the job was asked %d times, want once", plat.requests["trainer"])
	}
	for _, got := range plat.cordoned() {
		if strings.HasSuffix(got, "/"+inc.ID) {
			t.Fatalf("the interruption cordoned a node another incident owns: %v", plat.cordoned())
		}
	}
	actions, _ := trailActions(t, st, first[0].ID)
	if count(actions, InterruptionAction) != 1 {
		t.Fatalf("the other incident does not note the interruption: %v", actions)
	}
}

func TestInterruptionEndedClosesAndReleasesOnlyItsCordon(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Automatic, autoSettings()), liveLimits(), 1)
	ctx := context.Background()
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(ctx)
	id := interruptionIncident(t, st, "n1").ID
	settle(t, c, st, id)

	plat.announce()
	c.pollInterruptions(ctx)
	inc, err := st.GetIncident(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != types.StateResolved {
		t.Fatalf("state %s, want resolved as ended", inc.State)
	}
	if actions, results := trailActions(t, st, id); count(actions, interruptionEndedAction) != 1 || !containsText(results, "cancelled or completed") {
		t.Fatalf("not closed as ended:\n%v\n%v", actions, results)
	}
	if len(plat.released) != 1 || plat.released[0] != "n1/"+id {
		t.Fatalf("released %v, want only the incident's own cordon", plat.released)
	}
}

func TestInterruptionReadFailureChangesNothing(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Automatic, autoSettings()), liveLimits(), 1)
	ctx := context.Background()
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(ctx)
	id := interruptionIncident(t, st, "n1").ID

	plat.announce()
	plat.readErr = context.DeadlineExceeded
	c.pollInterruptions(ctx)
	if inc, _ := st.GetIncident(ctx, id); inc.State.Halted() {
		t.Fatalf("a failed read closed the incident: %s", inc.State)
	}
}

func TestInterruptionOnAnUnmanagedNodeIsIgnored(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Approval, nil), liveLimits(), 1)
	plat.announce(spotNotice("elsewhere", time.Time{}))
	c.pollInterruptions(context.Background())
	if incidents, _ := st.ListIncidents(context.Background(), store.IncidentFilter{}); len(incidents) != 0 {
		t.Fatalf("opened %d incidents for a node KubeNeuron does not manage", len(incidents))
	}
}

func TestInterruptionGoneNodeResolves(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Approval, nil), liveLimits(), 1)
	ctx := context.Background()
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(ctx)
	id := interruptionIncident(t, st, "n1").ID
	plat.gone = map[string]bool{"n1": true}
	plat.announce()
	c.pollInterruptions(ctx)
	if inc, _ := st.GetIncident(ctx, id); inc.State.Halted() {
		t.Fatalf("a gone node's incident was closed as ended: %s", inc.State)
	}
	c.resolveIncidentsOnVanishedNodes(ctx)
	if inc, _ := st.GetIncident(ctx, id); inc.State != types.StateResolved {
		t.Fatalf("state %s, want resolved once the node is gone", inc.State)
	}
}

func TestInterruptionRecordsHowTheWarnedJobsAnswered(t *testing.T) {
	for _, tc := range []struct {
		name string
		ack  bool
		want string
	}{
		{"acknowledged", true, "1 of 1 asked workload(s) reported a checkpoint or finished"},
		{"silent past the deadline", false, "0 of 1 asked workload(s) reported a checkpoint or finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, st, plat, clock := interruptionFixture(t, interruptionEngine(t, recoverymode.Approval, nil), liveLimits(), 1)
			ctx := context.Background()
			plat.announce(spotNotice("n1", time.Time{}))
			c.pollInterruptions(ctx)
			id := interruptionIncident(t, st, "n1").ID
			settle(t, c, st, id)
			plat.observe = func(w platform.Workload, _ int) checkpoint.Observation {
				obs := stampedAck(w, id, clock.Now().Add(time.Minute))
				if !tc.ack {
					delete(obs.Annotations, checkpoint.AnnotationState)
				}
				return obs
			}
			if !tc.ack {
				c.pollInterruptions(ctx)
				if _, results := trailActions(t, st, id); containsText(results, "asked workload(s) reported") {
					t.Fatalf("outcome recorded before the deadline: %v", results)
				}
				clock.Advance(5 * time.Minute)
			}
			c.pollInterruptions(ctx)
			c.pollInterruptions(ctx)
			actions, results := trailActions(t, st, id)
			if count(actions, interruptionOutcomeAction) != 1 || !containsText(results, tc.want) {
				t.Fatalf("want one outcome %q:\n%v\n%v", tc.want, actions, results)
			}
		})
	}
}

func TestInterruptionWithoutAPolicyIsOnlyObserved(t *testing.T) {
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, st, plat, _ := interruptionFixture(t, engine, liveLimits(), 1)
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(context.Background())
	inc := settle(t, c, st, interruptionIncident(t, st, "n1").ID)
	if len(plat.requests) != 0 || len(plat.cordoned()) != 0 {
		t.Fatalf("without a policy, asked %v and cordoned %v", plat.requests, plat.cordoned())
	}
	if inc.State != types.StateObserving && !inc.State.Halted() {
		t.Fatalf("state %s, want observed only", inc.State)
	}
}

// countingStore counts the reads of a poll pass and can fail node reads.
type countingStore struct {
	store.Store
	mu         sync.Mutex
	getNodeErr error
	lists      int
	trails     int
}

func (s *countingStore) GetNode(ctx context.Context, name string) (*types.Node, error) {
	s.mu.Lock()
	err := s.getNodeErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.Store.GetNode(ctx, name)
}

func (s *countingStore) ListIncidents(ctx context.Context, f store.IncidentFilter) ([]*types.Incident, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.Store.ListIncidents(ctx, f)
}

func (s *countingStore) AuditTrail(ctx context.Context, id string) ([]*types.AuditEntry, error) {
	s.mu.Lock()
	s.trails++
	s.mu.Unlock()
	return s.Store.AuditTrail(ctx, id)
}

func (s *countingStore) reads() (lists, trails int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lists, trails = s.lists, s.trails
	s.lists, s.trails = 0, 0
	return lists, trails
}

// A node the store cannot read keeps its notice in force: a database error
// must not read as "the notice is gone" and close the incident.
func TestInterruptionStoreErrorKeepsTheIncident(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Automatic, autoSettings()), liveLimits(), 1)
	ctx := context.Background()
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(ctx)
	id := interruptionIncident(t, st, "n1").ID
	settle(t, c, st, id)

	cs := &countingStore{Store: st, getNodeErr: errors.New("database is locked")}
	c.store = cs
	c.pollInterruptions(ctx)
	if inc, _ := st.GetIncident(ctx, id); inc.State.Halted() {
		t.Fatalf("a failed node read closed the incident: %s", inc.State)
	}
	if len(plat.released) != 0 {
		t.Fatalf("a failed node read released the cordon: %v", plat.released)
	}
}

func TestInterruptionUnreadableWorkloadsAtTheDeadlineAreUnknown(t *testing.T) {
	c, st, plat, clock := interruptionFixture(t, interruptionEngine(t, recoverymode.Approval, nil), liveLimits(), 1)
	ctx := context.Background()
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(ctx)
	id := interruptionIncident(t, st, "n1").ID
	settle(t, c, st, id)

	plat.listErr = errors.New("apiserver unavailable")
	c.pollInterruptions(ctx)
	if _, results := trailActions(t, st, id); containsText(results, "could not be read") {
		t.Fatalf("outcome recorded before the deadline: %v", results)
	}
	clock.Advance(5 * time.Minute)
	c.pollInterruptions(ctx)
	c.pollInterruptions(ctx)
	actions, results := trailActions(t, st, id)
	if count(actions, interruptionOutcomeAction) != 1 || !containsText(results, "could not be read at the deadline") {
		t.Fatalf("want one unknown outcome:\n%v\n%v", actions, results)
	}
	if containsText(results, "0 of 1 asked") {
		t.Fatalf("unread workloads reported as missed: %v", results)
	}
}

// A notice the cloud has not acted on an hour past its deadline, on a node
// still there, goes to a person with the cordon kept.
func TestInterruptionOverdueGoesToAPerson(t *testing.T) {
	for _, tc := range []struct {
		name string
		ago  time.Duration
		want types.IncidentState
	}{
		{"an hour past the deadline", 2 * time.Hour, types.StateNeedsHuman},
		{"shortly past the deadline", 10 * time.Minute, types.StateVerifying},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Automatic, autoSettings()), liveLimits(), 1)
			plat.announce(spotNotice("n1", time.Now().Add(-tc.ago)))
			c.pollInterruptions(context.Background())
			inc := settle(t, c, st, interruptionIncident(t, st, "n1").ID)
			if inc.State != tc.want {
				t.Fatalf("state %s, want %s", inc.State, tc.want)
			}
			if len(plat.cordoned()) != 1 || len(plat.released) != 0 {
				t.Fatalf("cordons %v, released %v; want the cordon kept", plat.cordoned(), plat.released)
			}
			if tc.want == types.StateNeedsHuman {
				if _, results := trailActions(t, st, inc.ID); !containsText(results, "the signal may be stuck") {
					t.Fatalf("no reason recorded: %v", results)
				}
			}
		})
	}
}

// A notice without a time whose assumed notice is no longer than the margin
// is still sent, against the cloud's assumed deadline.
func TestInterruptionImminentNoticeIsAskedAtTheCloudsDeadline(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Approval, nil), liveLimits(), 1)
	plat.announce(interruption.Notice{Node: "n1", Interruption: cloud.Interruption{
		Kind: cloud.InterruptionScheduledMaintenance, Source: interruption.SourceAKSScheduledEvent,
		Key: interruption.SourceAKSScheduledEvent + "/n1/t",
	}})
	c.pollInterruptions(context.Background())
	inc := settle(t, c, st, interruptionIncident(t, st, "n1").ID)
	if plat.requests["trainer"] != 1 {
		t.Fatalf("the job was asked %d times, want once", plat.requests["trainer"])
	}
	if _, results := trailActions(t, st, inc.ID); !containsText(results, "at the cloud's assumed deadline") {
		t.Fatalf("no warning against the assumed deadline: %v", results)
	}
}

// A pass lists the open incidents once, and stops reading the trail of an
// incident whose warning outcome is recorded.
func TestInterruptionPassReadsTheIncidentsOnce(t *testing.T) {
	c, st, plat, clock := interruptionFixture(t, interruptionEngine(t, recoverymode.Approval, nil), liveLimits(), 1)
	ctx := context.Background()
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(ctx)
	id := interruptionIncident(t, st, "n1").ID
	settle(t, c, st, id)
	plat.observe = func(w platform.Workload, _ int) checkpoint.Observation {
		return stampedAck(w, id, clock.Now().Add(time.Minute))
	}
	cs := &countingStore{Store: st}
	c.store = cs
	c.pollInterruptions(ctx)
	if lists, _ := cs.reads(); lists != 1 {
		t.Fatalf("a pass listed the incidents %d times, want once", lists)
	}
	c.pollInterruptions(ctx)
	if lists, trails := cs.reads(); lists != 1 || trails != 0 {
		t.Fatalf("a settled pass listed %d times and read %d trails, want 1 and 0", lists, trails)
	}
}

// An interruption the cloud announced is not a degraded node: it is left out
// of the report's GPU-hours and of the degraded gauge.
func TestInterruptionIsNotDegradation(t *testing.T) {
	to := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	got := aggregateRecovery([]*types.Incident{
		incident("i1", types.ClassCloudInterruption, "n", "", types.StateResolved, at(to, -20), at(to, -2)),
		incident("i2", types.ClassFellOffBus, "n", "", types.StateResolved, at(to, -6), at(to, -4)),
	}, map[string]int{"n": 8}, map[string]bool{"i1": true, "i2": true}, to.Add(-24*time.Hour), to)
	if got.AnnouncedInterruptions != 1 || got.Incidents != 1 || got.DegradedGPUHours != 16 || got.Recovered != 1 {
		t.Fatalf("report %+v; want the interruption counted apart, with no GPU-hours", got)
	}

	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Approval, nil), liveLimits(), 1)
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(context.Background())
	settle(t, c, st, interruptionIncident(t, st, "n1").ID)
	degraded, err := c.DegradedGPUs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for key, n := range degraded {
		if key.Class == string(types.ClassCloudInterruption) && n > 0 {
			t.Fatalf("degraded gauge counts the interruption: %v", degraded)
		}
	}
}

func openIncidentOf(t *testing.T, st *sqlite.Store, id string, class types.ProblemClass, node string) {
	t.Helper()
	now := time.Now()
	if err := st.CreateIncident(context.Background(), &types.Incident{ID: id, Class: class, Target: types.Target{Node: node},
		State: types.StateObserving, OpenedAt: now, UpdatedAt: now, StateChangedAt: now}); err != nil {
		t.Fatal(err)
	}
}

// A mass spot reclaim does not trip the breaker of a repair policy: the
// broken node is still repaired automatically.
func TestInterruptionsDoNotTripARepairBreaker(t *testing.T) {
	c, st, notifier := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	for i := 2; i <= 4; i++ {
		openIncidentOf(t, st, "spot-"+nodeName(i), types.ClassCloudInterruption, nodeName(i))
	}
	if inc := step(t, c, st, openOn(t, c, "n1"), 200); inc.State != types.StateResolved || len(notifier.approvals) != 0 {
		t.Fatalf("state=%s approvals=%v, want the fault repaired despite the announced interruptions", inc.State, notifier.approvals)
	}
}

// The breaker of an interruption's cordon counts only interruptions: faults
// elsewhere in the pool do not hold it.
func TestInterruptionCordonBreakerCountsOnlyInterruptions(t *testing.T) {
	c, st, plat, _ := interruptionFixture(t, interruptionEngine(t, recoverymode.Automatic, autoSettings()), liveLimits(), 4)
	for i := 2; i <= 4; i++ {
		openIncidentOf(t, st, "fault-"+nodeName(i), types.ClassFellOffBus, nodeName(i))
	}
	plat.announce(spotNotice("n1", time.Time{}))
	c.pollInterruptions(context.Background())
	settle(t, c, st, interruptionIncident(t, st, "n1").ID)
	if got := plat.cordoned(); len(got) != 1 || !strings.HasPrefix(got[0], "n1/") {
		t.Fatalf("cordons %v; faults on other nodes held the interruption's cordon", got)
	}
}
