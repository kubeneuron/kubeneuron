package operations

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// qualificationFixture is a mutable stand-in for the controller's inventory
// and coverage builder. It runs the real pure assessment against the fixture's
// current clock so a test can advance time, recreate a node, drop a report, or
// change the compiled configuration and watch the qualification react.
type qualificationFixture struct {
	now            time.Time
	nodes          []*types.Node
	configDigest   string
	profiles       []config.AcceleratorRuntimeProfile
	missingReports map[string]bool
	mutateReport   func(node string, report *types.AgentAcceleratorReport)
	staleAgents    map[string]bool
	listErr        error
	builderErr     error
	built          []string
}

func qualificationNode(name, uid string) *types.Node {
	return &types.Node{Name: name, UID: uid, Labels: map[string]string{"pool": "a100", "kubeneuron.io/tenant": "team-a", "kubeneuron.io/cluster": "east"}}
}

func newQualificationFixture() *qualificationFixture {
	return &qualificationFixture{
		now:            time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
		nodes:          []*types.Node{qualificationNode("gpu-b", "uid-b"), qualificationNode("gpu-a", "uid-a")},
		configDigest:   "sha256:live-config",
		profiles:       []config.AcceleratorRuntimeProfile{*operationProfile()},
		missingReports: map[string]bool{},
		staleAgents:    map[string]bool{},
	}
}

func (f *qualificationFixture) list(context.Context) ([]*types.Node, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.nodes, nil
}

func (f *qualificationFixture) build(_ context.Context, node string, vendor types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
	if f.builderErr != nil {
		return config.RuntimeContractCoverage{}, f.builderErr
	}
	f.built = append(f.built, node)
	var current *types.Node
	for _, candidate := range f.nodes {
		if candidate != nil && candidate.Name == node {
			current = candidate
		}
	}
	input := config.RuntimeContractCoverageInput{
		Now: f.now, ConfigDigest: f.configDigest, NodeName: node, Vendor: vendor,
		AgentLastSeen: f.now.Add(-time.Minute), AgentMaxAge: 5 * time.Minute, Profiles: f.profiles,
	}
	if current != nil {
		input.NodeUID, input.NodeLabels = current.UID, current.Labels
	}
	if f.staleAgents[node] {
		input.AgentLastSeen = f.now.Add(-input.AgentMaxAge - time.Minute)
	}
	if !f.missingReports[node] {
		report := coverageReportFor(f.now, node)
		report.NodeUID = input.NodeUID
		if f.mutateReport != nil {
			f.mutateReport(node, report)
		}
		input.Report = report
	}
	return config.AssessRuntimeContractCoverage(input), nil
}

func newQualificationManager(t *testing.T) (*Manager, *sqlite.Store, *qualificationFixture) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := newQualificationFixture()
	mgr := New(Options{
		Resources: st, Workflow: st,
		Now:                          func() time.Time { return fixture.now },
		ListNodes:                    fixture.list,
		BuildRuntimeContractCoverage: fixture.build,
	})
	return mgr, st, fixture
}

func qualificationRequest(key string) RuntimeContractQualificationRequest {
	return RuntimeContractQualificationRequest{
		Nodes: []string{"gpu-b", "gpu-a"}, Vendor: types.AcceleratorVendorNVIDIA, Tenant: "team-a", Cluster: "east",
		Requirements:   RuntimeContractQualificationRequirements{MinSamples: 2, MinDuration: 10 * time.Minute},
		ExpiresAt:      time.Date(2026, 9, 5, 13, 0, 0, 0, time.UTC),
		IdempotencyKey: key,
	}
}

func observe(t *testing.T, mgr *Manager, id, key string, version int) *RuntimeContractQualification {
	t.Helper()
	got, replayed, err := mgr.ObserveRuntimeContractQualification(context.Background(), "bob", id, RuntimeContractQualificationMutation{ExpectedVersion: version, IdempotencyKey: key})
	if err != nil || replayed {
		t.Fatalf("observe %s = (%#v, %v, %v)", key, got, replayed, err)
	}
	return got
}

func auditActions(t *testing.T, st *sqlite.Store, id string) string {
	t.Helper()
	events, err := st.ListOperationalAudit(context.Background(), types.ResourceRuntimeContractQualification, id, 100)
	if err != nil {
		t.Fatal(err)
	}
	actions := make([]string, 0, len(events))
	for _, event := range events {
		actions = append(actions, event.Action+"="+event.Result)
	}
	return strings.Join(actions, ",")
}

func TestQualificationCreateFreezesExactBindingAndSortedCohort(t *testing.T) {
	mgr, st, fixture := newQualificationManager(t)
	ctx := context.Background()
	request := qualificationRequest("create-1")
	got, replayed, err := mgr.CreateRuntimeContractQualification(ctx, "alice", request)
	if err != nil || replayed {
		t.Fatalf("create = (%#v, %v, %v)", got, replayed, err)
	}
	profile := operationProfile()
	if got.Version != RuntimeContractQualificationVersion || got.State != QualificationObserving || got.ResourceVersion != 1 || got.Vendor != types.AcceleratorVendorNVIDIA {
		t.Fatalf("qualification envelope = %#v", got)
	}
	if got.Profile != (RuntimeContractQualificationProfile{Name: profile.Name, UID: profile.ProfileUID, Generation: profile.ProfileGeneration, Digest: profile.ProfileDigest}) || got.ConfigDigest != "sha256:live-config" {
		t.Fatalf("frozen bindings = %#v / %q", got.Profile, got.ConfigDigest)
	}
	wantCohort := []RuntimeContractQualificationNode{{Name: "gpu-a", UID: "uid-a"}, {Name: "gpu-b", UID: "uid-b"}}
	if !reflect.DeepEqual(got.Cohort, wantCohort) {
		t.Fatalf("cohort = %#v, want stable name order with frozen UIDs", got.Cohort)
	}
	if !qualificationCoverageFull(got.InitialCoverage) || len(got.InitialCoverage) != 2 || got.InitialCoverage[0].Node != "gpu-a" || got.InitialCoverage[1].NodeUID != "uid-b" {
		t.Fatalf("initial coverage = %#v, want full evidence in cohort order", got.InitialCoverage)
	}
	if got.TotalObservations != 0 || got.SuccessfulSamples != 0 || len(got.Observations) != 0 {
		t.Fatalf("creation must not count as a sample: %#v", got)
	}
	if strings.Join(fixture.built, ",") != "gpu-a,gpu-b" {
		t.Fatalf("coverage built for %v, want the sorted cohort exactly once", fixture.built)
	}
	// Caller-owned request and lister-owned inventory are left untouched.
	if request.Nodes[0] != "gpu-b" || fixture.nodes[0].Name != "gpu-b" || fixture.nodes[1].Name != "gpu-a" {
		t.Fatalf("request nodes %v / inventory %s,%s were mutated", request.Nodes, fixture.nodes[0].Name, fixture.nodes[1].Name)
	}
	// Frozen evidence in the returned value does not alias durable state.
	got.Cohort[0].UID, got.InitialCoverage[0].Reasons = "tampered", append(got.InitialCoverage[0].Reasons, config.RuntimeContractReasonReportMissing)
	stored, err := mgr.GetRuntimeContractQualification(ctx, got.ID)
	if err != nil || stored.Cohort[0].UID != "uid-a" || len(stored.InitialCoverage[0].Reasons) != 0 {
		t.Fatalf("stored qualification = (%#v, %v), want the frozen cohort", stored, err)
	}
	if actions := auditActions(t, st, got.ID); actions != "create=Observing" {
		t.Fatalf("audit = %q", actions)
	}
	events, _ := st.ListOperationalAudit(ctx, types.ResourceRuntimeContractQualification, got.ID, 1)
	if events[0].Params["profile_uid"] != "profile-uid" || events[0].Params["initially_full"] != "true" || events[0].Params["cohort_size"] != "2" {
		t.Fatalf("create audit params = %v", events[0].Params)
	}
}

func TestQualificationCreateRejectsInvalidRequestsBeforeReservingKey(t *testing.T) {
	mgr, st, fixture := newQualificationManager(t)
	ctx := context.Background()
	base := qualificationRequest("bad-request")
	cases := map[string]struct {
		mutate func(*RuntimeContractQualificationRequest)
		want   string
		is     error
	}{
		"no nodes":         {func(r *RuntimeContractQualificationRequest) { r.Nodes = nil }, "at least one node", nil},
		"blank node":       {func(r *RuntimeContractQualificationRequest) { r.Nodes = []string{"gpu-a", " "} }, "must not be blank", nil},
		"duplicate node":   {func(r *RuntimeContractQualificationRequest) { r.Nodes = []string{"gpu-a", " gpu-a"} }, "more than once", nil},
		"invalid vendor":   {func(r *RuntimeContractQualificationRequest) { r.Vendor = "cuda" }, "vendor must be", nil},
		"zero samples":     {func(r *RuntimeContractQualificationRequest) { r.Requirements.MinSamples = 0 }, "min_samples", nil},
		"too many samples": {func(r *RuntimeContractQualificationRequest) { r.Requirements.MinSamples = maxQualificationSamples + 1 }, "min_samples", nil},
		"zero duration":    {func(r *RuntimeContractQualificationRequest) { r.Requirements.MinDuration = 0 }, "min_duration", nil},
		"long duration": {func(r *RuntimeContractQualificationRequest) {
			r.Requirements.MinDuration = maxQualificationDuration + time.Second
		}, "min_duration", nil},
		"missing expiry":   {func(r *RuntimeContractQualificationRequest) { r.ExpiresAt = time.Time{} }, "expiry is required", nil},
		"expiry in window": {func(r *RuntimeContractQualificationRequest) { r.ExpiresAt = fixture.now.Add(10 * time.Minute) }, "earliest possible ready time", nil},
		"expiry not safe": {func(r *RuntimeContractQualificationRequest) {
			r.ExpiresAt = fixture.now.Add(14*time.Minute + 59*time.Second)
		}, "earliest possible ready time", nil},
		"expiry too far":    {func(r *RuntimeContractQualificationRequest) { r.ExpiresAt = fixture.now.Add(31 * 24 * time.Hour) }, "exceeds", nil},
		"unknown node":      {func(r *RuntimeContractQualificationRequest) { r.Nodes = []string{"gpu-a", "gpu-z"} }, "not in the managed inventory", ErrIncompleteInventory},
		"out of scope node": {func(r *RuntimeContractQualificationRequest) { r.Tenant = "team-b" }, "outside requested tenant/cluster scope", ErrInvalidState},
		"blank actor":       {func(r *RuntimeContractQualificationRequest) { r.IdempotencyKey = " " }, "idempotency key are required", nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			request := base
			request.Nodes = append([]string(nil), base.Nodes...)
			tc.mutate(&request)
			got, replayed, err := mgr.CreateRuntimeContractQualification(ctx, "alice", request)
			if err == nil || got != nil || replayed || !strings.Contains(err.Error(), tc.want) || (tc.is != nil && !errors.Is(err, tc.is)) {
				t.Fatalf("create = (%#v, %v, %v), want error containing %q", got, replayed, err, tc.want)
			}
		})
	}
	if _, err := st.GetOperationalIdempotency(ctx, types.ResourceRuntimeContractQualification, "alice", scopedIdempotencyKey("create", "bad-request")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("idempotency lookup = %v, want ErrNotFound: rejected requests must not reserve the key", err)
	}
	items, err := mgr.ListRuntimeContractQualifications(ctx, OperationalListOptions{})
	if err != nil || len(items) != 0 {
		t.Fatalf("durable qualifications after rejections = (%v, %v), want none", items, err)
	}
	if _, _, err := mgr.CreateRuntimeContractQualification(ctx, " ", base); err == nil || !strings.Contains(err.Error(), "actor") {
		t.Fatalf("blank actor error = %v", err)
	}
	// The same key is usable once the request is valid.
	got, replayed, err := mgr.CreateRuntimeContractQualification(ctx, "alice", base)
	if err != nil || replayed || got == nil {
		t.Fatalf("recovered create = (%#v, %v, %v)", got, replayed, err)
	}
}

func TestQualificationCreateRejectsInventoryAndBindingFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("incomplete inventory is rejected before coverage", func(t *testing.T) {
		// A padded UID is never trimmed into a frozen identity: the platform
		// did not report "uid-b", so freezing it would bind a node that does
		// not exist and freezing the padded value would bind one nothing
		// canonical can ever match again.
		for name, nodes := range map[string][]*types.Node{
			"nil entry":        {qualificationNode("gpu-a", "uid-a"), nil, qualificationNode("gpu-b", "uid-b")},
			"blank name":       {qualificationNode("gpu-a", "uid-a"), {Name: " "}, qualificationNode("gpu-b", "uid-b")},
			"duplicate name":   {qualificationNode("gpu-a", "uid-a"), qualificationNode("gpu-b", "uid-b"), qualificationNode("gpu-a", "uid-a2")},
			"blank uid":        {qualificationNode("gpu-a", "uid-a"), qualificationNode("gpu-b", " ")},
			"empty uid":        {qualificationNode("gpu-a", "uid-a"), qualificationNode("gpu-b", "")},
			"leading pad uid":  {qualificationNode("gpu-a", "uid-a"), qualificationNode("gpu-b", " uid-b")},
			"trailing tab uid": {qualificationNode("gpu-a", "uid-a\t"), qualificationNode("gpu-b", "uid-b")},
			"newline uid":      {qualificationNode("gpu-a", "uid-a"), qualificationNode("gpu-b", "uid-b\n")},
		} {
			mgr, st, fixture := newQualificationManager(t)
			fixture.nodes = nodes
			got, replayed, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest(name))
			if !errors.Is(err, ErrIncompleteInventory) || got != nil || replayed {
				t.Fatalf("%s: create = (%#v, %v, %v), want ErrIncompleteInventory", name, got, replayed, err)
			}
			if len(fixture.built) != 0 {
				t.Fatalf("%s: built coverage for %v before rejecting the inventory", name, fixture.built)
			}
			if _, lookupErr := st.GetOperationalIdempotency(ctx, types.ResourceRuntimeContractQualification, "alice", scopedIdempotencyKey("create", name)); !errors.Is(lookupErr, store.ErrNotFound) {
				t.Fatalf("%s: idempotency lookup = %v, want ErrNotFound", name, lookupErr)
			}
			if items, listErr := mgr.ListRuntimeContractQualifications(ctx, OperationalListOptions{IncludeExpired: true}); listErr != nil || len(items) != 0 {
				t.Fatalf("%s: durable qualifications = (%v, %v), want none", name, items, listErr)
			}
		}
	})
	t.Run("lister and builder errors fail closed", func(t *testing.T) {
		mgr, _, fixture := newQualificationManager(t)
		boom := errors.New("informer not synced")
		fixture.listErr = boom
		if _, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("lister")); !errors.Is(err, boom) {
			t.Fatalf("lister error = %v, want %v", err, boom)
		}
		fixture.listErr, fixture.builderErr = nil, errors.New("report store read failed")
		if _, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("builder")); !errors.Is(err, fixture.builderErr) || !strings.Contains(err.Error(), `node "gpu-a"`) {
			t.Fatalf("builder error = %v, want the store error naming the node", err)
		}
	})
	t.Run("missing dependencies are unavailable", func(t *testing.T) {
		st, err := sqlite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		fixture := newQualificationFixture()
		for name, mgr := range map[string]*Manager{
			"nil manager":  nil,
			"no store":     New(Options{ListNodes: fixture.list, BuildRuntimeContractCoverage: fixture.build}),
			"no lister":    New(Options{Resources: st, BuildRuntimeContractCoverage: fixture.build}),
			"no builder":   New(Options{Resources: st, ListNodes: fixture.list}),
			"only a store": New(Options{Resources: st}),
		} {
			if _, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest(name)); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%s: create error = %v, want ErrUnavailable", name, err)
			}
			if _, _, err := mgr.ObserveRuntimeContractQualification(ctx, "alice", "rcq-x", RuntimeContractQualificationMutation{IdempotencyKey: name}); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%s: observe error = %v, want ErrUnavailable", name, err)
			}
		}
	})
	t.Run("non-exact selection is rejected", func(t *testing.T) {
		mgr, _, fixture := newQualificationManager(t)
		fixture.nodes[0].Labels = map[string]string{"pool": "h100", "kubeneuron.io/tenant": "team-a", "kubeneuron.io/cluster": "east"}
		_, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("uncovered"))
		if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "Uncovered profile selection") {
			t.Fatalf("uncovered error = %v", err)
		}
	})
	t.Run("cohort spanning two profiles is rejected", func(t *testing.T) {
		mgr, _, fixture := newQualificationManager(t)
		second := *operationProfile()
		second.Name, second.ProfileUID, second.NodeSelector = "nvidia-a100-b", "profile-uid-b", map[string]string{"pool": "a100-b"}
		fixture.profiles = append(fixture.profiles, second)
		fixture.nodes[0].Labels["pool"] = "a100-b"
		_, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("split"))
		if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "cohort must share profile") {
			t.Fatalf("split cohort error = %v", err)
		}
	})
	t.Run("blank config digest is rejected", func(t *testing.T) {
		mgr, _, fixture := newQualificationManager(t)
		fixture.configDigest = ""
		if _, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("nodigest")); !errors.Is(err, ErrIncompleteInventory) {
			t.Fatalf("blank config digest error = %v, want ErrIncompleteInventory", err)
		}
	})
	t.Run("builder answering for another node is rejected", func(t *testing.T) {
		mgr, _, _ := newQualificationManager(t)
		mgr.buildRuntimeContractCoverage = func(_ context.Context, node string, vendor types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
			return config.RuntimeContractCoverage{NodeName: "other", Vendor: vendor, Selection: config.RuntimeContractSelectionExact}, nil
		}
		if _, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("wrong-node")); !errors.Is(err, ErrIncompleteInventory) {
			t.Fatalf("wrong node error = %v, want ErrIncompleteInventory", err)
		}
	})
}

// A builder whose answer cannot be attributed to the frozen node, or whose
// profile identity is missing or pads an axis a later Observe compares, is an
// inconsistent read: creation must fail closed before reserving the key, and
// never trim a padded axis into a frozen identity. On Observe a node UID that
// differs in any raw way from the frozen one is an error before writing, never
// drift or a sample; profile and config axes are compared exactly against the
// canonical frozen values, so a padded value is binding drift, never a match.
func TestQualificationRejectsInconsistentCoverageIdentity(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		corrupt        func(*config.RuntimeContractCoverage)
		driftOnObserve bool
	}{
		"blank node uid":         {corrupt: func(c *config.RuntimeContractCoverage) { c.NodeUID = "" }},
		"whitespace node uid":    {corrupt: func(c *config.RuntimeContractCoverage) { c.NodeUID = "   " }},
		"different node uid":     {corrupt: func(c *config.RuntimeContractCoverage) { c.NodeUID = c.NodeUID + "-reborn" }},
		"leading pad node uid":   {corrupt: func(c *config.RuntimeContractCoverage) { c.NodeUID = " " + c.NodeUID }},
		"trailing pad node uid":  {corrupt: func(c *config.RuntimeContractCoverage) { c.NodeUID = c.NodeUID + "\t" }},
		"zero generation":        {corrupt: func(c *config.RuntimeContractCoverage) { c.ProfileGeneration = 0 }, driftOnObserve: true},
		"negative generation":    {corrupt: func(c *config.RuntimeContractCoverage) { c.ProfileGeneration = -1 }, driftOnObserve: true},
		"whitespace digest":      {corrupt: func(c *config.RuntimeContractCoverage) { c.ProfileDigest = " \t" }, driftOnObserve: true},
		"padded digest":          {corrupt: func(c *config.RuntimeContractCoverage) { c.ProfileDigest = " " + c.ProfileDigest + " " }, driftOnObserve: true},
		"blank profile uid":      {corrupt: func(c *config.RuntimeContractCoverage) { c.ProfileUID = "" }, driftOnObserve: true},
		"padded profile uid":     {corrupt: func(c *config.RuntimeContractCoverage) { c.ProfileUID = c.ProfileUID + "\n" }, driftOnObserve: true},
		"blank profile name":     {corrupt: func(c *config.RuntimeContractCoverage) { c.ProfileName = " " }, driftOnObserve: true},
		"padded profile name":    {corrupt: func(c *config.RuntimeContractCoverage) { c.ProfileName = "\t" + c.ProfileName }, driftOnObserve: true},
		"whitespace config hash": {corrupt: func(c *config.RuntimeContractCoverage) { c.ConfigDigest = " " }, driftOnObserve: true},
		"padded config hash":     {corrupt: func(c *config.RuntimeContractCoverage) { c.ConfigDigest = c.ConfigDigest + " " }, driftOnObserve: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mgr, st, fixture := newQualificationManager(t)
			corrupting := false
			mgr.buildRuntimeContractCoverage = func(ctx context.Context, node string, vendor types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
				coverage, err := fixture.build(ctx, node, vendor)
				if corrupting && node == "gpu-b" {
					tc.corrupt(&coverage)
				}
				return coverage, err
			}
			corrupting = true
			got, replayed, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("identity"))
			if !errors.Is(err, ErrIncompleteInventory) || got != nil || replayed {
				t.Fatalf("create = (%#v, %v, %v), want ErrIncompleteInventory", got, replayed, err)
			}
			if _, lookupErr := st.GetOperationalIdempotency(ctx, types.ResourceRuntimeContractQualification, "alice", scopedIdempotencyKey("create", "identity")); !errors.Is(lookupErr, store.ErrNotFound) {
				t.Fatalf("idempotency lookup = %v, want ErrNotFound", lookupErr)
			}
			if items, listErr := mgr.ListRuntimeContractQualifications(ctx, OperationalListOptions{IncludeExpired: true}); listErr != nil || len(items) != 0 {
				t.Fatalf("durable qualifications = (%v, %v), want none", items, listErr)
			}
			// A consistent read with the same key then succeeds and freezes the
			// canonical inventory UID and profile/config identity, which the
			// coverage evidence must echo exactly.
			corrupting = false
			created, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("identity"))
			if err != nil || created.InitialCoverage[1].NodeUID != "uid-b" || created.Profile.Generation != 1 {
				t.Fatalf("recovered create = (%#v, %v)", created, err)
			}
			profile := operationProfile()
			if created.Profile != (RuntimeContractQualificationProfile{Name: profile.Name, UID: profile.ProfileUID, Generation: 1, Digest: profile.ProfileDigest}) || created.ConfigDigest != "sha256:live-config" {
				t.Fatalf("frozen identity = %#v / %q, want the exact canonical values", created.Profile, created.ConfigDigest)
			}
			// The same inconsistency during Observe is an error, not drift.
			corrupting = true
			fixture.now = fixture.now.Add(time.Minute)
			observed, replayed, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{IdempotencyKey: "identity-observe"})
			if tc.driftOnObserve {
				// Profile and config axes are compared exactly, not validated, on
				// Observe: a changed or padded identity can never equal the
				// canonical frozen one, so it is binding drift and is recorded as
				// such while the frozen bindings stay untouched.
				if err != nil || replayed || observed.State != QualificationInvalidated || len(observed.Observations) != 1 || len(observed.Observations[0].Drift) == 0 {
					t.Fatalf("observe with changed profile identity = (%#v, %v, %v), want Invalidated with drift", observed, replayed, err)
				}
				if observed.Profile != created.Profile || observed.ConfigDigest != created.ConfigDigest {
					t.Fatalf("frozen bindings changed on drift: %#v", observed)
				}
				return
			}
			if !errors.Is(err, ErrIncompleteInventory) || observed != nil || replayed {
				t.Fatalf("observe = (%#v, %v, %v), want ErrIncompleteInventory", observed, replayed, err)
			}
			stored, getErr := mgr.GetRuntimeContractQualification(ctx, created.ID)
			if getErr != nil || stored.ResourceVersion != 1 || stored.TotalObservations != 0 || stored.State != QualificationObserving {
				t.Fatalf("stored after inconsistent observe = (%#v, %v), want untouched", stored, getErr)
			}
			if _, lookupErr := st.GetOperationalIdempotency(ctx, types.ResourceRuntimeContractQualification, "bob", scopedIdempotencyKey("observe", "identity-observe")); !errors.Is(lookupErr, store.ErrNotFound) {
				t.Fatalf("observe idempotency lookup = %v, want ErrNotFound", lookupErr)
			}
		})
	}
}

// Scope is derived from controller-owned labels and frozen exactly. A request
// may assert a scope, but it can neither relabel a cohort nor bypass the
// cross-scope check by omitting the assertion.
func TestQualificationScopeIsDerivedFromLabelsAndFrozenExactly(t *testing.T) {
	ctx := context.Background()
	unscoped := func(key string) RuntimeContractQualificationRequest {
		request := qualificationRequest(key)
		request.Tenant, request.Cluster = "", ""
		return request
	}
	t.Run("omitted request scope is derived from labels", func(t *testing.T) {
		mgr, st, _ := newQualificationManager(t)
		got, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", unscoped("derived"))
		if err != nil || got.Tenant != "team-a" || got.Cluster != "east" {
			t.Fatalf("create = (%#v, %v), want tenant team-a cluster east derived from labels", got, err)
		}
		envelope, err := st.GetOperationalResource(ctx, types.ResourceRuntimeContractQualification, got.ID)
		if err != nil || envelope.Tenant != "team-a" || envelope.Cluster != "east" {
			t.Fatalf("envelope = (%#v, %v), want derived scope persisted", envelope, err)
		}
		events, _ := st.ListOperationalAudit(ctx, types.ResourceRuntimeContractQualification, got.ID, 1)
		if events[0].Params["tenant"] != "team-a" || events[0].Params["cluster"] != "east" || events[0].Tenant != "team-a" {
			t.Fatalf("create audit = %#v, want derived scope", events[0])
		}
		listed, err := mgr.ListRuntimeContractQualifications(ctx, OperationalListOptions{Tenant: "team-a", Cluster: "east", IncludeExpired: true})
		if err != nil || len(listed) != 1 {
			t.Fatalf("scoped list = (%d, %v), want the derived-scope qualification", len(listed), err)
		}
	})
	t.Run("unlabeled cohort freezes an empty scope", func(t *testing.T) {
		mgr, _, fixture := newQualificationManager(t)
		for _, node := range fixture.nodes {
			delete(node.Labels, "kubeneuron.io/tenant")
			delete(node.Labels, "kubeneuron.io/cluster")
		}
		got, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", unscoped("unlabeled"))
		if err != nil || got.Tenant != "" || got.Cluster != "" {
			t.Fatalf("create = (%#v, %v), want empty frozen scope", got, err)
		}
		if _, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("asserted")); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("asserting a scope over unlabeled nodes = %v, want ErrInvalidState", err)
		}
	})
	t.Run("cross-scope cohort is rejected even without a request scope", func(t *testing.T) {
		for name, relabel := range map[string]func(*types.Node){
			"other tenant":   func(n *types.Node) { n.Labels["kubeneuron.io/tenant"] = "team-b" },
			"other cluster":  func(n *types.Node) { n.Labels["kubeneuron.io/cluster"] = "west" },
			"missing tenant": func(n *types.Node) { delete(n.Labels, "kubeneuron.io/tenant") },
			"empty cluster":  func(n *types.Node) { n.Labels["kubeneuron.io/cluster"] = "" },
		} {
			mgr, st, fixture := newQualificationManager(t)
			relabel(fixture.nodes[0])
			got, replayed, err := mgr.CreateRuntimeContractQualification(ctx, "alice", unscoped("split-"+name))
			if !errors.Is(err, ErrInvalidState) || got != nil || replayed || !strings.Contains(err.Error(), "cohort must share the scope") {
				t.Fatalf("%s: create = (%#v, %v, %v), want ErrInvalidState for a cross-scope cohort", name, got, replayed, err)
			}
			if len(fixture.built) != 0 {
				t.Fatalf("%s: built coverage for %v before rejecting the cohort scope", name, fixture.built)
			}
			if _, lookupErr := st.GetOperationalIdempotency(ctx, types.ResourceRuntimeContractQualification, "alice", scopedIdempotencyKey("create", "split-"+name)); !errors.Is(lookupErr, store.ErrNotFound) {
				t.Fatalf("%s: idempotency lookup = %v, want ErrNotFound", name, lookupErr)
			}
		}
	})
	t.Run("explicit request scope must match every label", func(t *testing.T) {
		mgr, _, _ := newQualificationManager(t)
		for name, mutate := range map[string]func(*RuntimeContractQualificationRequest){
			"tenant":            func(r *RuntimeContractQualificationRequest) { r.Tenant = "team-b" },
			"cluster":           func(r *RuntimeContractQualificationRequest) { r.Cluster = "west" },
			"tenant only wrong": func(r *RuntimeContractQualificationRequest) { r.Cluster, r.Tenant = "", "Team-A" },
		} {
			request := qualificationRequest("explicit-" + name)
			mutate(&request)
			if _, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", request); !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "outside requested tenant/cluster scope") {
				t.Fatalf("%s: create = %v, want ErrInvalidState", name, err)
			}
		}
		// A partial assertion that agrees with the labels still persists the
		// full derived scope.
		request := qualificationRequest("partial")
		request.Cluster = ""
		got, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", request)
		if err != nil || got.Tenant != "team-a" || got.Cluster != "east" {
			t.Fatalf("partial assertion create = (%#v, %v)", got, err)
		}
	})
	t.Run("label mutation after create is drift", func(t *testing.T) {
		cases := map[string]struct {
			prepare func(*qualificationFixture)
			mutate  func(*qualificationFixture)
			want    string
		}{
			"tenant label removed": {
				mutate: func(f *qualificationFixture) { delete(f.nodes[1].Labels, "kubeneuron.io/tenant") },
				want:   `node "gpu-a" tenant/cluster labels changed from "team-a"/"east" to ""/"east"`,
			},
			"cluster label changed": {
				mutate: func(f *qualificationFixture) { f.nodes[1].Labels["kubeneuron.io/cluster"] = "west" },
				want:   `node "gpu-a" tenant/cluster labels changed from "team-a"/"east" to "team-a"/"west"`,
			},
			"tenant label added to unscoped cohort": {
				prepare: func(f *qualificationFixture) {
					for _, node := range f.nodes {
						delete(node.Labels, "kubeneuron.io/tenant")
						delete(node.Labels, "kubeneuron.io/cluster")
					}
				},
				mutate: func(f *qualificationFixture) { f.nodes[0].Labels["kubeneuron.io/tenant"] = "team-a" },
				want:   `node "gpu-b" tenant/cluster labels changed from ""/"" to "team-a"/""`,
			},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				mgr, st, fixture := newQualificationManager(t)
				if tc.prepare != nil {
					tc.prepare(fixture)
				}
				created, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", unscoped("labels"))
				if err != nil {
					t.Fatal(err)
				}
				fixture.now = fixture.now.Add(time.Minute)
				healthy := observe(t, mgr, created.ID, "l1", created.ResourceVersion)
				if healthy.SuccessfulSamples != 1 {
					t.Fatalf("baseline = %#v", healthy)
				}
				tc.mutate(fixture)
				fixture.now = fixture.now.Add(time.Minute)
				fixture.built = nil
				got := observe(t, mgr, created.ID, "l2", healthy.ResourceVersion)
				if got.State != QualificationInvalidated || got.InvalidationReason != tc.want || got.Tenant != created.Tenant || got.Cluster != created.Cluster {
					t.Fatalf("after label mutation = %#v, want Invalidated with %q and unchanged frozen scope", got, tc.want)
				}
				if len(fixture.built) != 0 {
					t.Fatalf("built coverage %v; scope drift must be decided before coverage", fixture.built)
				}
				if actions := auditActions(t, st, created.ID); actions != "create=Observing,observe=full,observe=drift,invalidate=Invalidated" {
					t.Fatalf("audit = %q", actions)
				}
			})
		}
	})
}

// Readiness never outlives its evidence: once ReadyForApproval, a non-Full
// observation with intact bindings invalidates permanently. The same
// observation before readiness is recorded, does not count, and leaves the
// qualification Observing.
func TestQualificationReadyRegressesToInvalidatedOnNonFullEvidence(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		degrade func(*qualificationFixture)
		want    string
	}{
		"missing report":  {func(f *qualificationFixture) { f.missingReports["gpu-b"] = true }, `node "gpu-b" attestation Missing`},
		"stale heartbeat": {func(f *qualificationFixture) { f.staleAgents["gpu-a"] = true }, `node "gpu-a" attestation`},
		"driver mismatch": {func(f *qualificationFixture) {
			f.mutateReport = func(node string, report *types.AgentAcceleratorReport) {
				if node == "gpu-a" {
					report.DriverVersion = "535.0.0"
				}
			}
		}, `node "gpu-a" attestation Mismatch`},
		"stale report": {func(f *qualificationFixture) {
			f.mutateReport = func(_ string, report *types.AgentAcceleratorReport) {
				report.ObservedAt = f.now.Add(-time.Hour)
			}
		}, "attestation Stale"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mgr, st, fixture := newQualificationManager(t)
			request := qualificationRequest("regress")
			request.Requirements = RuntimeContractQualificationRequirements{MinSamples: 1, MinDuration: time.Minute}
			created, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", request)
			if err != nil {
				t.Fatal(err)
			}
			// Before readiness the degraded evidence is recorded, does not count,
			// and does not invalidate.
			tc.degrade(fixture)
			fixture.now = fixture.now.Add(time.Minute)
			early := observe(t, mgr, created.ID, "r0", created.ResourceVersion)
			if early.State != QualificationObserving || early.SuccessfulSamples != 0 || early.Observations[0].Successful || len(early.Observations[0].Degraded) == 0 || len(early.Observations[0].Drift) != 0 {
				t.Fatalf("pre-ready degraded observation = %#v, want Observing, uncounted, with degraded reasons", early)
			}
			// Restore full evidence (a fresh fixture has the same nodes, UIDs,
			// labels, and profile) and become ready.
			now := fixture.now
			*fixture = *newQualificationFixture()
			fixture.now = now.Add(time.Minute)
			first := observe(t, mgr, created.ID, "r1", early.ResourceVersion)
			fixture.now = fixture.now.Add(time.Minute)
			ready := observe(t, mgr, created.ID, "r2", first.ResourceVersion)
			if ready.State != QualificationReadyForApproval || ready.SuccessfulSamples != 2 {
				t.Fatalf("ready = %#v", ready)
			}
			// After readiness the same degradation is fatal.
			tc.degrade(fixture)
			fixture.now = fixture.now.Add(time.Minute)
			got := observe(t, mgr, created.ID, "r3", ready.ResourceVersion)
			if got.State != QualificationInvalidated || got.InvalidatedAt == nil || !got.InvalidatedAt.Equal(fixture.now) || got.ReadyAt == nil {
				t.Fatalf("post-ready degraded observation = %#v, want Invalidated while keeping ReadyAt as history", got)
			}
			if !strings.HasPrefix(got.InvalidationReason, "ready qualification regressed to non-Full coverage: ") || !strings.Contains(got.InvalidationReason, tc.want) {
				t.Fatalf("invalidation reason = %q, want a regression reason containing %q", got.InvalidationReason, tc.want)
			}
			last := got.Observations[len(got.Observations)-1]
			if last.Successful || len(last.Drift) != 0 || len(last.Degraded) == 0 || len(last.Coverage) != 2 || got.SuccessfulSamples != 2 || got.TotalObservations != 4 {
				t.Fatalf("regression observation = %#v", got)
			}
			if actions := auditActions(t, st, created.ID); actions != "create=Observing,observe=not-full,observe=full,observe=full,ready=ReadyForApproval,observe=not-full,invalidate=Invalidated" {
				t.Fatalf("audit = %q", actions)
			}
			events, _ := st.ListOperationalAudit(ctx, types.ResourceRuntimeContractQualification, created.ID, 100)
			if reason := events[len(events)-1].Params["reason"]; reason != got.InvalidationReason {
				t.Fatalf("invalidate audit reason = %q, want %q", reason, got.InvalidationReason)
			}
			// Permanent: restored evidence does not revive readiness.
			*fixture = *newQualificationFixture()
			fixture.now = now.Add(time.Hour)
			if _, _, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{IdempotencyKey: "r4"}); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("observe after regression = %v, want ErrInvalidState", err)
			}
			if stored, _ := mgr.GetRuntimeContractQualification(ctx, created.ID); stored.State != QualificationInvalidated || stored.ResourceVersion != 5 {
				t.Fatalf("stored = %#v", stored)
			}
		})
	}
}

func TestQualificationInitialMissingOrMismatchedCoverageObservesButNeverQualifies(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(*qualificationFixture){
		"missing report": func(f *qualificationFixture) { f.missingReports["gpu-b"] = true },
		"driver mismatch": func(f *qualificationFixture) {
			f.mutateReport = func(node string, report *types.AgentAcceleratorReport) {
				if node == "gpu-a" {
					report.DriverVersion = "535.0.0"
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			mgr, _, fixture := newQualificationManager(t)
			setup(fixture)
			got, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("partial"))
			if err != nil || got.State != QualificationObserving || qualificationCoverageFull(got.InitialCoverage) {
				t.Fatalf("create = (%#v, %v), want Observing with non-full initial evidence", got, err)
			}
			for index, step := range []time.Duration{time.Minute, 2 * time.Minute, 11 * time.Minute, 30 * time.Minute} {
				fixture.now = fixture.now.Add(step)
				got = observe(t, mgr, got.ID, "poll-"+string(rune('a'+index)), got.ResourceVersion)
				if got.State != QualificationObserving || got.SuccessfulSamples != 0 || got.FirstSuccessfulSampleAt != nil || got.ReadyAt != nil {
					t.Fatalf("after %d polls: %#v, want Observing with zero successful samples", index+1, got)
				}
				if got.TotalObservations != index+1 || len(got.Observations) != index+1 || got.Observations[index].Successful || len(got.Observations[index].Drift) != 0 {
					t.Fatalf("observations after %d polls = %#v, want each non-full sample recorded without drift", index+1, got.Observations)
				}
			}
		})
	}
}

func TestQualificationBecomesReadyOnlyAfterBothSampleCountAndDuration(t *testing.T) {
	mgr, st, fixture := newQualificationManager(t)
	ctx := context.Background()
	created, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("ready"))
	if err != nil {
		t.Fatal(err)
	}
	start := fixture.now
	fixture.now = start.Add(time.Minute)
	first := observe(t, mgr, created.ID, "o1", created.ResourceVersion)
	if first.State != QualificationObserving || first.SuccessfulSamples != 1 || first.FirstSuccessfulSampleAt == nil || !first.FirstSuccessfulSampleAt.Equal(fixture.now) || first.ResourceVersion != 2 {
		t.Fatalf("first sample = %#v, want one successful sample at %s", first, fixture.now)
	}
	// Sample count satisfied, duration not: still Observing.
	fixture.now = start.Add(2 * time.Minute)
	second := observe(t, mgr, created.ID, "o2", first.ResourceVersion)
	if second.State != QualificationObserving || second.SuccessfulSamples != 2 || second.ReadyAt != nil {
		t.Fatalf("count-only sample = %#v, want Observing", second)
	}
	// A non-full sample in between is recorded and does not reset progress.
	fixture.now = start.Add(5 * time.Minute)
	fixture.missingReports["gpu-a"] = true
	third := observe(t, mgr, created.ID, "o3", second.ResourceVersion)
	if third.State != QualificationObserving || third.SuccessfulSamples != 2 || third.TotalObservations != 3 || third.Observations[2].Successful || third.Observations[2].Coverage[0].Attestation != config.RuntimeContractAttestationMissing {
		t.Fatalf("non-full sample = %#v, want recorded without counting", third)
	}
	delete(fixture.missingReports, "gpu-a")
	// Duration measured from the first successful sample (start+1m), so at
	// start+10m only 9 minutes have elapsed.
	fixture.now = start.Add(10 * time.Minute)
	fourth := observe(t, mgr, created.ID, "o4", third.ResourceVersion)
	if fourth.State != QualificationObserving || fourth.SuccessfulSamples != 3 {
		t.Fatalf("duration-short sample = %#v, want Observing", fourth)
	}
	fixture.now = start.Add(11 * time.Minute)
	ready := observe(t, mgr, created.ID, "o5", fourth.ResourceVersion)
	if ready.State != QualificationReadyForApproval || ready.ReadyAt == nil || !ready.ReadyAt.Equal(fixture.now) || ready.SuccessfulSamples != 4 || ready.ResourceVersion != 6 {
		t.Fatalf("ready = %#v", ready)
	}
	if actions := auditActions(t, st, created.ID); actions != "create=Observing,observe=full,observe=full,observe=not-full,observe=full,observe=full,ready=ReadyForApproval" {
		t.Fatalf("audit = %q", actions)
	}
	stored, err := mgr.GetRuntimeContractQualification(ctx, created.ID)
	if err != nil || stored.State != QualificationReadyForApproval || stored.ResourceVersion != 6 || !reflect.DeepEqual(stored.Cohort, created.Cohort) || stored.Profile != created.Profile {
		t.Fatalf("stored ready = (%#v, %v)", stored, err)
	}
	if got, _ := st.GetOperationalResource(ctx, types.ResourceRuntimeContractQualification, created.ID); got.State != string(QualificationReadyForApproval) || got.ConfigDigest != created.ConfigDigest || got.Tenant != "team-a" {
		t.Fatalf("envelope = %#v", got)
	}

	// Ready is evidence, not authority: nothing else in the system changed.
	plans, err := mgr.ListAutonomyPlans(ctx, 10)
	if err != nil || len(plans) != 0 {
		t.Fatalf("autonomy plans = (%v, %v), want none", plans, err)
	}
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil || len(incidents) != 0 {
		t.Fatalf("incidents = (%v, %v), want none", incidents, err)
	}
	events, err := mgr.ListAuditEvents(ctx, types.OperationalAuditFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != types.ResourceRuntimeContractQualification {
			t.Fatalf("audit touched %s/%s; a qualification must not reach any other resource", event.Kind, event.ResourceID)
		}
	}
	// A ready qualification keeps sampling but never advances further, and
	// expiry still overrides readiness.
	fixture.now = start.Add(12 * time.Minute)
	still := observe(t, mgr, created.ID, "o6", ready.ResourceVersion)
	if still.State != QualificationReadyForApproval || still.SuccessfulSamples != 5 {
		t.Fatalf("post-ready sample = %#v", still)
	}
	fixture.now = created.ExpiresAt
	expired := observe(t, mgr, created.ID, "o7", still.ResourceVersion)
	if expired.State != QualificationExpired || expired.ExpiredAt == nil || expired.TotalObservations != 6 {
		t.Fatalf("expired after ready = %#v", expired)
	}
}

func TestQualificationExpiryNeverBecomesReady(t *testing.T) {
	mgr, st, fixture := newQualificationManager(t)
	ctx := context.Background()
	request := qualificationRequest("expire")
	request.Requirements = RuntimeContractQualificationRequirements{MinSamples: 1, MinDuration: time.Minute}
	request.ExpiresAt = fixture.now.Add(20 * time.Minute)
	created, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.now = fixture.now.Add(time.Minute)
	first := observe(t, mgr, created.ID, "e1", created.ResourceVersion)
	if first.SuccessfulSamples != 1 || first.State != QualificationObserving {
		t.Fatalf("first = %#v", first)
	}
	// Coverage is still Full at expiry, but the clock decides first and no
	// coverage is even built.
	fixture.now = request.ExpiresAt
	fixture.built = nil
	expired := observe(t, mgr, created.ID, "e2", first.ResourceVersion)
	if expired.State != QualificationExpired || expired.ReadyAt != nil || expired.ExpiredAt == nil || expired.SuccessfulSamples != 1 || expired.TotalObservations != 1 {
		t.Fatalf("expired = %#v, want Expired without a new sample", expired)
	}
	if len(fixture.built) != 0 {
		t.Fatalf("built coverage %v for an expired qualification", fixture.built)
	}
	if actions := auditActions(t, st, created.ID); actions != "create=Observing,observe=full,expire=Expired" {
		t.Fatalf("audit = %q", actions)
	}
	_, _, err = mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{IdempotencyKey: "e3"})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("observe after expiry = %v, want ErrInvalidState", err)
	}
	listed, err := mgr.ListRuntimeContractQualifications(ctx, OperationalListOptions{IncludeExpired: true, State: string(QualificationExpired)})
	if err != nil || len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("expired list = (%v, %v)", listed, err)
	}
}

func TestQualificationDriftInvalidatesPermanently(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		drift        func(*qualificationFixture)
		want         string
		coverageSeen bool
	}{
		"node recreated": {func(f *qualificationFixture) { f.nodes[1] = qualificationNode("gpu-a", "uid-a-reborn") }, `node "gpu-a" uid changed`, false},
		"node removed":   {func(f *qualificationFixture) { f.nodes = f.nodes[:1] }, "no longer in the managed inventory", false},
		"scope drift":    {func(f *qualificationFixture) { f.nodes[0].Labels["kubeneuron.io/tenant"] = "team-b" }, `node "gpu-b" tenant/cluster labels changed from "team-a"/"east" to "team-b"/"east"`, false},
		"config drift":   {func(f *qualificationFixture) { f.configDigest = "sha256:new-config" }, "config digest changed", true},
		"profile drift": {func(f *qualificationFixture) {
			f.profiles[0].ProfileGeneration = 2
			f.mutateReport = func(_ string, report *types.AgentAcceleratorReport) { report.ProfileGeneration = 2 }
		}, "now binds profile", true},
		"selection drift": {func(f *qualificationFixture) { f.nodes[0].Labels["pool"] = "h100" }, "no longer Exact", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mgr, st, fixture := newQualificationManager(t)
			created, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("drift"))
			if err != nil {
				t.Fatal(err)
			}
			fixture.now = fixture.now.Add(time.Minute)
			healthy := observe(t, mgr, created.ID, "d1", created.ResourceVersion)
			if healthy.SuccessfulSamples != 1 {
				t.Fatalf("baseline = %#v", healthy)
			}
			tc.drift(fixture)
			fixture.now = fixture.now.Add(time.Minute)
			fixture.built = nil
			got := observe(t, mgr, created.ID, "d2", healthy.ResourceVersion)
			if got.State != QualificationInvalidated || got.InvalidatedAt == nil || !strings.Contains(got.InvalidationReason, tc.want) {
				t.Fatalf("drift result = %#v, want Invalidated for %q", got, tc.want)
			}
			last := got.Observations[len(got.Observations)-1]
			if last.Successful || len(last.Drift) == 0 || got.SuccessfulSamples != 1 || got.TotalObservations != 2 {
				t.Fatalf("drift observation = %#v", got)
			}
			if (len(fixture.built) != 0) != tc.coverageSeen || (len(last.Coverage) != 0) != tc.coverageSeen {
				t.Fatalf("coverage built %v / recorded %d, want inventory drift decided before coverage", fixture.built, len(last.Coverage))
			}
			if !reflect.DeepEqual(got.Cohort, created.Cohort) || got.Profile != created.Profile || got.ConfigDigest != created.ConfigDigest {
				t.Fatalf("frozen bindings changed after drift: %#v", got)
			}
			if actions := auditActions(t, st, created.ID); actions != "create=Observing,observe=full,observe=drift,invalidate=Invalidated" {
				t.Fatalf("audit = %q", actions)
			}
			// Permanent: restoring the fleet does not revive it.
			*fixture = *newQualificationFixture()
			fixture.now = fixture.now.Add(time.Hour)
			_, _, err = mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{IdempotencyKey: "d3"})
			if !errors.Is(err, ErrInvalidState) {
				t.Fatalf("observe after invalidation = %v, want ErrInvalidState", err)
			}
			if stored, _ := mgr.GetRuntimeContractQualification(ctx, created.ID); stored.State != QualificationInvalidated || stored.ResourceVersion != 3 {
				t.Fatalf("stored = %#v", stored)
			}
		})
	}
}

func TestQualificationObserveFailsClosedWithoutWritingOrReservingKey(t *testing.T) {
	mgr, st, fixture := newQualificationManager(t)
	ctx := context.Background()
	created, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("closed"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.now = fixture.now.Add(time.Minute)
	boom := errors.New("report store read failed")
	// A cohort node whose inventory UID is blank or padded is an inconsistent
	// read, not a recreated node: it is neither trimmed into a match with the
	// frozen UID nor recorded as drift, and no coverage is built for it.
	incomplete := map[string]bool{"nil inventory": true, "duplicate node": true, "blank uid": true, "empty uid": true, "leading pad uid": true, "trailing pad uid": true}
	for name, arrange := range map[string]func(){
		"builder error":    func() { fixture.builderErr = boom },
		"lister error":     func() { fixture.listErr = boom },
		"nil inventory":    func() { fixture.nodes = append(fixture.nodes, nil) },
		"duplicate node":   func() { fixture.nodes = append(fixture.nodes, qualificationNode("gpu-a", "uid-a")) },
		"blank uid":        func() { fixture.nodes[1] = qualificationNode("gpu-a", " ") },
		"empty uid":        func() { fixture.nodes[0] = qualificationNode("gpu-b", "") },
		"leading pad uid":  func() { fixture.nodes[1] = qualificationNode("gpu-a", " uid-a") },
		"trailing pad uid": func() { fixture.nodes[0] = qualificationNode("gpu-b", "uid-b\t") },
	} {
		*fixture = *newQualificationFixture()
		fixture.now = fixture.now.Add(time.Minute)
		arrange()
		got, replayed, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{IdempotencyKey: "closed-" + name})
		if err == nil || got != nil || replayed {
			t.Fatalf("%s: observe = (%#v, %v, %v), want an error", name, got, replayed, err)
		}
		if errors.Is(err, ErrInvalidState) || (name == "builder error" && !errors.Is(err, boom)) || (incomplete[name] && !errors.Is(err, ErrIncompleteInventory)) {
			t.Fatalf("%s: unexpected error class %v", name, err)
		}
		if len(fixture.built) != 0 {
			t.Fatalf("%s: built coverage for %v before failing closed", name, fixture.built)
		}
		stored, getErr := mgr.GetRuntimeContractQualification(ctx, created.ID)
		if getErr != nil || stored.ResourceVersion != 1 || stored.TotalObservations != 0 || stored.State != QualificationObserving {
			t.Fatalf("%s: stored after failure = (%#v, %v), want untouched version 1", name, stored, getErr)
		}
		if _, lookupErr := st.GetOperationalIdempotency(ctx, types.ResourceRuntimeContractQualification, "bob", scopedIdempotencyKey("observe", "closed-"+name)); !errors.Is(lookupErr, store.ErrNotFound) {
			t.Fatalf("%s: idempotency lookup = %v, want ErrNotFound", name, lookupErr)
		}
	}
	if actions := auditActions(t, st, created.ID); actions != "create=Observing" {
		t.Fatalf("audit after failures = %q", actions)
	}
	// Unknown qualification is a plain not-found, also without reserving.
	if _, _, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", "rcq-missing", RuntimeContractQualificationMutation{IdempotencyKey: "missing"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing qualification error = %v", err)
	}
}

func TestQualificationIdempotencyAndOptimisticConcurrency(t *testing.T) {
	mgr, _, fixture := newQualificationManager(t)
	ctx := context.Background()
	request := qualificationRequest("idem")
	created, replayed, err := mgr.CreateRuntimeContractQualification(ctx, "alice", request)
	if err != nil || replayed {
		t.Fatal(err)
	}
	// Same key, same request (different node order, padded names): replay.
	again := request
	again.Nodes = []string{" gpu-a", "gpu-b "}
	got, replayed, err := mgr.CreateRuntimeContractQualification(ctx, "alice", again)
	if err != nil || !replayed || got.ID != created.ID {
		t.Fatalf("create replay = (%#v, %v, %v)", got, replayed, err)
	}
	// Same key, different request digest: conflict, no second resource.
	different := request
	different.Requirements.MinSamples = 3
	if _, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", different); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("create conflict = %v", err)
	}
	// Another actor with the same key is an independent request.
	other, replayed, err := mgr.CreateRuntimeContractQualification(ctx, "carol", request)
	if err != nil || replayed || other.ID == created.ID {
		t.Fatalf("other actor create = (%#v, %v, %v)", other, replayed, err)
	}
	// The store's expiry filter uses the wall clock while the fixture clock is
	// fixed, so listing asks for expired resources explicitly.
	listed, err := mgr.ListRuntimeContractQualifications(ctx, OperationalListOptions{Tenant: "team-a", State: string(QualificationObserving), IncludeExpired: true})
	if err != nil || len(listed) != 2 {
		t.Fatalf("list = (%d, %v), want both qualifications", len(listed), err)
	}
	if none, err := mgr.ListRuntimeContractQualifications(ctx, OperationalListOptions{Tenant: "team-b", IncludeExpired: true}); err != nil || len(none) != 0 {
		t.Fatalf("cross-tenant list = (%v, %v), want none", none, err)
	}

	fixture.now = fixture.now.Add(time.Minute)
	first := observe(t, mgr, created.ID, "obs", created.ResourceVersion)
	replay, replayed, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{ExpectedVersion: created.ResourceVersion, IdempotencyKey: "obs"})
	if err != nil || !replayed || replay.ResourceVersion != first.ResourceVersion || replay.TotalObservations != 1 {
		t.Fatalf("observe replay = (%#v, %v, %v), want the prior result without a second sample", replay, replayed, err)
	}
	if _, _, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{ExpectedVersion: first.ResourceVersion, IdempotencyKey: "obs"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("observe key reuse with a different version = %v, want ErrIdempotencyConflict", err)
	}
	if _, _, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", other.ID, RuntimeContractQualificationMutation{IdempotencyKey: "obs"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("observe key reuse for another qualification = %v, want ErrIdempotencyConflict", err)
	}
	if _, _, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{ExpectedVersion: created.ResourceVersion, IdempotencyKey: "stale"}); !errors.Is(err, store.ErrOperationalConflict) {
		t.Fatalf("stale version = %v, want ErrOperationalConflict", err)
	}
	if _, _, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{}); err == nil || !strings.Contains(err.Error(), "idempotency key are required") {
		t.Fatalf("missing key = %v", err)
	}
	stored, _ := mgr.GetRuntimeContractQualification(ctx, created.ID)
	if stored.ResourceVersion != 2 || stored.TotalObservations != 1 {
		t.Fatalf("stored after rejected observes = %#v, want untouched", stored)
	}
	if untouched, _ := mgr.GetRuntimeContractQualification(ctx, other.ID); untouched.ResourceVersion != 1 || untouched.TotalObservations != 0 {
		t.Fatalf("other qualification touched: %#v", untouched)
	}
	if _, err := mgr.GetRuntimeContractQualification(ctx, " "); err == nil {
		t.Fatal("blank ID must be rejected")
	}
	if _, err := mgr.GetRuntimeContractQualification(ctx, "rcq-missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing get = %v", err)
	}
}

func TestQualificationRetainsBoundedObservationsWithoutLosingCounters(t *testing.T) {
	mgr, _, fixture := newQualificationManager(t)
	ctx := context.Background()
	request := qualificationRequest("bounded")
	request.Requirements = RuntimeContractQualificationRequirements{MinSamples: maxQualificationObservations + 5, MinDuration: time.Minute}
	request.ExpiresAt = fixture.now.Add(24 * time.Hour)
	created, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", request)
	if err != nil {
		t.Fatal(err)
	}
	current := created
	for i := 0; i < maxQualificationObservations+5; i++ {
		fixture.now = fixture.now.Add(time.Minute)
		current = observe(t, mgr, created.ID, "b-"+time.Duration(i).String(), current.ResourceVersion)
	}
	if current.State != QualificationReadyForApproval || current.TotalObservations != maxQualificationObservations+5 || current.SuccessfulSamples != maxQualificationObservations+5 {
		t.Fatalf("counters = %#v", current)
	}
	if len(current.Observations) != maxQualificationObservations || !current.Observations[0].At.Equal(created.CreatedAt.Add(6*time.Minute)) || !current.Observations[len(current.Observations)-1].At.Equal(fixture.now) {
		t.Fatalf("retained window = %d entries from %s, want the most recent %d", len(current.Observations), current.Observations[0].At, maxQualificationObservations)
	}
	if !current.FirstSuccessfulSampleAt.Equal(created.CreatedAt.Add(time.Minute)) || len(current.InitialCoverage) != 2 {
		t.Fatalf("pruning must not touch the first sample time or initial evidence: %#v", current)
	}
}

func TestQualificationExposesNoAuthorityMethods(t *testing.T) {
	// The two View methods are pure read projections (stored record plus the
	// clock); they write nothing and grant nothing. Every other name here is
	// one of the four lifecycle operations.
	allowed := map[string]bool{
		"CreateRuntimeContractQualification": true, "GetRuntimeContractQualification": true,
		"ListRuntimeContractQualifications": true, "ObserveRuntimeContractQualification": true,
		"ViewRuntimeContractQualification": true, "ViewRuntimeContractQualifications": true,
	}
	typ := reflect.TypeOf(&Manager{})
	found := 0
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !strings.Contains(name, "Qualification") {
			continue
		}
		found++
		if !allowed[name] {
			t.Errorf("unexpected qualification method %s: this slice must expose no approve/promote/execute path", name)
		}
	}
	if found != len(allowed) {
		t.Fatalf("found %d qualification methods, want exactly %d", found, len(allowed))
	}
	for _, state := range []RuntimeContractQualificationState{QualificationObserving, QualificationReadyForApproval, QualificationInvalidated, QualificationExpired} {
		if strings.Contains(strings.ToLower(string(state)), "approved") || strings.Contains(strings.ToLower(string(state)), "enabled") {
			t.Fatalf("state %q implies authority", state)
		}
	}
}
