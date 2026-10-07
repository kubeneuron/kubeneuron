package controller

// The checkpoint coordination pre-phase (checkpoint.go), driven through the
// real executePlatformStep / executeStep paths against a fake platform that
// records every call in order. The ordering assertions are the point of most
// of these tests: the disruption must come AFTER coordination, and nothing
// coordination does may ever stop the disruption from coming.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// basePlatform satisfies platform.Platform and NOTHING optional: it stands in
// for bare metal, which does not implement WorkloadCheckpointer.
type basePlatform struct {
	mu        sync.Mutex
	calls     []string
	workloads []platform.Workload
	listErr   error
	// seen, when set, is handed the context of every platform call as it is
	// made, so a test can check which deadline the controller ran it under.
	seen func(ctx context.Context, call string)
	// blockOn names the calls ("list", "request", "observe") that block until
	// their context is done and then fail with its error: a platform that
	// hangs, which is what the coordination budget has to survive.
	blockOn map[string]bool
}

func (p *basePlatform) record(ctx context.Context, call string) {
	p.mu.Lock()
	p.calls = append(p.calls, call)
	seen := p.seen
	p.mu.Unlock()
	if seen != nil {
		seen(ctx, call)
	}
}

// blocked waits for ctx to end when kind is in blockOn, and reports whether
// it did.
func (p *basePlatform) blocked(ctx context.Context, kind string) bool {
	if !p.blockOn[kind] {
		return false
	}
	<-ctx.Done()
	return true
}

func (p *basePlatform) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *basePlatform) Name() string                                                 { return "base-test" }
func (p *basePlatform) ListNodes(context.Context) ([]types.Node, error)              { return nil, nil }
func (p *basePlatform) Cordon(context.Context, string, string) error                 { return nil }
func (p *basePlatform) Uncordon(context.Context, string) error                       { return nil }
func (p *basePlatform) CordonForOwner(context.Context, string, string, string) error { return nil }
func (p *basePlatform) ReleaseCordonOwners(context.Context, string, []string) (bool, int, error) {
	return true, 0, nil
}
func (p *basePlatform) WatchNodes(ctx context.Context) (<-chan platform.NodeEvent, error) {
	ch := make(chan platform.NodeEvent)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}
func (p *basePlatform) Drain(ctx context.Context, node string, _ platform.DrainOptions) error {
	p.record(ctx, "drain "+node)
	return nil
}
func (p *basePlatform) NodeWorkloads(ctx context.Context, node string) ([]platform.Workload, error) {
	p.record(ctx, "list "+node)
	if p.blocked(ctx, "list") {
		return nil, ctx.Err()
	}
	if p.listErr != nil {
		return nil, p.listErr
	}
	return append([]platform.Workload(nil), p.workloads...), nil
}
func (p *basePlatform) EvictWorkload(ctx context.Context, w platform.Workload) error {
	p.record(ctx, "evict "+w.Namespace+"/"+w.Name)
	return nil
}

// checkpointPlatform adds the optional capability. request and observe are
// per-test scripts; the fake records what the controller asked for.
type checkpointPlatform struct {
	basePlatform
	request func(w platform.Workload, req checkpoint.Request, attempt int) (checkpoint.Request, error)
	observe func(w platform.Workload, pass int) checkpoint.Observation

	requests map[string]int
	observes map[string]int
}

func (p *checkpointPlatform) Name() string { return "checkpoint-test" }

func (p *checkpointPlatform) RequestCheckpoint(ctx context.Context, _ checkpoint.Policy, node string, w platform.Workload, req checkpoint.Request) (checkpoint.Request, error) {
	p.mu.Lock()
	if p.requests == nil {
		p.requests = map[string]int{}
	}
	p.requests[w.Name]++
	attempt := p.requests[w.Name]
	p.mu.Unlock()
	p.record(ctx, fmt.Sprintf("request %s/%s@%s", w.Namespace, w.Name, node))
	if p.blocked(ctx, "request") {
		return checkpoint.Request{}, fmt.Errorf("checkpoint: stamping %s/%s: %w", w.Namespace, w.Name, ctx.Err())
	}
	if p.request == nil {
		return req, nil
	}
	return p.request(w, req, attempt)
}

func (p *checkpointPlatform) ObserveCheckpoint(ctx context.Context, _ checkpoint.Policy, _ string, w platform.Workload) checkpoint.Observation {
	p.mu.Lock()
	if p.observes == nil {
		p.observes = map[string]int{}
	}
	p.observes[w.Name]++
	pass := p.observes[w.Name]
	p.mu.Unlock()
	p.record(ctx, "observe "+w.Namespace+"/"+w.Name)
	if p.blocked(ctx, "observe") {
		return checkpoint.Observation{Err: fmt.Errorf("checkpoint: reading %s/%s: %w", w.Namespace, w.Name, ctx.Err())}
	}
	if p.observe == nil {
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
	}
	return p.observe(w, pass)
}

// fakeClock is the coordinator's clock: it advances only when a test says so,
// so a five-minute window is crossed by a call rather than a sleep.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// fixtureIncidentID is the ID protectionFixture gives the incident, which is
// what an explicit acknowledgement in these tests must be bound to.
const fixtureIncidentID = "inc-prot"

func optedIn(name string, gpu bool) platform.Workload {
	return platform.Workload{
		Name: name, Namespace: "training", Kind: "Pod", UsesGPU: gpu, UID: "uid-" + name,
		Annotations: map[string]string{checkpoint.AnnotationOptIn: checkpoint.OptInValue},
	}
}

// stampedAck observes w alive and carrying the in-force stamp of incidentID,
// as the live object does once RequestCheckpoint has written it, with the
// workload's bound acknowledgement of that incident on top.
func stampedAck(w platform.Workload, incidentID string, deadline time.Time) checkpoint.Observation {
	ann := checkpoint.Request{IncidentID: incidentID, RequestedAt: deadline.Add(-time.Minute), DeadlineAt: deadline}.Annotations()
	ann[checkpoint.AnnotationState] = checkpoint.StateCompleteFor(incidentID)
	return checkpoint.Observation{Found: true, UID: w.UID, Annotations: ann}
}

// refuseLikeAdapter scripts the fake's RequestCheckpoint the way the Kubernetes
// adapter behaves on a live foreign request: the error names the workload only
// and the parsed LIVE request comes back alongside it. live is the annotation
// set the adapter reads on the object; nil means the listing is still current.
func refuseLikeAdapter(live map[string]string) func(platform.Workload, checkpoint.Request, int) (checkpoint.Request, error) {
	return func(w platform.Workload, _ checkpoint.Request, _ int) (checkpoint.Request, error) {
		if live == nil {
			live = w.Annotations
		}
		existing, _ := checkpoint.ParseRequest(live)
		return existing, fmt.Errorf("%w: %s/%s carries a live request from another incident",
			platform.ErrCheckpointForeignRequest, w.Namespace, w.Name)
	}
}

func enabledPolicy() checkpoint.Policy {
	return checkpoint.Policy{
		Enabled: true, DefaultWait: 5 * time.Minute, MaxWait: 15 * time.Minute,
		SkipClasses: checkpoint.DefaultSkipClasses(), Namespaces: []string{"training"},
	}
}

// checkpointFixture wires a controller around the two-rung disruptive
// playbook with the given platform and policy. The incident's class is one
// the default policy coordinates for, and the clock is the fake one.
func checkpointFixture(t *testing.T, plat platform.Platform, policy checkpoint.Policy) (*Controller, *types.Incident, *fakeClock) {
	t.Helper()
	c, st, inc := protectionFixture(t, disruptivePlaybook(), nil, plat, nil)
	inc.Class = types.ClassECCDBE
	if err := st.UpdateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.Checkpoint = policy })
	clock := &fakeClock{now: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)}
	c.checkpointNow = clock.Now
	c.checkpointPoll = time.Millisecond
	return c, inc, clock
}

var checkpointOutcomes = []checkpoint.Outcome{
	checkpoint.OutcomeAcknowledged, checkpoint.OutcomeExited, checkpoint.OutcomeExpired,
	checkpoint.OutcomeUnreachable, checkpoint.OutcomeSkipped,
}

type checkpointMetrics struct {
	requests  map[checkpoint.Outcome]float64
	waitCount uint64
	deferrals map[string]float64
}

func snapshotCheckpointMetrics(t *testing.T) checkpointMetrics {
	t.Helper()
	m := checkpointMetrics{requests: map[checkpoint.Outcome]float64{}, deferrals: snapshotDeferrals()}
	for _, o := range checkpointOutcomes {
		m.requests[o] = testutil.ToFloat64(metrics.CheckpointRequests.WithLabelValues(string(o)))
	}
	m.waitCount = waitHistogramCount(t)
	return m
}

// waitHistogramCount reads the sample count of the wait histogram from the
// default registry.
func waitHistogramCount(t *testing.T) uint64 {
	t.Helper()
	count, _ := waitHistogram(t)
	return count
}

// waitHistogram reads the sample count and sum of the wait histogram from the
// default registry. Tests diff two readings, so the samples other tests left
// behind do not matter.
func waitHistogram(t *testing.T) (count uint64, sum float64) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		if mf.GetName() == "kubeneuron_checkpoint_wait_seconds" {
			h := mf.GetMetric()[0].GetHistogram()
			return h.GetSampleCount(), h.GetSampleSum()
		}
	}
	return 0, 0
}

// assertWaitObserved requires exactly one new wait sample since the given
// reading, worth exactly want seconds.
func assertWaitObserved(t *testing.T, beforeCount uint64, beforeSum float64, want time.Duration) {
	t.Helper()
	count, sum := waitHistogram(t)
	if count-beforeCount != 1 {
		t.Fatalf("checkpoint_wait_seconds samples = %d, want 1", count-beforeCount)
	}
	if got := sum - beforeSum; got != want.Seconds() {
		t.Fatalf("checkpoint_wait_seconds observed %vs, want %vs", got, want.Seconds())
	}
	if sum < beforeSum {
		t.Fatalf("checkpoint_wait_seconds sum went down (%v -> %v): a negative observation", beforeSum, sum)
	}
}

// assertCheckpointMetrics requires exactly the given request deltas, exactly
// the given number of new wait samples, and — unless waited — no deferral at
// all. A wait that happened must show up under checkpoint_wait and nowhere else.
func assertCheckpointMetrics(t *testing.T, before checkpointMetrics, want map[checkpoint.Outcome]float64, waitSamples uint64, waited bool) {
	t.Helper()
	after := snapshotCheckpointMetrics(t)
	for _, o := range checkpointOutcomes {
		if got := after.requests[o] - before.requests[o]; got != want[o] {
			t.Fatalf("checkpoint_requests_total{outcome=%s} moved by %v, want %v", o, got, want[o])
		}
	}
	if got := after.waitCount - before.waitCount; got != waitSamples {
		t.Fatalf("checkpoint_wait_seconds samples = %d, want %d", got, waitSamples)
	}
	if waited {
		assertDeferred(t, before.deferrals, metrics.DeferCheckpointWait)
	} else {
		assertNoDeferrals(t, before.deferrals)
	}
}

func auditResults(t *testing.T, c *Controller, inc *types.Incident) []string {
	t.Helper()
	trail, err := c.store.AuditTrail(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range trail {
		out = append(out, e.Result)
	}
	return out
}

func requireAudit(t *testing.T, results []string, substr string) {
	t.Helper()
	for _, r := range results {
		if strings.Contains(r, substr) {
			return
		}
	}
	t.Fatalf("no audit row contains %q; trail:\n  %s", substr, strings.Join(results, "\n  "))
}

func drainStep() *playbook.Step { return &playbook.Step{Name: "drain", Action: "platform.drain"} }
func evictStep() *playbook.Step {
	return &playbook.Step{Name: "evict", Action: "platform.evict_gpu_workload"}
}

func runDrain(t *testing.T, c *Controller, inc *types.Incident) {
	t.Helper()
	if _, err := c.executePlatformStep(context.Background(), inc, "drain", drainStep()); err != nil {
		t.Fatalf("drain must never fail because of coordination: %v", err)
	}
}

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("platform calls:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// The default policy is disabled, and a disabled policy must be an EXACT
// no-op: no listing, no request, no audit row, no metric. An installation that
// never asked for the feature runs the code it ran before.
func TestCheckpointDisabledPolicyIsAnExactNoOp(t *testing.T) {
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	c, inc, _ := checkpointFixture(t, plat, checkpoint.Policy{})
	before := snapshotCheckpointMetrics(t)

	runDrain(t, c, inc)

	assertCalls(t, plat.Calls(), []string{"drain n1"})
	assertCheckpointMetrics(t, before, nil, 0, false)
	if got := auditResults(t, c, inc); len(got) != 0 {
		t.Fatalf("a disabled policy wrote audit rows: %v", got)
	}
}

// A class in skipClasses has no running job left to warn: no listing, one
// skipped count, one audit row naming the class, and the drain proceeds.
func TestCheckpointSkipClassSkipsWithoutListing(t *testing.T) {
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
	inc.Class = types.ClassFellOffBus // in DefaultSkipClasses
	before := snapshotCheckpointMetrics(t)

	runDrain(t, c, inc)

	assertCalls(t, plat.Calls(), []string{"drain n1"})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeSkipped: 1}, 0, false)
	requireAudit(t, auditResults(t, c, inc), "checkpoint: skipped (class-skipped: "+string(types.ClassFellOffBus))
}

// Bare metal does not implement the optional capability. That is a skip, not
// a failure, and it is audited so the trail says why nobody was asked.
func TestCheckpointUnsupportedPlatformSkips(t *testing.T) {
	plat := &basePlatform{workloads: []platform.Workload{optedIn("trainer", true)}}
	c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
	before := snapshotCheckpointMetrics(t)

	runDrain(t, c, inc)

	assertCalls(t, plat.Calls(), []string{"drain n1"})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeSkipped: 1}, 0, false)
	requireAudit(t, auditResults(t, c, inc), "does not support checkpoint coordination")
}

// The happy path, and the ordering that makes the feature mean anything: the
// request goes out, the controller genuinely waits, the workload acknowledges,
// and ONLY THEN is the node drained.
func TestCheckpointAcknowledgementPrecedesDrain(t *testing.T) {
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true), {
		// Not opted in: never asked, but still drained with the node.
		Name: "sidecar", Namespace: "training", Kind: "Pod", UID: "uid-sidecar",
	}}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		if pass >= 2 {
			return stampedAck(w, fixtureIncidentID, clock.Now().Add(5*time.Minute))
		}
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: map[string]string{}}
	}
	before := snapshotCheckpointMetrics(t)

	runDrain(t, c, inc)

	assertCalls(t, plat.Calls(), []string{
		"list n1", "request training/trainer@n1",
		"observe training/trainer", "observe training/trainer",
		"drain n1",
	})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeAcknowledged: 1}, 1, true)
	trail := auditResults(t, c, inc)
	requireAudit(t, trail, "checkpoint: requested 1 workload(s) training/trainer; deadline 2026-09-29T10:05:00Z (wait 5m0s) before platform.drain")
	requireAudit(t, trail, "training/trainer=acknowledged; proceeding with platform.drain")
	for _, r := range trail {
		if strings.Contains(r, checkpoint.AnnotationOptIn) {
			t.Fatalf("audit row carries an annotation payload: %q", r)
		}
	}
}

// evict_gpu_workload only ever evicts GPU holders, so it only ever ASKS GPU
// holders: an opted-in CPU pod on the same node is neither requested nor
// evicted.
func TestCheckpointEvictOnlyRequestsGPUWorkloads(t *testing.T) {
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true), optedIn("logger", false)}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		if pass >= 2 {
			return stampedAck(w, fixtureIncidentID, clock.Now().Add(5*time.Minute))
		}
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: map[string]string{}}
	}
	before := snapshotCheckpointMetrics(t)

	if _, err := c.executePlatformStep(context.Background(), inc, "evict_gpu_workload", evictStep()); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, plat.Calls(), []string{
		"list n1", "request training/trainer@n1",
		"observe training/trainer", "observe training/trainer",
		// The eviction itself re-lists, as it always did, and evicts only the
		// GPU holder — after the acknowledgement, never before.
		"list n1", "evict training/trainer",
	})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeAcknowledged: 1}, 1, true)
}

// The first observation happens before any sleep. A workload that has already
// exited when asked settles on that look, so no wait, and no deferral, is
// recorded — while the eviction still happens afterwards.
func TestCheckpointExitedWorkloadSettlesWithoutWaiting(t *testing.T) {
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	plat.observe = func(platform.Workload, int) checkpoint.Observation {
		return checkpoint.Observation{Found: false}
	}
	c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
	before := snapshotCheckpointMetrics(t)

	if _, err := c.executePlatformStep(context.Background(), inc, "evict_gpu_workload", evictStep()); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, plat.Calls(), []string{
		"list n1", "request training/trainer@n1", "observe training/trainer",
		"list n1", "evict training/trainer",
	})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExited: 1}, 1, false)
}

// A request that fails outright is unreachable: no wait, no error, no
// escalation. The drain proceeds within its own budget.
func TestCheckpointRequestErrorNeverBlocksOrFailsTheDrain(t *testing.T) {
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	plat.request = func(platform.Workload, checkpoint.Request, int) (checkpoint.Request, error) {
		return checkpoint.Request{}, errors.New("apiserver: 503")
	}
	c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
	before := snapshotCheckpointMetrics(t)

	runDrain(t, c, inc)

	assertCalls(t, plat.Calls(), []string{"list n1", "request training/trainer@n1", "drain n1"})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeUnreachable: 1}, 0, false)
	requireAudit(t, auditResults(t, c, inc), "no request in force for training/trainer=unreachable")
}

// A guarded-patch conflict is retried exactly once against the adapter's fresh
// live read. A second conflict is unreachable, never a third attempt.
func TestCheckpointConflictRetriesOnceOnly(t *testing.T) {
	t.Run("second attempt lands", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = []platform.Workload{optedIn("trainer", true)}
		plat.request = func(_ platform.Workload, req checkpoint.Request, attempt int) (checkpoint.Request, error) {
			if attempt == 1 {
				return checkpoint.Request{}, fmt.Errorf("%w: resourceVersion moved", platform.ErrCheckpointConflict)
			}
			return req, nil
		}
		plat.observe = func(platform.Workload, int) checkpoint.Observation {
			return checkpoint.Observation{Found: true, Terminal: true, UID: "uid-trainer"}
		}
		c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
		runDrain(t, c, inc)
		assertCalls(t, plat.Calls(), []string{
			"list n1", "request training/trainer@n1", "request training/trainer@n1",
			"observe training/trainer", "drain n1",
		})
	})
	t.Run("second conflict is unreachable", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = []platform.Workload{optedIn("trainer", true)}
		plat.request = func(platform.Workload, checkpoint.Request, int) (checkpoint.Request, error) {
			return checkpoint.Request{}, fmt.Errorf("%w: still moving", platform.ErrCheckpointConflict)
		}
		c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
		before := snapshotCheckpointMetrics(t)
		runDrain(t, c, inc)
		assertCalls(t, plat.Calls(), []string{
			"list n1", "request training/trainer@n1", "request training/trainer@n1", "drain n1",
		})
		assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeUnreachable: 1}, 0, false)
	})
}

// A workload gone or replaced between the listing and the request has, by the
// zero-RBAC path, already exited. Nothing is waited for.
func TestCheckpointGoneOrReplacedWorkloadCountsAsExited(t *testing.T) {
	for name, err := range map[string]error{
		"gone":     platform.ErrCheckpointWorkloadGone,
		"replaced": platform.ErrCheckpointScope,
	} {
		t.Run(name, func(t *testing.T) {
			plat := &checkpointPlatform{}
			plat.workloads = []platform.Workload{optedIn("trainer", true)}
			plat.request = func(platform.Workload, checkpoint.Request, int) (checkpoint.Request, error) {
				return checkpoint.Request{}, fmt.Errorf("%w: training/trainer", err)
			}
			c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
			before := snapshotCheckpointMetrics(t)
			runDrain(t, c, inc)
			assertCalls(t, plat.Calls(), []string{"list n1", "request training/trainer@n1", "drain n1"})
			assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExited: 1}, 0, false)
		})
	}
}

// Reads that fail during the wait extend nothing: the deadline is the same
// bounded one, and at the deadline the failure is the workload's final answer.
// The drain still happens and the step still succeeds.
func TestCheckpointObserveErrorKeepsTheBoundedWaitAndProceeds(t *testing.T) {
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	plat.observe = func(_ platform.Workload, pass int) checkpoint.Observation {
		if pass == 1 {
			// Seen by the NEXT pass: the whole DefaultWait window passes.
			clock.Advance(5 * time.Minute)
		}
		return checkpoint.Observation{Err: errors.New("apiserver: connection reset")}
	}
	before := snapshotCheckpointMetrics(t)

	runDrain(t, c, inc)

	calls := plat.Calls()
	if calls[len(calls)-1] != "drain n1" {
		t.Fatalf("drain must follow coordination; calls: %v", calls)
	}
	observes := 0
	for _, call := range calls {
		if strings.HasPrefix(call, "observe") {
			observes++
		}
	}
	// Pass 1 (unreadable, before the deadline: pending), a sleep, pass 2
	// (past the deadline; unreadable at the deadline: unreachable).
	if observes != 2 {
		t.Fatalf("observed %d times, want exactly 2: a read error must neither settle early nor keep polling past the deadline", observes)
	}
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeUnreachable: 1}, 1, true)
}

// The deadline is the operator's. A workload that never answers is expired at
// it, the wait is recorded, and the drain proceeds — never an error, never an
// escalation.
func TestCheckpointExpiryProceedsWithoutError(t *testing.T) {
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		if pass == 2 {
			clock.Advance(10 * time.Minute) // seen by pass 3
		}
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
	}
	before := snapshotCheckpointMetrics(t)

	runDrain(t, c, inc)

	assertCalls(t, plat.Calls(), []string{
		"list n1", "request training/trainer@n1",
		"observe training/trainer", "observe training/trainer", "observe training/trainer",
		"drain n1",
	})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 1}, 1, true)
	requireAudit(t, auditResults(t, c, inc), "training/trainer=expired; proceeding with platform.drain")
}

// The wait never exceeds the step's remaining budget minus the reserve kept
// for the disruption itself, and when nothing is left after the reserve no
// request goes out at all.
func TestCheckpointWaitIsBoundedByStepBudgetAndReserve(t *testing.T) {
	t.Run("budget shorter than the policy wait bounds the deadline", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = []platform.Workload{optedIn("trainer", true)}
		var stamped checkpoint.Request
		plat.request = func(_ platform.Workload, req checkpoint.Request, _ int) (checkpoint.Request, error) {
			stamped = req
			return req, nil
		}
		plat.observe = func(platform.Workload, int) checkpoint.Observation {
			return checkpoint.Observation{Found: false}
		}
		c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
		step := drainStep()
		step.Timeout = playbook.Duration(3 * time.Minute) // 3m - 90s reserve = 90s of wait
		if _, err := c.executePlatformStep(context.Background(), inc, "drain", step); err != nil {
			t.Fatal(err)
		}
		if want := clock.Now().Add(90 * time.Second); !stamped.DeadlineAt.Equal(want) {
			t.Fatalf("stamped deadline %v, want %v (step budget minus the reserve, not the 5m policy default)", stamped.DeadlineAt, want)
		}
	})
	t.Run("budget inside the reserve asks nobody and its audit row borrows no step time", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = []platform.Workload{optedIn("trainer", true)}
		c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
		audits := &auditContextStore{Store: c.store}
		c.store = audits
		before := snapshotCheckpointMetrics(t)
		step := drainStep()
		step.Timeout = playbook.Duration(60 * time.Second)
		// The step's own context is live, with a deadline well in the future:
		// exactly the time the no-budget audit row must not be able to spend.
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		if _, err := c.executePlatformStep(ctx, inc, "drain", step); err != nil {
			t.Fatal(err)
		}
		assertCalls(t, plat.Calls(), []string{"drain n1"})
		assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeSkipped: 1}, 0, false)
		const skippedRow = "step budget 1m0s leaves nothing to wait with after the 1m30s reserve"
		outerDeadline, _ := ctx.Deadline()
		var seen bool
		for _, a := range audits.appends() {
			if !strings.Contains(a.result, skippedRow) {
				continue
			}
			seen = true
			if a.err == nil {
				t.Fatalf("no-budget skipped row was attempted under a live context (deadline %v, ok=%v); it must be attempted under an already-cancelled child of the step context so it can never wait on the store", a.deadline, a.hasDeadline)
			}
			if !a.hasDeadline || !a.deadline.Equal(outerDeadline) {
				t.Fatalf("no-budget skipped row context has deadline %v (ok=%v), want a child of the step context (deadline %v), not a detached one", a.deadline, a.hasDeadline, outerDeadline)
			}
		}
		if !seen {
			t.Fatal("no-budget skipped row was never attempted; the best-effort audit must still be tried")
		}
		// The row itself is best effort: a cancelled context fails the append
		// fast, so it is logged, not persisted, and the disruption above still
		// ran. Its absence is the expected shape of this trail.
		requireNoAudit(t, auditResults(t, c, inc), skippedRow)
	})
	t.Run("the executeStep context's grace is not spent on waiting", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = []platform.Workload{optedIn("trainer", true)}
		var stamped checkpoint.Request
		plat.request = func(_ platform.Workload, req checkpoint.Request, _ int) (checkpoint.Request, error) {
			stamped = req
			return req, nil
		}
		plat.observe = func(platform.Workload, int) checkpoint.Observation {
			return checkpoint.Observation{Found: false}
		}
		c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
		step := drainStep()
		step.Timeout = playbook.Duration(2 * time.Minute)
		// executeStep's own context: timeout plus AgentResultGrace.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute+agentResultGrace)
		defer cancel()
		if _, err := c.executePlatformStep(ctx, inc, "drain", step); err != nil {
			t.Fatal(err)
		}
		// 2m intended budget minus 90s reserve = 30s; the 15s grace on the
		// context must not have become 45s.
		got := stamped.DeadlineAt.Sub(clock.Now())
		if got > 30*time.Second || got < 25*time.Second {
			t.Fatalf("stamped wait %v, want about 30s: the agent-result grace must not be spent on coordination", got)
		}
	})
}

// Each workload is granted ITS OWN deadline: its checkpoint-max-wait request,
// or the policy default when it made none, bounded by the step budget. A
// workload that asked for 30s next to one granted the 5m default gets 30s and
// nothing longer; the step waits until the latest of the two and only then
// drains. A request can only shorten that workload's window, never a
// neighbour's, and a neighbour's window never lengthens it.
func TestCheckpointDeadlinesArePerWorkload(t *testing.T) {
	// requestsByName captures what the controller actually stamped, per
	// workload, on the fake platform.
	capture := func(plat *checkpointPlatform) map[string]checkpoint.Request {
		stamped := map[string]checkpoint.Request{}
		var mu sync.Mutex
		plat.request = func(w platform.Workload, req checkpoint.Request, _ int) (checkpoint.Request, error) {
			mu.Lock()
			defer mu.Unlock()
			stamped[w.Name] = req
			return req, nil
		}
		return stamped
	}
	fast := optedIn("fast", true)
	fast.Annotations[checkpoint.AnnotationMaxWait] = "30s"
	slow := optedIn("slow", true) // no request: the 5m default

	t.Run("short request is not stretched to the neighbour's default", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = []platform.Workload{fast, slow}
		stamped := capture(plat)
		c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
		start := clock.Now()
		plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
			// Pass 1 sees both pending. The 30s window then passes, so pass 2
			// finds fast expired and slow still pending; the rest of slow's
			// 5m passes and pass 3 finds slow expired.
			switch {
			case w.Name == "slow" && pass == 1:
				clock.Advance(30 * time.Second)
			case w.Name == "slow" && pass == 2:
				clock.Advance(4*time.Minute + 30*time.Second)
			}
			return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
		}
		before := snapshotCheckpointMetrics(t)

		runDrain(t, c, inc)

		if got, want := stamped["fast"].DeadlineAt, start.Add(30*time.Second); !got.Equal(want) {
			t.Fatalf("fast deadline %v, want %v (its own 30s request, not the neighbour's 5m)", got, want)
		}
		if got, want := stamped["slow"].DeadlineAt, start.Add(5*time.Minute); !got.Equal(want) {
			t.Fatalf("slow deadline %v, want %v (the policy default)", got, want)
		}
		if stamped["fast"].DeadlineAt.Equal(stamped["slow"].DeadlineAt) {
			t.Fatal("both workloads got the same deadline: the per-workload grant was lost")
		}
		if !stamped["fast"].RequestedAt.Equal(start) || !stamped["slow"].RequestedAt.Equal(start) {
			t.Fatalf("requested-at fast=%v slow=%v, want one common request time %v",
				stamped["fast"].RequestedAt, stamped["slow"].RequestedAt, start)
		}
		assertCalls(t, plat.Calls(), []string{
			"list n1", "request training/fast@n1", "request training/slow@n1",
			"observe training/fast", "observe training/slow",
			"observe training/fast", "observe training/slow",
			"observe training/slow", // fast is already settled; only slow is looked at
			"drain n1",
		})
		assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 2}, 2, true)
		trail := auditResults(t, c, inc)
		// The step's audited wait is the LATEST deadline in force: the default.
		requireAudit(t, trail, "checkpoint: requested 2 workload(s) training/fast, training/slow; deadline 2026-09-29T10:05:00Z (wait 5m0s) before platform.drain")
		requireAudit(t, trail, "training/fast=expired, training/slow=expired; proceeding with platform.drain")
	})

	t.Run("the step budget bounds each grant on its own", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = []platform.Workload{fast, slow}
		stamped := capture(plat)
		plat.observe = func(platform.Workload, int) checkpoint.Observation {
			return checkpoint.Observation{Found: false}
		}
		c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
		step := drainStep()
		step.Timeout = playbook.Duration(3 * time.Minute) // 3m - 90s reserve = 90s of budget
		if _, err := c.executePlatformStep(context.Background(), inc, "drain", step); err != nil {
			t.Fatal(err)
		}
		if got, want := stamped["fast"].DeadlineAt, clock.Now().Add(30*time.Second); !got.Equal(want) {
			t.Fatalf("fast deadline %v, want %v: a request under the budget is honored as-is", got, want)
		}
		if got, want := stamped["slow"].DeadlineAt, clock.Now().Add(90*time.Second); !got.Equal(want) {
			t.Fatalf("slow deadline %v, want %v: the default is cut to the budget", got, want)
		}
	})
}

// The wait histogram measures from the DURABLE request time. A same-incident
// request resumed after a controller restart keeps the RequestedAt the object
// already carries, so the observation is the whole window the workload was
// given, not only the part this retry slept through. A stamp that cannot be
// trusted (in the future, or absurdly old) is bounded, and the observation
// itself is cut to the 30m ceiling, so no observation is negative or above
// the ceiling even when the step waits on after an ancient stamp.
func TestCheckpointWaitMetricSpansTheDurableWindow(t *testing.T) {
	// The fake resolves the way the Kubernetes adapter does: a same-incident
	// stamp keeps its original RequestedAt and the earlier of the deadlines.
	resumeLikeAdapter := func(w platform.Workload, req checkpoint.Request, _ int) (checkpoint.Request, error) {
		if existing, ok := checkpoint.ParseRequest(w.Annotations); ok && existing.IncidentID == req.IncidentID {
			req.DeadlineAt = checkpoint.ResolveDeadline(req.DeadlineAt, req.IncidentID, &existing)
			if !existing.RequestedAt.IsZero() {
				req.RequestedAt = existing.RequestedAt
			}
		}
		return req, nil
	}
	// expireAfter advances the clock by d once the first look has happened,
	// so the second look finds the window over.
	expireAfter := func(clock *fakeClock, d time.Duration) func(platform.Workload, int) checkpoint.Observation {
		return func(w platform.Workload, pass int) checkpoint.Observation {
			if pass == 1 {
				clock.Advance(d)
			}
			return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
		}
	}
	stampedWorkload := func(req checkpoint.Request) platform.Workload {
		w := optedIn("trainer", true)
		for k, v := range req.Annotations() {
			w.Annotations[k] = v
		}
		return w
	}

	t.Run("resumed same-incident request measures from the original request time", func(t *testing.T) {
		plat := &checkpointPlatform{}
		c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
		// Asked 3m ago with a 5m window: 2m of it is left when this step resumes.
		plat.workloads = []platform.Workload{stampedWorkload(checkpoint.Request{
			IncidentID: inc.ID, RequestedAt: clock.Now().Add(-3 * time.Minute),
			DeadlineAt: clock.Now().Add(2 * time.Minute), Reason: inc.Class, NextAction: "platform.drain",
		})}
		plat.request = resumeLikeAdapter
		plat.observe = expireAfter(clock, 2*time.Minute)
		before := snapshotCheckpointMetrics(t)
		count, sum := waitHistogram(t)

		runDrain(t, c, inc)

		assertCalls(t, plat.Calls(), []string{
			"list n1", "request training/trainer@n1",
			"observe training/trainer", "observe training/trainer", "drain n1",
		})
		assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 1}, 1, true)
		// 3m already elapsed plus the 2m this step waited: the durable window,
		// not the 2m the retry slept.
		assertWaitObserved(t, count, sum, 5*time.Minute)
		requireAudit(t, auditResults(t, c, inc), "deadline 2026-09-29T10:02:00Z (wait 2m0s)")
	})

	t.Run("a future durable request time falls back to this step's own", func(t *testing.T) {
		plat := &checkpointPlatform{}
		c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
		// A tampered stamp: requested-at an hour from now, deadline in 2m.
		plat.workloads = []platform.Workload{stampedWorkload(checkpoint.Request{
			IncidentID: inc.ID, RequestedAt: clock.Now().Add(time.Hour),
			DeadlineAt: clock.Now().Add(2 * time.Minute), Reason: inc.Class, NextAction: "platform.drain",
		})}
		plat.request = resumeLikeAdapter
		plat.observe = expireAfter(clock, 2*time.Minute)
		count, sum := waitHistogram(t)

		runDrain(t, c, inc)

		assertWaitObserved(t, count, sum, 2*time.Minute)
	})

	t.Run("an ancient same-incident request time observes at most the ceiling", func(t *testing.T) {
		plat := &checkpointPlatform{}
		c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
		// A stale or tampered stamp: requested-at in 1970, deadline in 2m. The
		// start is floored 30m before this step's request time, and the 2m this
		// step then waits must not push the observation past the ceiling.
		plat.workloads = []platform.Workload{stampedWorkload(checkpoint.Request{
			IncidentID: inc.ID, RequestedAt: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
			DeadlineAt: clock.Now().Add(2 * time.Minute), Reason: inc.Class, NextAction: "platform.drain",
		})}
		plat.request = resumeLikeAdapter
		plat.observe = expireAfter(clock, 2*time.Minute)
		count, sum := waitHistogram(t)

		runDrain(t, c, inc)

		assertWaitObserved(t, count, sum, checkpoint.MaxWaitCeiling)
	})

	t.Run("an honored foreign request measures from its own request time, at most the ceiling", func(t *testing.T) {
		for name, tc := range map[string]struct {
			requestedAgo time.Duration
			want         time.Duration
		}{
			"recent foreign request": {requestedAgo: time.Minute, want: 3 * time.Minute},
			// Requested-at in 1970: the start is floored 30m before this step's
			// request time, and the 2m this step then waited is cut off by the
			// ceiling on the observation itself.
			"ancient foreign stamp": {requestedAgo: 56 * 365 * 24 * time.Hour, want: checkpoint.MaxWaitCeiling},
		} {
			t.Run(name, func(t *testing.T) {
				plat := &checkpointPlatform{}
				c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
				plat.workloads = []platform.Workload{stampedWorkload(checkpoint.Request{
					IncidentID: "inc-other", RequestedAt: clock.Now().Add(-tc.requestedAgo),
					DeadlineAt: clock.Now().Add(2 * time.Minute), Reason: inc.Class, NextAction: "platform.drain",
				})}
				plat.request = refuseLikeAdapter(nil)
				plat.observe = expireAfter(clock, 2*time.Minute)
				count, sum := waitHistogram(t)

				runDrain(t, c, inc)

				assertWaitObserved(t, count, sum, tc.want)
			})
		}
	})
}

// A same-incident stamp already on the workload is the durable record of the
// window it was granted. When that window has already expired — a controller
// restart after the wait — the request resolves to the expired deadline and
// the step proceeds at once: no new countdown, no sleep, no deferral.
func TestCheckpointSameIncidentExpiredStampProceedsImmediately(t *testing.T) {
	plat := &checkpointPlatform{}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	expired := checkpoint.Request{
		IncidentID: inc.ID, RequestedAt: clock.Now().Add(-10 * time.Minute),
		DeadlineAt: clock.Now().Add(-5 * time.Minute), Reason: inc.Class, NextAction: "platform.drain",
	}
	w := optedIn("trainer", true)
	for k, v := range expired.Annotations() {
		w.Annotations[k] = v
	}
	plat.workloads = []platform.Workload{w}
	// The fake resolves the way the Kubernetes adapter does.
	plat.request = func(w platform.Workload, req checkpoint.Request, _ int) (checkpoint.Request, error) {
		existing, _ := checkpoint.ParseRequest(w.Annotations)
		req.DeadlineAt = checkpoint.ResolveDeadline(req.DeadlineAt, req.IncidentID, &existing)
		return req, nil
	}
	before := snapshotCheckpointMetrics(t)

	runDrain(t, c, inc)

	assertCalls(t, plat.Calls(), []string{
		"list n1", "request training/trainer@n1", "observe training/trainer", "drain n1",
	})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 1}, 1, false)
	requireAudit(t, auditResults(t, c, inc), "deadline 2026-09-29T09:55:00Z")
}

// A live request from ANOTHER incident is never overwritten and never granted
// a new window: this step honors the deadline the workload already carries,
// capped to the deadline this step itself proposed.
func TestCheckpointForeignRequestIsHonoredButCapped(t *testing.T) {
	for name, tc := range map[string]struct {
		foreignIn time.Duration
		wantWait  time.Duration
	}{
		"earlier foreign deadline is honored as-is": {foreignIn: 2 * time.Minute, wantWait: 2 * time.Minute},
		"later foreign deadline is capped to ours":  {foreignIn: 20 * time.Minute, wantWait: 5 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			plat := &checkpointPlatform{}
			c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
			foreign := checkpoint.Request{
				IncidentID: "inc-other", RequestedAt: clock.Now().Add(-time.Minute),
				DeadlineAt: clock.Now().Add(tc.foreignIn), Reason: inc.Class, NextAction: "platform.drain",
			}
			w := optedIn("trainer", true)
			for k, v := range foreign.Annotations() {
				w.Annotations[k] = v
			}
			plat.workloads = []platform.Workload{w}
			plat.request = refuseLikeAdapter(nil)
			plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
				if pass == 1 {
					clock.Advance(tc.wantWait) // exactly the honored deadline, seen by pass 2
				}
				return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
			}
			before := snapshotCheckpointMetrics(t)
			start := clock.Now()

			runDrain(t, c, inc)

			assertCalls(t, plat.Calls(), []string{
				"list n1", "request training/trainer@n1",
				"observe training/trainer", "observe training/trainer", "drain n1",
			})
			assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 1}, 1, true)
			// The wait really was the honored (capped) deadline: the completion
			// row, which carries only this step's own elapsed time, says so.
			trail := auditResults(t, c, inc)
			requireAudit(t, trail, "checkpoint: complete after "+clock.Now().Sub(start).String()+": training/trainer=expired")
			// The opening row is the generic one: the honored deadline is a
			// value read off the object and is not written.
			requireAudit(t, trail, "checkpoint: coordinating 1 workload(s) training/trainer; honoring an already-live request before platform.drain")
			for _, row := range trail {
				if strings.Contains(row, "deadline ") || strings.Contains(row, "inc-other") {
					t.Fatalf("audit row renders the honored foreign request: %q", row)
				}
			}
		})
	}
}

// The adapter promises the live foreign request with the foreign error. An
// adapter that breaks that promise, or reports a request that could not be a
// live foreign one, binds nothing: nothing is waited for, and the drain
// proceeds. The listing snapshot is never consulted as a fallback, so a stale
// listed request cannot stand in for the live one.
func TestCheckpointForeignRequestWithoutALiveRequestIsUnreachable(t *testing.T) {
	for name, returned := range map[string]func(clock *fakeClock, inc *types.Incident) checkpoint.Request{
		"zero request": func(*fakeClock, *types.Incident) checkpoint.Request { return checkpoint.Request{} },
		"this step's own ID": func(c *fakeClock, inc *types.Incident) checkpoint.Request {
			return checkpoint.Request{IncidentID: inc.ID, DeadlineAt: c.Now().Add(time.Minute)}
		},
		"already expired": func(c *fakeClock, _ *types.Incident) checkpoint.Request {
			return checkpoint.Request{IncidentID: "inc-other", DeadlineAt: c.Now().Add(-time.Second)}
		},
		"deadline at requested": func(c *fakeClock, _ *types.Incident) checkpoint.Request {
			return checkpoint.Request{IncidentID: "inc-other", DeadlineAt: c.Now()}
		},
	} {
		t.Run(name, func(t *testing.T) {
			plat := &checkpointPlatform{}
			c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
			// The LISTING shows a perfectly live foreign request; it must not
			// be what the step binds to.
			listed := checkpoint.Request{
				IncidentID: "inc-listed", RequestedAt: clock.Now().Add(-time.Minute),
				DeadlineAt: clock.Now().Add(2 * time.Minute), Reason: inc.Class, NextAction: "platform.drain",
			}
			w := optedIn("trainer", true)
			for k, v := range listed.Annotations() {
				w.Annotations[k] = v
			}
			plat.workloads = []platform.Workload{w}
			plat.request = func(w platform.Workload, _ checkpoint.Request, _ int) (checkpoint.Request, error) {
				return returned(clock, inc), fmt.Errorf("%w: %s/%s carries a live request from another incident",
					platform.ErrCheckpointForeignRequest, w.Namespace, w.Name)
			}
			before := snapshotCheckpointMetrics(t)

			runDrain(t, c, inc)

			assertCalls(t, plat.Calls(), []string{"list n1", "request training/trainer@n1", "drain n1"})
			assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeUnreachable: 1}, 0, false)
			requireAudit(t, auditResults(t, c, inc), "no request in force for training/trainer=unreachable")
		})
	}
}

// The request race the live result exists for. The listing showed inc-A's
// request, and the workload's retained "complete:inc-A"; by the time this step
// asks, inc-B has stamped the object. The step binds to inc-B, the live one,
// so the retained inc-A answer settles nothing and the step waits out inc-B's
// (capped) window. Then the same again one incident later: bound to inc-B, a
// live object restamped by inc-C with "complete:inc-B" left on it is not an
// acknowledgement either. In neither case may a listed or retained value be
// read as the answer to the question in force.
func TestCheckpointStaleListedForeignRequestCannotAcknowledge(t *testing.T) {
	stamp := func(w platform.Workload, incidentID, state string, clock *fakeClock, deadlineIn time.Duration, inc *types.Incident) map[string]string {
		ann := map[string]string{}
		for k, v := range w.Annotations {
			ann[k] = v
		}
		req := checkpoint.Request{
			IncidentID: incidentID, RequestedAt: clock.Now().Add(-time.Minute),
			DeadlineAt: clock.Now().Add(deadlineIn), Reason: inc.Class, NextAction: "platform.drain",
		}
		for k, v := range req.Annotations() {
			ann[k] = v
		}
		if state != "" {
			ann[checkpoint.AnnotationState] = state
		}
		return ann
	}

	t.Run("listed A with complete:A, live B", func(t *testing.T) {
		plat := &checkpointPlatform{}
		c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
		w := optedIn("trainer", true)
		w.Annotations = stamp(w, "inc-A", checkpoint.StateCompleteFor("inc-A"), clock, 2*time.Minute, inc)
		plat.workloads = []platform.Workload{w}
		// The live object: inc-B's request, with inc-A's answer still on it.
		live := stamp(optedIn("trainer", true), "inc-B", checkpoint.StateCompleteFor("inc-A"), clock, 3*time.Minute, inc)
		plat.request = refuseLikeAdapter(live)
		plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
			if pass == 1 {
				clock.Advance(3 * time.Minute) // inc-B's whole window
			}
			return checkpoint.Observation{Found: true, UID: w.UID, Annotations: live}
		}
		before := snapshotCheckpointMetrics(t)

		runDrain(t, c, inc)

		assertCalls(t, plat.Calls(), []string{
			"list n1", "request training/trainer@n1",
			"observe training/trainer", "observe training/trainer", "drain n1",
		})
		// Not acknowledged on the first look, and expired only once inc-B's
		// window (3m, not inc-A's 2m) is over.
		assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 1}, 1, true)
		requireAudit(t, auditResults(t, c, inc), "checkpoint: complete after 3m0s: training/trainer=expired")
	})

	t.Run("bound to live B, then restamped by C with complete:B", func(t *testing.T) {
		plat := &checkpointPlatform{}
		c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
		w := optedIn("trainer", true)
		w.Annotations = stamp(w, "inc-A", "", clock, 2*time.Minute, inc)
		plat.workloads = []platform.Workload{w}
		liveB := stamp(optedIn("trainer", true), "inc-B", "", clock, 3*time.Minute, inc)
		plat.request = refuseLikeAdapter(liveB)
		plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
			if pass == 1 {
				// Before the second look inc-C stamps the object, and the
				// workload's answer to inc-B is retained. Then inc-B's window ends.
				clock.Advance(3 * time.Minute)
				return checkpoint.Observation{Found: true, UID: w.UID, Annotations: liveB}
			}
			liveC := stamp(optedIn("trainer", true), "inc-C", checkpoint.StateCompleteFor("inc-B"), clock, 5*time.Minute, inc)
			return checkpoint.Observation{Found: true, UID: w.UID, Annotations: liveC}
		}
		before := snapshotCheckpointMetrics(t)

		runDrain(t, c, inc)

		assertCalls(t, plat.Calls(), []string{
			"list n1", "request training/trainer@n1",
			"observe training/trainer", "observe training/trainer", "drain n1",
		})
		assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 1}, 1, true)
	})

	t.Run("bound to live B, B's own answer under B's live stamp acknowledges", func(t *testing.T) {
		plat := &checkpointPlatform{}
		c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
		w := optedIn("trainer", true)
		w.Annotations = stamp(w, "inc-A", checkpoint.StateCompleteFor("inc-A"), clock, 2*time.Minute, inc)
		plat.workloads = []platform.Workload{w}
		liveB := stamp(optedIn("trainer", true), "inc-B", checkpoint.StateCompleteFor("inc-B"), clock, 3*time.Minute, inc)
		plat.request = refuseLikeAdapter(liveB)
		plat.observe = func(w platform.Workload, _ int) checkpoint.Observation {
			return checkpoint.Observation{Found: true, UID: w.UID, Annotations: liveB}
		}
		before := snapshotCheckpointMetrics(t)

		runDrain(t, c, inc)

		assertCalls(t, plat.Calls(), []string{"list n1", "request training/trainer@n1", "observe training/trainer", "drain n1"})
		assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeAcknowledged: 1}, 1, false)
	})
}

// A listing that fails is one unreachable answer for the step, audited, and
// the drain proceeds: coordination must never hold a remediation by failing.
func TestCheckpointListingFailureProceedsWithoutCoordination(t *testing.T) {
	plat := &checkpointPlatform{}
	plat.listErr = errors.New("apiserver: timeout")
	c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
	before := snapshotCheckpointMetrics(t)

	runDrain(t, c, inc)

	assertCalls(t, plat.Calls(), []string{"list n1", "drain n1"})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeUnreachable: 1}, 0, false)
	requireAudit(t, auditResults(t, c, inc), "checkpoint: workload listing failed; proceeding without coordination")
}

// Dry run: never a patch, never a wait, never a metric. With the policy on,
// the simulated result carries a read-only projection of what a real run
// would have asked; when that projection cannot be made the dry run is still a
// success and says so.
func TestCheckpointDryRunProjectsWithoutPatching(t *testing.T) {
	t.Run("projects the candidate count", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = []platform.Workload{optedIn("trainer", true), optedIn("logger", false), {
			Name: "sidecar", Namespace: "training", UID: "uid-sidecar", UsesGPU: true,
		}}
		c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
		inc.DryRun = true
		before := snapshotCheckpointMetrics(t)
		res, err := c.executeStep(context.Background(), inc, evictStep())
		if err != nil || !res.OK {
			t.Fatalf("dry run = %+v, %v", res, err)
		}
		want := "DRY-RUN: would execute platform.evict_gpu_workload on n1; checkpoint coordination would request 1 candidate workload(s) of 3 listed"
		if res.Output != want {
			t.Fatalf("output = %q, want %q", res.Output, want)
		}
		assertCalls(t, plat.Calls(), []string{"list n1"})
		assertCheckpointMetrics(t, before, nil, 0, false)
		if got := auditResults(t, c, inc); len(got) != 0 {
			t.Fatalf("dry run wrote audit rows: %v", got)
		}
	})
	t.Run("projection failure leaves the dry run successful", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.listErr = errors.New("apiserver: timeout")
		c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
		inc.DryRun = true
		res, err := c.executeStep(context.Background(), inc, drainStep())
		if err != nil || !res.OK {
			t.Fatalf("dry run = %+v, %v", res, err)
		}
		if !strings.HasSuffix(res.Output, "; checkpoint projection unavailable (workload listing failed)") {
			t.Fatalf("output = %q", res.Output)
		}
	})
	t.Run("disabled policy projects nothing and lists nothing", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = []platform.Workload{optedIn("trainer", true)}
		c, inc, _ := checkpointFixture(t, plat, checkpoint.Policy{})
		inc.DryRun = true
		res, err := c.executeStep(context.Background(), inc, drainStep())
		if err != nil || res.Output != "DRY-RUN: would execute platform.drain on n1" {
			t.Fatalf("dry run = %+v, %v", res, err)
		}
		assertCalls(t, plat.Calls(), nil)
	})
}

// A step that is not a disruption gets no pre-phase whatever the policy says.
func TestCheckpointOnlyWrapsDrainAndEvict(t *testing.T) {
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
	before := snapshotCheckpointMetrics(t)
	if _, err := c.executePlatformStep(context.Background(), inc, "cordon",
		&playbook.Step{Name: "cordon", Action: "platform.cordon"}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, plat.Calls(), nil)
	assertCheckpointMetrics(t, before, nil, 0, false)
}

// stepContext is executeStep's own context for a step of the given timeout:
// the timeout plus the agent-result grace, exactly as executeStep builds it.
func stepContext(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout+agentResultGrace)
	t.Cleanup(cancel)
	return ctx
}

// deadlineOf is the deadline of ctx, which must have one.
func deadlineOf(t *testing.T, ctx context.Context, call string) time.Time {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("%s ran under a context with no deadline", call)
	}
	return deadline
}

// The pre-phase runs under its own deadline, the step's budget minus the
// reserve, and the disruption runs under the step's own context. Checked on
// the contexts themselves, not by waiting: every listing, request and
// observation carries a deadline at least the reserve (plus the grace) before
// the step's, and the drain carries the step's own, untouched.
func TestCheckpointPrePhaseRunsUnderItsOwnDeadline(t *testing.T) {
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		return stampedAck(w, fixtureIncidentID, clock.Now().Add(30*time.Second))
	}
	const timeout = 2 * time.Minute // budget: 2m - 90s = 30s
	ctx := stepContext(t, timeout)
	stepDeadline := deadlineOf(t, ctx, "step")

	var mu sync.Mutex
	deadlines := map[string]time.Time{}
	plat.seen = func(ctx context.Context, call string) {
		mu.Lock()
		defer mu.Unlock()
		deadlines[call] = deadlineOf(t, ctx, call)
	}
	step := drainStep()
	step.Timeout = playbook.Duration(timeout)
	if _, err := c.executePlatformStep(ctx, inc, "drain", step); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, plat.Calls(), []string{"list n1", "request training/trainer@n1", "observe training/trainer", "drain n1"})
	for _, call := range []string{"list n1", "request training/trainer@n1", "observe training/trainer"} {
		// The budget is measured from the step deadline and the coordination
		// deadline is set a few instructions later, so a hair under is exact.
		kept := stepDeadline.Sub(deadlines[call])
		if kept < checkpointBudgetReserve+agentResultGrace-10*time.Millisecond {
			t.Fatalf("%s ran under a deadline only %v before the step's; the %v reserve (plus the %v grace) must be kept for the disruption",
				call, kept, checkpointBudgetReserve, agentResultGrace)
		}
		if kept > checkpointBudgetReserve+agentResultGrace+time.Second {
			t.Fatalf("%s ran under a deadline %v before the step's: the whole budget must be available to the pre-phase", call, kept)
		}
	}
	if !deadlines["drain n1"].Equal(stepDeadline) {
		t.Fatalf("drain ran under deadline %v, want the step's own %v", deadlines["drain n1"], stepDeadline)
	}
}

// A platform that hangs — on the listing, on the patch, or on a read — spends
// the coordination budget and nothing else. The drain then starts under the
// step's own context with the reserve intact, the hang is one unreachable
// answer, and the step neither fails nor waits past the budget. The audit
// row whose turn comes after the budget is spent is attempted under the
// spent coordination context, never under the step's own: it fails fast, is
// logged, and is absent from the trail. The metric, not the row, is what a
// spent budget is guaranteed to leave behind.
func TestCheckpointBlockingPlatformCannotConsumeTheReserve(t *testing.T) {
	const budget = 300 * time.Millisecond
	const timeout = checkpointBudgetReserve + budget
	for _, tc := range []struct {
		block string
		calls []string
		audit string
	}{
		{"list", []string{"list n1", "drain n1"}, "checkpoint: workload listing failed; proceeding without coordination"},
		{"request", []string{"list n1", "request training/trainer@n1", "drain n1"}, "no request in force for training/trainer=unreachable"},
		{"observe", []string{"list n1", "request training/trainer@n1", "observe training/trainer", "drain n1"}, "training/trainer=unreachable; proceeding with platform.drain"},
	} {
		t.Run("blocking "+tc.block, func(t *testing.T) {
			plat := &checkpointPlatform{}
			plat.workloads = []platform.Workload{optedIn("trainer", true)}
			plat.blockOn = map[string]bool{tc.block: true}
			c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
			var logs bytes.Buffer
			c.log = slog.New(slog.NewTextHandler(&logs, nil))
			ctx := stepContext(t, timeout)
			var drainCtx context.Context
			plat.seen = func(ctx context.Context, call string) {
				if call == "drain n1" {
					drainCtx = ctx
				}
			}
			before := snapshotCheckpointMetrics(t)
			step := drainStep()
			step.Timeout = playbook.Duration(timeout)

			started := time.Now()
			if _, err := c.executePlatformStep(ctx, inc, "drain", step); err != nil {
				t.Fatalf("a hung platform call must never fail the step: %v", err)
			}
			if took := time.Since(started); took > budget+5*time.Second {
				t.Fatalf("the step took %v; the hang must end with the %v budget", took, budget)
			}
			assertCalls(t, plat.Calls(), tc.calls)
			if drainCtx.Err() != nil {
				t.Fatalf("the drain started under an expired context: %v", drainCtx.Err())
			}
			if left := time.Until(deadlineOf(t, drainCtx, "drain")); left < checkpointBudgetReserve+agentResultGrace-5*time.Second {
				t.Fatalf("the drain started with %v left, want at least the %v reserve", left, checkpointBudgetReserve)
			}
			assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeUnreachable: 1}, map[string]uint64{"list": 0, "request": 0, "observe": 1}[tc.block], false)
			requireNoAudit(t, auditResults(t, c, inc), tc.audit)
			if !strings.Contains(logs.String(), "checkpoint audit append failed") {
				t.Fatalf("the dropped audit row was not logged:\n%s", logs.String())
			}
		})
	}
}

// auditContextStore wraps the controller's store and records, for every audit
// append, the state of the context it was called with at the moment of the
// call: whether it was already done, and what deadline it carried. It is how a
// test proves a best-effort row was attempted without a live budget to spend.
type auditContextStore struct {
	store.Store
	mu   sync.Mutex
	rows []auditAppend
}

type auditAppend struct {
	result      string
	err         error // ctx.Err() at the time of the call
	deadline    time.Time
	hasDeadline bool
}

func (s *auditContextStore) AppendAudit(ctx context.Context, e *types.AuditEntry) error {
	deadline, ok := ctx.Deadline()
	s.mu.Lock()
	s.rows = append(s.rows, auditAppend{result: e.Result, err: ctx.Err(), deadline: deadline, hasDeadline: ok})
	s.mu.Unlock()
	return s.Store.AppendAudit(ctx, e)
}

func (s *auditContextStore) appends() []auditAppend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]auditAppend(nil), s.rows...)
}

// requireNoAudit is the inverse of requireAudit: no row may contain substr.
func requireNoAudit(t *testing.T, results []string, substr string) {
	t.Helper()
	for _, r := range results {
		if strings.Contains(r, substr) {
			t.Fatalf("an audit row contains %q, which must not have been written under a spent budget: %q", substr, r)
		}
	}
}

// A wait that really sleeps leaves the step's context alone: after a short
// real wait the drain still runs under a live context with the reserve left.
func TestCheckpointShortWaitLeavesTheDrainContextIntact(t *testing.T) {
	plat := &checkpointPlatform{}
	w := optedIn("trainer", true)
	w.Annotations[checkpoint.AnnotationMaxWait] = "1s"
	plat.workloads = []platform.Workload{w}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	c.checkpointPoll = 150 * time.Millisecond // a real sleep between the two looks
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		if pass == 1 {
			clock.Advance(time.Second) // pass 2 finds the 1s window over
		}
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
	}
	const timeout = 3 * time.Minute
	ctx := stepContext(t, timeout)
	var drainCtx context.Context
	plat.seen = func(ctx context.Context, call string) {
		if call == "drain n1" {
			drainCtx = ctx
		}
	}
	before := snapshotCheckpointMetrics(t)
	step := drainStep()
	step.Timeout = playbook.Duration(timeout)
	started := time.Now()
	if _, err := c.executePlatformStep(ctx, inc, "drain", step); err != nil {
		t.Fatal(err)
	}
	if slept := time.Since(started); slept < 150*time.Millisecond {
		t.Fatalf("the step took %v; it must really have slept one poll interval", slept)
	}
	assertCalls(t, plat.Calls(), []string{"list n1", "request training/trainer@n1", "observe training/trainer", "observe training/trainer", "drain n1"})
	if drainCtx.Err() != nil {
		t.Fatalf("the drain started under an expired context: %v", drainCtx.Err())
	}
	if left := time.Until(deadlineOf(t, drainCtx, "drain")); left < timeout {
		t.Fatalf("the drain started with %v left of %v: the wait must come out of the budget, and it slept far less than that", left, timeout+agentResultGrace)
	}
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 1}, 1, true)
}

// Each sleep is cut to what is left until the latest deadline in force: a
// window shorter than the poll interval does not sleep a whole interval.
func TestCheckpointSleepIsCappedByTheRemainingWindow(t *testing.T) {
	plat := &checkpointPlatform{}
	w := optedIn("trainer", true)
	w.Annotations[checkpoint.AnnotationMaxWait] = "20ms"
	plat.workloads = []platform.Workload{w}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	c.checkpointPoll = time.Hour // would hang the test if it were the sleep
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		if pass == 1 {
			clock.Advance(20 * time.Millisecond)
		}
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
	}
	before := snapshotCheckpointMetrics(t)
	started := time.Now()
	runDrain(t, c, inc)
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the step took %v; the sleep must be capped at the 20ms left on the window, not the poll interval", took)
	}
	assertCalls(t, plat.Calls(), []string{"list n1", "request training/trainer@n1", "observe training/trainer", "observe training/trainer", "drain n1"})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 1}, 1, true)
	requireAudit(t, auditResults(t, c, inc), "deadline 2026-09-29T10:00:00.02Z (wait 20ms)")
}

// When the coordination budget runs out on the wall clock before the logical
// deadline is observed, the wait ends: what is still open was not observed
// for the step's own reason, not the workload's, so it is unreachable, not
// expired (its stamped deadline has not passed), no further read goes out on
// the spent context, and the drain follows under the step's context.
func TestCheckpointBudgetExpiryEndsTheWaitAsUnreachable(t *testing.T) {
	const budget = 200 * time.Millisecond
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
	var mu sync.Mutex
	var readAfterBudget int
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		// The fake clock never moves, so the logical deadline never arrives;
		// only the coordination context can end this.
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
	}
	plat.seen = func(ctx context.Context, call string) {
		if strings.HasPrefix(call, "observe") && ctx.Err() != nil {
			mu.Lock()
			readAfterBudget++
			mu.Unlock()
		}
	}
	var drainCtx context.Context
	seen := plat.seen
	plat.seen = func(ctx context.Context, call string) {
		seen(ctx, call)
		if call == "drain n1" {
			drainCtx = ctx
		}
	}
	before := snapshotCheckpointMetrics(t)
	step := drainStep()
	step.Timeout = playbook.Duration(checkpointBudgetReserve + budget)
	ctx := stepContext(t, checkpointBudgetReserve+budget)
	started := time.Now()
	if _, err := c.executePlatformStep(ctx, inc, "drain", step); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took < budget || took > budget+5*time.Second {
		t.Fatalf("the step took %v, want about the %v budget", took, budget)
	}
	calls := plat.Calls()
	if calls[len(calls)-1] != "drain n1" || drainCtx.Err() != nil {
		t.Fatalf("calls %v, drain ctx err %v: the drain must follow under a live context", calls, drainCtx.Err())
	}
	if readAfterBudget != 0 {
		t.Fatalf("%d read(s) went out on the spent coordination context", readAfterBudget)
	}
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeUnreachable: 1}, 1, true)
	requireNoAudit(t, auditResults(t, c, inc), "training/trainer=expired")
}

// The deferral metric counts only a sleep that began. The loop's sleep is the
// poll interval cut to the latest deadline and to the coordination window; at
// or past either on the coordinator's clock it is zero or negative, which is
// no wait and must not be reported as one.
func TestCheckpointSleepForIsNonPositiveOnceTheWindowIsSpent(t *testing.T) {
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	poll := time.Second
	cases := []struct {
		name              string
		latest, budgetEnd time.Time
		want              time.Duration
	}{
		{"poll bounds a long window", at.Add(time.Minute), at.Add(time.Minute), poll},
		{"deadline shorter than poll", at.Add(20 * time.Millisecond), at.Add(time.Minute), 20 * time.Millisecond},
		{"budget shorter than poll", at.Add(time.Minute), at.Add(20 * time.Millisecond), 20 * time.Millisecond},
		{"deadline reached", at, at.Add(time.Minute), 0},
		{"budget reached", at.Add(time.Minute), at, 0},
		{"budget already behind", at.Add(time.Minute), at.Add(-time.Second), -time.Second},
	}
	for _, tc := range cases {
		if got := checkpointSleepFor(at, tc.latest, tc.budgetEnd, poll); got != tc.want {
			t.Errorf("%s: sleep %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A window that is already spent on the coordinator's clock when the first
// look ends — a listing that consumed the whole budget — leaves no time to
// sleep. Nothing pauses, so no deferral is recorded; the workload, whose own
// stamped deadline (bounded to the window) has passed, counts expired, and the
// step proceeds at once rather than spinning on reads until the wall-clock
// budget runs out.
func TestCheckpointSpentWindowIsNotADeferral(t *testing.T) {
	const budget = 200 * time.Millisecond
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	plat.seen = func(_ context.Context, call string) {
		if strings.HasPrefix(call, "list") {
			clock.Advance(budget) // the listing spends the whole logical window
		}
	}
	before := snapshotCheckpointMetrics(t)
	step := drainStep()
	step.Timeout = playbook.Duration(checkpointBudgetReserve + budget)
	started := time.Now()
	if _, err := c.executePlatformStep(stepContext(t, checkpointBudgetReserve+budget), inc, "drain", step); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took >= budget {
		t.Fatalf("the step took %v: a zero sleep must not wait out the %v wall-clock budget", took, budget)
	}
	assertCalls(t, plat.Calls(), []string{"list n1", "request training/trainer@n1", "observe training/trainer", "drain n1"})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 1}, 1, false)
}

// The step's own context being cancelled mid-wait ends the wait the same way:
// what is still open is unreachable, never expired, and the disruption is
// still attempted (under the cancelled context, which is the step's own
// business, never coordination's).
func TestCheckpointCancelledStepYieldsUnreachable(t *testing.T) {
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
	ctx, cancel := context.WithCancel(context.Background())
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		if pass == 1 {
			cancel() // the step is cancelled while the request is pending
		}
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
	}
	before := snapshotCheckpointMetrics(t)
	if _, err := c.executePlatformStep(ctx, inc, "drain", drainStep()); err != nil {
		t.Fatalf("coordination must never fail the step: %v", err)
	}
	assertCalls(t, plat.Calls(), []string{"list n1", "request training/trainer@n1", "observe training/trainer", "drain n1"})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeUnreachable: 1}, 1, false)
}

// The one request time, and every deadline measured from it, is fixed when
// the coordination window opens, BEFORE the listing. A listing that is slow
// but does not exhaust the budget spends the window: the stamp still carries
// the original request time, and its deadline never reaches past the end of
// the window as first computed, however long the listing took.
func TestCheckpointSlowListingDoesNotExtendTheStampedDeadline(t *testing.T) {
	const timeout = 6 * time.Minute // budget: 6m - 90s = 4m30s, under the 5m default grant
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("trainer", true)}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	start := clock.Now()
	budgetEnd := start.Add(timeout - checkpointBudgetReserve)
	plat.seen = func(_ context.Context, call string) {
		if strings.HasPrefix(call, "list") {
			clock.Advance(2 * time.Minute) // the listing takes two minutes
		}
	}
	var stamped checkpoint.Request
	plat.request = func(_ platform.Workload, req checkpoint.Request, _ int) (checkpoint.Request, error) {
		stamped = req
		return req, nil
	}
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		if pass == 1 {
			clock.Advance(3 * time.Minute) // past the window
		}
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
	}
	before := snapshotCheckpointMetrics(t)
	step := drainStep()
	step.Timeout = playbook.Duration(timeout)
	if _, err := c.executePlatformStep(stepContext(t, timeout), inc, "drain", step); err != nil {
		t.Fatal(err)
	}
	if !stamped.RequestedAt.Equal(start) {
		t.Fatalf("stamped requested-at %v, want the time the window opened %v, not the time the listing returned", stamped.RequestedAt, start)
	}
	// The budget is measured from the step context's wall-clock deadline, so
	// a hair under the arithmetic end of the window is exact; over it never.
	if stamped.DeadlineAt.After(budgetEnd) || stamped.DeadlineAt.Before(budgetEnd.Add(-time.Second)) {
		t.Fatalf("stamped deadline %v, want the end of the original window %v (a fresh %v after a 2m listing would promise time the step no longer has)",
			stamped.DeadlineAt, budgetEnd, timeout-checkpointBudgetReserve)
	}
	assertCalls(t, plat.Calls(), []string{"list n1", "request training/trainer@n1", "observe training/trainer", "observe training/trainer", "drain n1"})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 1}, 1, true)
}

// Sequential requests share the one request time: the second workload's
// stamp, issued after the first patch took its time, carries the same
// requested-at and a deadline bounded by the same original window. No stamp
// promises a workload time past the end of the window as first computed.
func TestCheckpointSequentialRequestsShareTheOriginalWindow(t *testing.T) {
	const timeout = 6 * time.Minute // budget 4m30s
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("a", true), optedIn("b", true), optedIn("c", true)}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	start := clock.Now()
	budgetEnd := start.Add(timeout - checkpointBudgetReserve)
	stamped := map[string]checkpoint.Request{}
	plat.request = func(w platform.Workload, req checkpoint.Request, _ int) (checkpoint.Request, error) {
		stamped[w.Name] = req
		clock.Advance(time.Minute) // each patch takes a minute
		return req, nil
	}
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		if pass == 1 {
			clock.Advance(5 * time.Minute)
		}
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
	}
	step := drainStep()
	step.Timeout = playbook.Duration(timeout)
	if _, err := c.executePlatformStep(stepContext(t, timeout), inc, "drain", step); err != nil {
		t.Fatal(err)
	}
	if len(stamped) != 3 {
		t.Fatalf("stamped %d requests, want 3", len(stamped))
	}
	for name, req := range stamped {
		if !req.RequestedAt.Equal(start) {
			t.Fatalf("%s: requested-at %v, want the common %v", name, req.RequestedAt, start)
		}
		if req.DeadlineAt.After(budgetEnd) {
			t.Fatalf("%s: deadline %v is past the end of the original window %v", name, req.DeadlineAt, budgetEnd)
		}
	}
}

// When the window closes before a candidate's turn comes (here: the first
// patch hangs until the budget is spent), the remaining candidates are not
// asked at all: no request is issued on the spent context, and each is
// counted unreachable.
func TestCheckpointNoRequestAfterTheBudgetEnds(t *testing.T) {
	const budget = 200 * time.Millisecond
	const timeout = checkpointBudgetReserve + budget
	plat := &checkpointPlatform{}
	plat.workloads = []platform.Workload{optedIn("a", true), optedIn("b", true)}
	plat.blockOn = map[string]bool{"request": true}
	c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
	var logs bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	before := snapshotCheckpointMetrics(t)
	step := drainStep()
	step.Timeout = playbook.Duration(timeout)
	started := time.Now()
	if _, err := c.executePlatformStep(stepContext(t, timeout), inc, "drain", step); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > budget+5*time.Second {
		t.Fatalf("the step took %v; the hang must end with the %v budget", took, budget)
	}
	assertCalls(t, plat.Calls(), []string{"list n1", "request training/a@n1", "drain n1"})
	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeUnreachable: 2}, 0, false)
}

// A drain asks exactly the workloads it will evict. A pod the drain skips —
// finished, a mirror or DaemonSet pod, or an unmanaged pod on a non-forced
// drain — opted in or not, is neither requested nor waited for; with force
// the unmanaged pod is evicted, so it is asked. The eviction step evicts GPU
// holders whatever their drain exclusion, so it asks them all.
func TestCheckpointDrainCandidatesMatchDrainEligibility(t *testing.T) {
	excluded := func(name string, e platform.DrainExclusion) platform.Workload {
		w := optedIn(name, true)
		w.DrainExclusion = e
		return w
	}
	workloads := func() []platform.Workload {
		return []platform.Workload{
			optedIn("managed", true),
			excluded("finished", platform.DrainExclusionTerminal),
			excluded("mirror", platform.DrainExclusionInfrastructure),
			excluded("bare", platform.DrainExclusionUnmanaged),
		}
	}
	exited := func(platform.Workload, int) checkpoint.Observation { return checkpoint.Observation{Found: false} }

	t.Run("drain without force asks only the managed pod", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = workloads()
		plat.observe = exited
		c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
		before := snapshotCheckpointMetrics(t)
		runDrain(t, c, inc)
		assertCalls(t, plat.Calls(), []string{"list n1", "request training/managed@n1", "observe training/managed", "drain n1"})
		assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExited: 1}, 1, false)
		requireAudit(t, auditResults(t, c, inc), "checkpoint: requested 1 workload(s) training/managed;")
	})
	t.Run("forced drain also asks the unmanaged pod", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = workloads()
		plat.observe = exited
		c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
		step := drainStep()
		step.Params = map[string]string{"force": "true"}
		if _, err := c.executePlatformStep(context.Background(), inc, "drain", step); err != nil {
			t.Fatal(err)
		}
		assertCalls(t, plat.Calls(), []string{
			"list n1", "request training/managed@n1", "request training/bare@n1",
			"observe training/managed", "observe training/bare", "drain n1",
		})
	})
	t.Run("only excluded pods means no request and a skip", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = workloads()[1:]
		c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
		before := snapshotCheckpointMetrics(t)
		runDrain(t, c, inc)
		assertCalls(t, plat.Calls(), []string{"list n1", "drain n1"})
		assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeSkipped: 1}, 0, false)
		requireAudit(t, auditResults(t, c, inc), "no eligible workloads on n1; 3 listed")
	})
	t.Run("the eviction step asks every GPU holder", func(t *testing.T) {
		plat := &checkpointPlatform{}
		plat.workloads = workloads()
		plat.observe = exited
		c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
		if _, err := c.executePlatformStep(context.Background(), inc, "evict_gpu_workload", evictStep()); err != nil {
			t.Fatal(err)
		}
		calls := plat.Calls()
		requests := 0
		for _, call := range calls {
			if strings.HasPrefix(call, "request ") {
				requests++
			}
		}
		if requests != 4 {
			t.Fatalf("evict asked %d workloads, want all 4 GPU holders; calls: %v", requests, calls)
		}
	})
	t.Run("the dry-run projection counts the same candidates", func(t *testing.T) {
		for force, want := range map[string]string{"false": "1 candidate workload(s) of 4 listed", "true": "2 candidate workload(s) of 4 listed"} {
			plat := &checkpointPlatform{}
			plat.workloads = workloads()
			c, inc, _ := checkpointFixture(t, plat, enabledPolicy())
			inc.DryRun = true
			step := drainStep()
			step.Params = map[string]string{"force": force}
			res, err := c.executeStep(context.Background(), inc, step)
			if err != nil || !strings.HasSuffix(res.Output, want) {
				t.Fatalf("force=%s: output %q, err %v; want suffix %q", force, res.Output, err, want)
			}
		}
	})
}

// An explicit acknowledgement is bound to the incident that asked. A stale
// "complete:<older incident>" left on a long-lived pod, or an unbound plain
// "complete", never settles this incident's request; the same incident
// resuming its own stamp after a restart still recognizes its bound answer;
// and a workload answering another incident's honored live request is judged
// against THAT incident, the one it read.
func TestCheckpointAcknowledgementIsBoundToTheIncident(t *testing.T) {
	stamped := func(req checkpoint.Request, state string) platform.Workload {
		w := optedIn("trainer", true)
		for k, v := range req.Annotations() {
			w.Annotations[k] = v
		}
		if state != "" {
			w.Annotations[checkpoint.AnnotationState] = state
		}
		return w
	}
	live := func(w platform.Workload, _ int) checkpoint.Observation {
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
	}
	resumeLikeAdapter := func(w platform.Workload, req checkpoint.Request, _ int) (checkpoint.Request, error) {
		if existing, ok := checkpoint.ParseRequest(w.Annotations); ok && existing.IncidentID == req.IncidentID {
			req.DeadlineAt = checkpoint.ResolveDeadline(req.DeadlineAt, req.IncidentID, &existing)
			if !existing.RequestedAt.IsZero() {
				req.RequestedAt = existing.RequestedAt
			}
		}
		return req, nil
	}

	for name, state := range map[string]string{
		"an older incident's acknowledgement": checkpoint.StateCompleteFor("inc-old"),
		"an unbound plain complete":           "complete",
		"a bare prefix":                       checkpoint.StateCompletePrefix,
	} {
		t.Run(name+" does not settle the request", func(t *testing.T) {
			plat := &checkpointPlatform{}
			c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
			w := optedIn("trainer", true)
			w.Annotations[checkpoint.AnnotationState] = state
			plat.workloads = []platform.Workload{w}
			plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
				if pass == 1 {
					clock.Advance(5 * time.Minute)
				}
				return live(w, pass)
			}
			before := snapshotCheckpointMetrics(t)
			runDrain(t, c, inc)
			assertCalls(t, plat.Calls(), []string{
				"list n1", "request training/trainer@n1", "observe training/trainer", "observe training/trainer", "drain n1",
			})
			assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 1}, 1, true)
		})
	}

	t.Run("the same incident recognizes its bound answer after a restart", func(t *testing.T) {
		plat := &checkpointPlatform{}
		c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
		// Asked 1m ago with a 5m window; the workload answered that request.
		plat.workloads = []platform.Workload{stamped(checkpoint.Request{
			IncidentID: inc.ID, RequestedAt: clock.Now().Add(-time.Minute),
			DeadlineAt: clock.Now().Add(4 * time.Minute), Reason: inc.Class, NextAction: "platform.drain",
		}, checkpoint.StateCompleteFor(inc.ID))}
		plat.request = resumeLikeAdapter
		plat.observe = live
		before := snapshotCheckpointMetrics(t)
		count, sum := waitHistogram(t)
		runDrain(t, c, inc)
		assertCalls(t, plat.Calls(), []string{"list n1", "request training/trainer@n1", "observe training/trainer", "drain n1"})
		assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeAcknowledged: 1}, 1, false)
		assertWaitObserved(t, count, sum, time.Minute)
	})

	t.Run("a workload answering an honored foreign request is judged against that incident", func(t *testing.T) {
		for name, tc := range map[string]struct {
			state string
			want  checkpoint.Outcome
			looks int
		}{
			"bound to the foreign incident": {checkpoint.StateCompleteFor("inc-other"), checkpoint.OutcomeAcknowledged, 1},
			"bound to this incident":        {checkpoint.StateCompleteFor(fixtureIncidentID), checkpoint.OutcomeExpired, 2},
		} {
			t.Run(name, func(t *testing.T) {
				plat := &checkpointPlatform{}
				c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
				plat.workloads = []platform.Workload{stamped(checkpoint.Request{
					IncidentID: "inc-other", RequestedAt: clock.Now().Add(-time.Minute),
					DeadlineAt: clock.Now().Add(2 * time.Minute), Reason: inc.Class, NextAction: "platform.drain",
				}, tc.state)}
				plat.request = refuseLikeAdapter(nil)
				plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
					if pass == 1 {
						clock.Advance(2 * time.Minute)
					}
					return live(w, pass)
				}
				before := snapshotCheckpointMetrics(t)
				runDrain(t, c, inc)
				observes := 0
				for _, call := range plat.Calls() {
					if strings.HasPrefix(call, "observe") {
						observes++
					}
				}
				if observes != tc.looks {
					t.Fatalf("observed %d times, want %d", observes, tc.looks)
				}
				assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{tc.want: 1}, 1, tc.looks > 1)
			})
		}
	})
}

// A sub-second checkpoint-max-wait is a valid grant and is stamped as given:
// the deadline is the request time plus exactly the requested window, and the
// audit row records it at that precision.
func TestCheckpointSubSecondWaitIsStampedExactly(t *testing.T) {
	plat := &checkpointPlatform{}
	w := optedIn("trainer", true)
	w.Annotations[checkpoint.AnnotationMaxWait] = "500ms"
	plat.workloads = []platform.Workload{w}
	var stamped checkpoint.Request
	plat.request = func(_ platform.Workload, req checkpoint.Request, _ int) (checkpoint.Request, error) {
		stamped = req
		return req, nil
	}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		if pass == 1 {
			clock.Advance(500 * time.Millisecond)
		}
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
	}
	start := clock.Now()
	runDrain(t, c, inc)
	if want := start.Add(500 * time.Millisecond); !stamped.DeadlineAt.Equal(want) || !stamped.RequestedAt.Equal(start) {
		t.Fatalf("stamped %v..%v, want %v..%v", stamped.RequestedAt, stamped.DeadlineAt, start, want)
	}
	if parsed, ok := checkpoint.ParseRequest(stamped.Annotations()); !ok || !parsed.DeadlineAt.Equal(stamped.DeadlineAt) {
		t.Fatalf("the stamp does not round-trip the sub-second deadline: %+v", parsed)
	}
	trail := auditResults(t, c, inc)
	requireAudit(t, trail, "deadline 2026-09-29T10:00:00.5Z (wait 500ms)")
	requireAudit(t, trail, "checkpoint: complete after 500ms: training/trainer=expired")
}

// When another incident's live request is honored, the log names this
// incident and the workload only: the foreign deadline, incident, reason and
// next action are annotation values a tenant could have written, and none of
// them reaches the log or the audit trail.
func TestCheckpointForeignRequestLeaksNothingIntoLogs(t *testing.T) {
	const foreignID = "inc-other\n{\"level\":\"ERROR\",\"msg\":\"injected\"}"
	plat := &checkpointPlatform{}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	var logs bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	foreign := checkpoint.Request{
		IncidentID: foreignID, RequestedAt: clock.Now().Add(-time.Minute),
		DeadlineAt: clock.Now().Add(2*time.Minute + 750*time.Millisecond), Reason: "tenant-reason", NextAction: "tenant-action",
	}
	w := optedIn("trainer", true)
	for k, v := range foreign.Annotations() {
		w.Annotations[k] = v
	}
	plat.workloads = []platform.Workload{w}
	plat.request = refuseLikeAdapter(nil)
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		if pass == 1 {
			clock.Advance(3 * time.Minute)
		}
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
	}
	runDrain(t, c, inc)

	if !strings.Contains(logs.String(), "honoring another incident's live request") {
		t.Fatalf("the honored request was not logged at all:\n%s", logs.String())
	}
	// Every value of the foreign request, in every rendering the code could
	// produce: the ID, reason and action verbatim; the timestamps in the wire
	// layout, in whole-second RFC 3339, and in Go's default time rendering;
	// and the wait that would be derived from the honored (capped) deadline.
	leaks := []string{
		foreignID, "inc-other", "injected", "tenant-reason", "tenant-action",
		foreign.DeadlineAt.UTC().Format(checkpoint.TimestampLayout),
		foreign.DeadlineAt.UTC().Format(time.RFC3339),
		foreign.DeadlineAt.UTC().String(),
		foreign.RequestedAt.UTC().Format(checkpoint.TimestampLayout),
		foreign.RequestedAt.UTC().Format(time.RFC3339),
		foreign.RequestedAt.UTC().String(),
		"2m45.75s",
		"(wait ",
		"deadline ",
	}
	for _, leaked := range leaks {
		if strings.Contains(logs.String(), leaked) {
			t.Fatalf("log carries the annotation value %q:\n%s", leaked, logs.String())
		}
	}
	trail := auditResults(t, c, inc)
	for _, row := range trail {
		for _, leaked := range leaks {
			if strings.Contains(row, leaked) {
				t.Fatalf("audit row carries the annotation value %q: %q", leaked, row)
			}
		}
	}
	// The row that opens the wait is the generic one, and the phase still
	// waited the honored window: the completion row carries this step's own
	// elapsed time only.
	requireAudit(t, trail, "checkpoint: coordinating 1 workload(s) training/trainer; honoring an already-live request before platform.drain")
	requireAudit(t, trail, "checkpoint: complete after 3m0s: training/trainer=expired; proceeding with platform.drain")
}

// A mixed node: one of the tracked requests is this step's own and one is
// another incident's. ANY honored request makes the opening row generic, since
// the latest deadline may be the foreign one; the own request's deadline is
// not rendered either, and the foreign values appear nowhere.
func TestCheckpointMixedForeignAndOwnRequestsAuditGenerically(t *testing.T) {
	plat := &checkpointPlatform{}
	c, inc, clock := checkpointFixture(t, plat, enabledPolicy())
	var logs bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	foreign := checkpoint.Request{
		IncidentID: "inc-foreign-7f3b", RequestedAt: clock.Now().Add(-90 * time.Second),
		DeadlineAt: clock.Now().Add(4*time.Minute + 250*time.Millisecond), Reason: "foreign-reason", NextAction: "foreign-action",
	}
	held := optedIn("held", true)
	for k, v := range foreign.Annotations() {
		held.Annotations[k] = v
	}
	plat.workloads = []platform.Workload{optedIn("trainer", true), held}
	plat.request = func(w platform.Workload, req checkpoint.Request, _ int) (checkpoint.Request, error) {
		if w.Name == "held" {
			return refuseLikeAdapter(nil)(w, req, 0)
		}
		return req, nil
	}
	plat.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		if w.Name == "trainer" && pass == 1 {
			clock.Advance(5 * time.Minute)
		}
		return checkpoint.Observation{Found: true, UID: w.UID, Annotations: w.Annotations}
	}
	before := snapshotCheckpointMetrics(t)

	runDrain(t, c, inc)

	assertCheckpointMetrics(t, before, map[checkpoint.Outcome]float64{checkpoint.OutcomeExpired: 2}, 2, true)
	trail := auditResults(t, c, inc)
	requireAudit(t, trail, "checkpoint: coordinating 2 workload(s) training/held, training/trainer; honoring an already-live request before platform.drain")
	leaks := []string{
		"inc-foreign-7f3b", "foreign-reason", "foreign-action",
		foreign.DeadlineAt.UTC().Format(checkpoint.TimestampLayout),
		foreign.DeadlineAt.UTC().Format(time.RFC3339),
		foreign.RequestedAt.UTC().Format(checkpoint.TimestampLayout),
		foreign.RequestedAt.UTC().Format(time.RFC3339),
		"4m0.25s", "(wait ", "deadline ",
	}
	for _, leaked := range leaks {
		if strings.Contains(logs.String(), leaked) {
			t.Fatalf("log carries %q:\n%s", leaked, logs.String())
		}
		for _, row := range trail {
			if strings.Contains(row, leaked) {
				t.Fatalf("audit row carries %q: %q", leaked, row)
			}
		}
	}
}
