package controller

// §3.3 of docs/definition-plan.md: a node under an open incident should stop
// attracting new GPU work before anything cordons it. The property these tests
// exist to hold is not "the taint gets applied" — it is that the taint can
// never outlive the incident that placed it, including when the process that
// placed it is gone.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// taintPlatform is a cluster that remembers its degraded marks.
type taintPlatform struct {
	platform.Platform
	mu      sync.Mutex
	marks   map[string]string // node -> effect
	listErr error
	applied int
	removed int
}

func newTaintPlatform() *taintPlatform {
	return &taintPlatform{marks: map[string]string{}}
}

func (p *taintPlatform) Name() string { return "taint-test" }

func (p *taintPlatform) ApplyDegradedTaint(_ context.Context, node, _, effect string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applied++
	p.marks[node] = effect
	return nil
}

func (p *taintPlatform) RemoveDegradedTaint(_ context.Context, node string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removed++
	delete(p.marks, node)
	return nil
}

func (p *taintPlatform) DegradedTaintedNodes(context.Context) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listErr != nil {
		return nil, p.listErr
	}
	out := make([]string, 0, len(p.marks))
	for node := range p.marks {
		out = append(out, node)
	}
	return out, nil
}

// mark seeds a taint the way a previous controller process would have left it.
func (p *taintPlatform) mark(node, effect string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.marks[node] = effect
}

func (p *taintPlatform) marked(node string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	effect, ok := p.marks[node]
	return effect, ok
}

func taintFixture(t *testing.T, policy DegradedTaintPolicy) (*Controller, *sqlite.Store, *taintPlatform) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	p := newTaintPlatform()
	// A live gate, not nil: the taint decision reads the LIVE execution mode,
	// and a controller with none fails closed — so a gateless fixture would
	// silently assert nothing.
	c := New(st, st, nil, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2}), nil, p, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetDegradedTaintPolicy(policy)
	return c, st, p
}

// openIncident creates one incident on a node. class is optional: the store's
// uniqueness key is (node, gpu, class), so a second incident on the same node
// needs a different one.
func openIncident(t *testing.T, st *sqlite.Store, id, node string, state types.IncidentState, class ...types.ProblemClass) *types.Incident {
	t.Helper()
	now := time.Now()
	problem := types.ClassFellOffBus
	if len(class) > 0 {
		problem = class[0]
	}
	inc := &types.Incident{
		ID: id, Target: types.Target{Node: node}, Class: problem,
		State: state, OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := st.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	return inc
}

// The whole point: the mark lands as soon as the incident starts being worked,
// long before any playbook cordons the node.
func TestIncidentLeavingOpenMarksTheNodeDegraded(t *testing.T) {
	c, st, p := taintFixture(t, DegradedTaintPolicy{Enabled: true, Effect: "PreferNoSchedule"})
	inc := openIncident(t, st, "inc-1", "n1", types.StateOpen)

	if err := c.transition(context.Background(), inc, types.StateObserving, "system", "observe", "", nil); err != nil {
		t.Fatal(err)
	}
	effect, ok := p.marked("n1")
	if !ok || effect != "PreferNoSchedule" {
		t.Fatalf("marks = %v (%v), want the node marked PreferNoSchedule at OBSERVING", effect, ok)
	}
}

// Scheduling is what every workload in the cluster depends on. A new field must
// do nothing at all until somebody asks for it.
func TestDegradedTaintIsNeverAppliedWhileDisabled(t *testing.T) {
	c, st, p := taintFixture(t, DegradedTaintPolicy{}) // the zero value is off
	inc := openIncident(t, st, "inc-1", "n1", types.StateOpen)

	if err := c.transition(context.Background(), inc, types.StateEvaluating, "system", "evaluate", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.marked("n1"); ok {
		t.Fatal("a disabled feature must not touch node scheduling")
	}
	if p.applied != 0 {
		t.Fatalf("applied = %d, want the platform never called", p.applied)
	}
}

func TestHaltingIncidentRemovesTheMark(t *testing.T) {
	c, st, p := taintFixture(t, DegradedTaintPolicy{Enabled: true, Effect: "PreferNoSchedule"})
	ctx := context.Background()
	inc := openIncident(t, st, "inc-1", "n1", types.StateOpen)
	if err := c.transition(ctx, inc, types.StateObserving, "system", "observe", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.transition(ctx, inc, types.StateResolved, "system", "resolve", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.marked("n1"); ok {
		t.Fatal("the mark's whole lifetime is one open incident")
	}
	if p.removed != 1 {
		t.Fatalf("removals = %d, want exactly one on the halting transition", p.removed)
	}
}

// Unlike the cordon, which a human-owned incident keeps: nothing would ever
// come back to clear a taint left behind by an incident parked for a person.
func TestNeedsHumanAlsoRemovesTheMark(t *testing.T) {
	c, st, p := taintFixture(t, DegradedTaintPolicy{Enabled: true, Effect: "NoSchedule"})
	ctx := context.Background()
	inc := openIncident(t, st, "inc-1", "n1", types.StateOpen)
	if err := c.transition(ctx, inc, types.StateEvaluating, "system", "evaluate", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.transition(ctx, inc, types.StateNeedsHuman, "system", "quarantine", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.marked("n1"); ok {
		t.Fatal("a mark nothing will ever come back for must not survive its incident")
	}
}

// A bad node commonly carries several incidents. One resolving must not flicker
// the node in and out of the scheduler's preference.
func TestMarkSurvivesWhileAnotherIncidentStillHoldsTheNode(t *testing.T) {
	c, st, p := taintFixture(t, DegradedTaintPolicy{Enabled: true, Effect: "PreferNoSchedule"})
	ctx := context.Background()
	first := openIncident(t, st, "inc-1", "n1", types.StateOpen)
	openIncident(t, st, "inc-2", "n1", types.StateEvaluating, types.ClassECCDBE)
	if err := c.transition(ctx, first, types.StateObserving, "system", "observe", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.transition(ctx, first, types.StateResolved, "system", "resolve", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.marked("n1"); !ok {
		t.Fatal("the node still has an open incident; its mark must stay")
	}
}

// The leaked-taint case this feature could not ship without: the controller
// died between marking a node and closing its incident, so nothing in memory
// knows the mark exists. Recovery must come from the cluster.
func TestJanitorClearsAMarkLeftByADeadController(t *testing.T) {
	c, st, p := taintFixture(t, DegradedTaintPolicy{Enabled: true, Effect: "PreferNoSchedule"})
	ctx := context.Background()
	p.mark("n1", "PreferNoSchedule") // placed by a process that is gone
	inc := openIncident(t, st, "inc-1", "n1", types.StateEvaluating)
	inc.State = types.StateResolved
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	c.reconcileDegradedTaints(ctx)
	if _, ok := p.marked("n1"); ok {
		t.Fatal("a mark whose incident has closed must be cleared by the janitor")
	}
}

// The incident row is gone entirely (retention, a manual delete): nothing will
// ever reference this mark again.
func TestJanitorClearsAMarkWithNoIncidentAtAll(t *testing.T) {
	c, _, p := taintFixture(t, DegradedTaintPolicy{Enabled: true, Effect: "PreferNoSchedule"})
	p.mark("orphan", "PreferNoSchedule")

	c.reconcileDegradedTaints(context.Background())
	if _, ok := p.marked("orphan"); ok {
		t.Fatal("an orphaned mark must not survive a janitor pass")
	}
}

// Turning the switch back off has to take the marks with it, or the fleet stays
// biased against those nodes with no configuration left that explains why.
func TestJanitorClearsEveryMarkWhenTheFeatureIsDisabled(t *testing.T) {
	c, st, p := taintFixture(t, DegradedTaintPolicy{})
	p.mark("n1", "PreferNoSchedule")
	openIncident(t, st, "inc-1", "n1", types.StateEvaluating) // still open, and irrelevant

	c.reconcileDegradedTaints(context.Background())
	if _, ok := p.marked("n1"); ok {
		t.Fatal("a disabled feature must clean up after itself, not freeze its marks in place")
	}
}

// Enabling the field with incidents already open, or a transition-time apply
// that lost to an apiserver blip: the janitor converges the other way too.
func TestJanitorMarksANodeWhoseIncidentWasAlreadyOpen(t *testing.T) {
	c, st, p := taintFixture(t, DegradedTaintPolicy{Enabled: true, Effect: "NoSchedule"})
	openIncident(t, st, "inc-1", "n1", types.StateAwaitingApproval)

	c.reconcileDegradedTaints(context.Background())
	effect, ok := p.marked("n1")
	if !ok || effect != "NoSchedule" {
		t.Fatalf("marks = %v (%v), want the already-open incident's node marked", effect, ok)
	}
}

// Removing a mark because the store is unreadable would put a failing node back
// into the scheduler's rotation exactly when nothing can see what is wrong.
func TestJanitorKeepsMarksWhenTheStoreIsUnreadable(t *testing.T) {
	c, _, p := taintFixture(t, DegradedTaintPolicy{Enabled: true, Effect: "PreferNoSchedule"})
	p.mark("n1", "PreferNoSchedule")
	c.store = failingListStore{Store: c.store, err: errors.New("database unavailable")}

	c.reconcileDegradedTaints(context.Background())
	if _, ok := p.marked("n1"); !ok {
		t.Fatal("a store outage is not evidence that an incident closed")
	}
}

type failingListStore struct {
	store.Store
	err error
}

func (s failingListStore) ListIncidents(context.Context, store.IncidentFilter) ([]*types.Incident, error) {
	return nil, s.err
}

// A platform with no scheduler to talk to must be inert, not a crash.
func TestControllerWithoutATaintingPlatformIsInert(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// A live gate, not nil: the taint decision reads the LIVE execution mode,
	// and a controller with none fails closed — so a gateless fixture would
	// silently assert nothing.
	c := New(st, st, nil, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2}), nil, nil, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetDegradedTaintPolicy(DegradedTaintPolicy{Enabled: true, Effect: "PreferNoSchedule"})
	ctx := context.Background()
	inc := openIncident(t, st, "inc-1", "n1", types.StateOpen)

	c.reconcileDegradedTaints(ctx)
	if err := c.transition(ctx, inc, types.StateObserving, "system", "observe", "", nil); err != nil {
		t.Fatal(err)
	}
}

// An unrecognised effect must fail the whole configuration install, so the
// reload path keeps the previous one rather than marking nodes with something
// nobody asked for.
func TestUnknownTaintEffectIsRejected(t *testing.T) {
	c, _, _ := taintFixture(t, DegradedTaintPolicy{})
	if err := c.InstallRuntimeConfig(RuntimeConfig{
		DegradedTaint: DegradedTaintPolicy{Enabled: true, Effect: "NoExecute"},
	}); err == nil {
		t.Fatal("NoExecute would evict running pods; it must not be installable here")
	}
	if err := c.InstallRuntimeConfig(RuntimeConfig{
		DegradedTaint: DegradedTaintPolicy{Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if got := c.degradedTaintPolicy(context.Background()).Effect; got != "PreferNoSchedule" {
		t.Fatalf("effect = %q, want the weak default", got)
	}
}

// The value is operator-facing text on a node object, and the API's alphabet is
// narrow: a class name with an unexpected character must not make every write
// fail validation.
func TestDegradedTaintValueIsAlwaysAcceptable(t *testing.T) {
	for _, tc := range []struct{ class, want string }{
		{"fell-off-bus", "fell-off-bus"},
		{"ecc/dbe", "ecc-dbe"},
		{"", "incident"},
		{"-leading", "leading"},
	} {
		got := degradedTaintValue(&types.Incident{Class: types.ProblemClass(tc.class)})
		if got != tc.want {
			t.Fatalf("degradedTaintValue(%q) = %q, want %q", tc.class, got, tc.want)
		}
	}
}

// TestDryRunNeverWritesATaint holds the promise dry-run makes. A NoSchedule
// mark is a cluster-wide scheduling change affecting every workload, not only
// the ones KubeNeuron would remediate, so an installation told to change
// nothing must not write one.
func TestDryRunNeverWritesATaint(t *testing.T) {
	c, st, p := taintFixture(t, DegradedTaintPolicy{Enabled: true, Effect: "NoSchedule"})
	inc := openIncident(t, st, "inc-dry", "n1", types.StateOpen)
	inc.DryRun = true
	if err := st.UpdateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}

	if err := c.transition(context.Background(), inc, types.StateObserving, "system", "observe", "", nil); err != nil {
		t.Fatal(err)
	}
	if effect, ok := p.marked("n1"); ok {
		t.Fatalf("dry-run wrote a real %s taint on n1; dry-run promises it changes nothing", effect)
	}

	// The janitor must not put it back either.
	c.reconcileDegradedTaints(context.Background())
	if _, ok := p.marked("n1"); ok {
		t.Fatal("the janitor marked a node whose only incident is dry-run")
	}
}

// TestUnparkedIncidentIsMarkedAgain covers the transition halting removed the
// mark on: NEEDS_HUMAN -> EVALUATING is legal, and an incident somebody sent
// back for another attempt is being actively worked. Leaving it unmarked
// re-opens the exact window the feature closes, at the worst moment.
func TestUnparkedIncidentIsMarkedAgain(t *testing.T) {
	c, st, p := taintFixture(t, DegradedTaintPolicy{Enabled: true, Effect: "PreferNoSchedule"})
	ctx := context.Background()
	inc := openIncident(t, st, "inc-park", "n1", types.StateOpen)

	if err := c.transition(ctx, inc, types.StateEvaluating, "system", "evaluate", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.marked("n1"); !ok {
		t.Fatal("precondition: the node should be marked while the incident is worked")
	}
	if err := c.transition(ctx, inc, types.StateNeedsHuman, "system", "park", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.marked("n1"); ok {
		t.Fatal("precondition: halting removes the mark")
	}

	if err := c.transition(ctx, inc, types.StateEvaluating, "operator", "unpark", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.marked("n1"); !ok {
		t.Fatal("an unparked incident left the node unmarked; the scheduler is free to pile new work " +
			"onto failing hardware exactly while somebody is working the incident")
	}
}

// staleListStore serves the janitor's FLEET-WIDE incident list from a snapshot
// taken before an incident opened, while per-node reads see the truth. That is
// exactly the shape of the janitor's two-read race: the marked-node list and
// the incident list are taken at different moments, and anything that opens in
// the gap is invisible to the second one.
type staleListStore struct {
	store.Store
}

func (s staleListStore) ListIncidents(ctx context.Context, f store.IncidentFilter) ([]*types.Incident, error) {
	if f.Node == "" {
		return nil, nil // the stale fleet-wide snapshot
	}
	return s.Store.ListIncidents(ctx, f)
}

// TestJanitorConfirmsBeforeRemovingAMark covers the two-read race. Removing a
// mark on a stale read returns a node to the scheduler's rotation while a
// fault is actively being worked on it — the one direction of this janitor's
// error that costs anybody anything.
func TestJanitorConfirmsBeforeRemovingAMark(t *testing.T) {
	c, st, p := taintFixture(t, DegradedTaintPolicy{Enabled: true, Effect: "NoSchedule"})
	ctx := context.Background()
	p.mark("n1", "NoSchedule")
	openIncident(t, st, "inc-late", "n1", types.StateEvaluating)
	c.store = staleListStore{Store: st}

	c.reconcileDegradedTaints(ctx)
	if _, ok := p.marked("n1"); !ok {
		t.Fatal("the janitor removed the mark from a node under an open incident, " +
			"trusting a fleet-wide list taken before that incident opened")
	}
}

// The confirming read must not resurrect the case the janitor exists for: a
// mark with genuinely nothing behind it still comes off.
func TestJanitorStillClearsATrulyAbandonedMark(t *testing.T) {
	c, _, p := taintFixture(t, DegradedTaintPolicy{Enabled: true, Effect: "NoSchedule"})
	p.mark("ghost", "NoSchedule")

	c.reconcileDegradedTaints(context.Background())
	if _, ok := p.marked("ghost"); ok {
		t.Fatal("a mark with no incident behind it survived the janitor")
	}
}
