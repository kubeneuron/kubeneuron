package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/internal/operations"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func runtimeContractProfile() config.AcceleratorRuntimeProfile {
	return config.AcceleratorRuntimeProfile{
		Name:              "nvidia-a100",
		NodeSelector:      map[string]string{"pool": "a100"},
		Vendor:            types.AcceleratorVendorNVIDIA,
		ProfileDigest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DriverVersion:     "550.54.15",
		RuntimeVersion:    "dcgm-3.3.5",
		ProfileUID:        "profile-uid",
		ProfileGeneration: 1,
		MaxReportAge:      config.Duration(5 * time.Minute),
		AllowedActions: []config.AcceleratorActionPolicy{{
			Action:                               types.AcceleratorActionResetDevice,
			Scopes:                               []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice},
			RequireVerifiedUnpartitionedTopology: true,
		}},
	}
}

func runtimeContractReport(node, uid string, observed time.Time) types.AgentAcceleratorReport {
	profile := runtimeContractProfile()
	return types.AgentAcceleratorReport{
		Node: node, NodeUID: uid, Vendor: types.AcceleratorVendorNVIDIA, ObservedAt: observed,
		ProfileDigest: profile.ProfileDigest, ProfileUID: profile.ProfileUID, ProfileGeneration: profile.ProfileGeneration,
		DriverVersion: profile.DriverVersion, RuntimeVersion: profile.RuntimeVersion,
		TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned, Readiness: types.AcceleratorReadinessReady,
		Devices:      []types.AgentAcceleratorDevice{{ID: "GPU-a", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU}},
		Capabilities: []types.AgentAcceleratorCapability{{Action: types.AcceleratorActionResetDevice, Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice}}},
	}
}

func newRuntimeContractController(t *testing.T) (*Controller, *storesqlite.Store) {
	t.Helper()
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := New(st, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{runtimeContractProfile()}); err != nil {
		t.Fatal(err)
	}
	return c, st
}

func TestBuildRuntimeContractCoverageFullFromLiveEvidence(t *testing.T) {
	c, st := newRuntimeContractController(t)
	ctx := context.Background()
	lastSeen := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	node := &types.Node{Name: "node-a", UID: "node-uid", Labels: map[string]string{"pool": "a100"}, AgentLastSeen: lastSeen}
	if err := st.UpsertNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	report := runtimeContractReport("node-a", "node-uid", lastSeen)
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}

	got, err := c.BuildRuntimeContractCoverage(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if err != nil {
		t.Fatalf("BuildRuntimeContractCoverage() error = %v", err)
	}
	profile := runtimeContractProfile()
	switch {
	case got.Version != config.RuntimeContractCoverageVersion,
		got.NodeName != "node-a", got.NodeUID != "node-uid", got.Vendor != types.AcceleratorVendorNVIDIA,
		got.ProfileName != profile.Name, got.ProfileUID != profile.ProfileUID, got.ProfileGeneration != profile.ProfileGeneration, got.ProfileDigest != profile.ProfileDigest,
		got.Selection != config.RuntimeContractSelectionExact,
		got.Attestation != config.RuntimeContractAttestationFreshCompatible,
		got.VerificationDepth != config.RuntimeContractVerificationFull,
		len(got.Reasons) != 0,
		got.EvaluatedAt.IsZero(),
		!got.AgentLastSeen.Equal(lastSeen), !got.ReportObservedAt.Equal(lastSeen):
		t.Fatalf("coverage = %#v, want a Full result bound to the retained report", got)
	}
	if got.ConfigDigest != "" {
		t.Fatalf("config_digest = %q, want empty for a file-based deployment without an operator digest", got.ConfigDigest)
	}
	// The builder is read-only: the store still holds exactly the retained
	// report, unchanged.
	after, err := st.GetAcceleratorReport(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if err != nil || !after.ObservedAt.Equal(report.ObservedAt) || after.ProfileDigest != report.ProfileDigest {
		t.Fatalf("retained report after coverage = %+v, %v; want unchanged", after, err)
	}
}

func TestBuildRuntimeContractCoverageMissingReportIsMissingAttestation(t *testing.T) {
	c, st := newRuntimeContractController(t)
	ctx := context.Background()
	node := &types.Node{Name: "node-a", UID: "node-uid", Labels: map[string]string{"pool": "a100"}, AgentLastSeen: time.Now().UTC().Add(-time.Minute)}
	if err := st.UpsertNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	got, err := c.BuildRuntimeContractCoverage(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if err != nil {
		t.Fatalf("BuildRuntimeContractCoverage() error = %v", err)
	}
	if got.Selection != config.RuntimeContractSelectionExact || got.Attestation != config.RuntimeContractAttestationMissing || got.VerificationDepth != config.RuntimeContractVerificationReduced {
		t.Fatalf("coverage = %#v, want Exact/Missing/Reduced", got)
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != config.RuntimeContractReasonReportMissing || !got.ReportObservedAt.IsZero() {
		t.Fatalf("reasons = %v observed=%v, want only ReportMissing and no observation time", got.Reasons, got.ReportObservedAt)
	}
}

func TestBuildRuntimeContractCoverageStrictNodeUID(t *testing.T) {
	c, st := newRuntimeContractController(t)
	ctx := context.Background()
	lastSeen := time.Now().UTC().Add(-time.Minute)
	// The report carries no UID while the current Node object has one: the
	// report came from a prior incarnation and must not attest this node.
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid", Labels: map[string]string{"pool": "a100"}, AgentLastSeen: lastSeen}); err != nil {
		t.Fatal(err)
	}
	report := runtimeContractReport("node-a", "", lastSeen)
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}
	got, err := c.BuildRuntimeContractCoverage(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if err != nil {
		t.Fatalf("BuildRuntimeContractCoverage() error = %v", err)
	}
	if got.Attestation != config.RuntimeContractAttestationMismatch || got.VerificationDepth != config.RuntimeContractVerificationReduced {
		t.Fatalf("coverage = %#v, want Mismatch/Reduced for a blank report UID against a nonblank node UID", got)
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != config.RuntimeContractReasonReportNodeMismatch {
		t.Fatalf("reasons = %v, want [ReportNodeMismatch]", got.Reasons)
	}
}

func TestBuildRuntimeContractCoverageUncoveredNodeAndOtherVendor(t *testing.T) {
	c, st := newRuntimeContractController(t)
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-b", UID: "uid-b", Labels: map[string]string{"pool": "h100"}, AgentLastSeen: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	got, err := c.BuildRuntimeContractCoverage(ctx, "node-b", types.AcceleratorVendorNVIDIA)
	if err != nil {
		t.Fatalf("BuildRuntimeContractCoverage() error = %v", err)
	}
	if got.Selection != config.RuntimeContractSelectionUncovered || got.Attestation != config.RuntimeContractAttestationNotApplicable || got.VerificationDepth != config.RuntimeContractVerificationReduced {
		t.Fatalf("uncovered coverage = %#v", got)
	}
	// Another vendor is a distinct contract: the exact-vendor lookup does not
	// borrow the NVIDIA profile or report.
	amd, err := c.BuildRuntimeContractCoverage(ctx, "node-b", types.AcceleratorVendorAMD)
	if err != nil {
		t.Fatalf("AMD coverage error = %v", err)
	}
	if amd.Vendor != types.AcceleratorVendorAMD || amd.Selection != config.RuntimeContractSelectionUncovered || amd.ProfileName != "" {
		t.Fatalf("AMD coverage = %#v", amd)
	}
}

func TestBuildRuntimeContractCoverageErrorsSurface(t *testing.T) {
	c, _ := newRuntimeContractController(t)
	ctx := context.Background()
	if _, err := c.BuildRuntimeContractCoverage(ctx, "ghost", types.AcceleratorVendorNVIDIA); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown node error = %v, want ErrNotFound", err)
	}
	if _, err := c.BuildRuntimeContractCoverage(ctx, "node-a", "cuda"); err == nil || !strings.Contains(err.Error(), "unsupported vendor") {
		t.Fatalf("invalid vendor error = %v", err)
	}
	if _, err := c.BuildRuntimeContractCoverage(ctx, " ", types.AcceleratorVendorNVIDIA); err == nil || !strings.Contains(err.Error(), "node is required") {
		t.Fatalf("blank node error = %v", err)
	}
}

// reportlessStore hides the accelerator report capability behind the plain
// Store interface, standing in for an older or out-of-tree workflow store.
type reportlessStore struct{ store.Store }

func TestBuildRuntimeContractCoverageWithoutReportStoreIsUnavailable(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid", AgentLastSeen: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	c := New(reportlessStore{st}, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err = c.BuildRuntimeContractCoverage(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if !errors.Is(err, operations.ErrUnavailable) || !strings.Contains(err.Error(), "accelerator report store") {
		t.Fatalf("reportless store error = %v, want ErrUnavailable naming the report store", err)
	}
	// An unknown node answers the same way: capability is checked before any
	// read so the failure cannot be mistaken for a per-node condition.
	if _, err := c.BuildRuntimeContractCoverage(ctx, "ghost", types.AcceleratorVendorNVIDIA); !errors.Is(err, operations.ErrUnavailable) {
		t.Fatalf("reportless unknown node error = %v, want ErrUnavailable", err)
	}
}

// faultyReportStore keeps the accelerator report capability but fails the
// reads the builder depends on, standing in for a store whose backing is
// unhealthy rather than absent.
type faultyReportStore struct {
	*storesqlite.Store
	nodeErr   error
	reportErr error
}

func (s faultyReportStore) GetNode(ctx context.Context, name string) (*types.Node, error) {
	if s.nodeErr != nil {
		return nil, s.nodeErr
	}
	return s.Store.GetNode(ctx, name)
}

func (s faultyReportStore) GetAcceleratorReport(ctx context.Context, node string, vendor types.AcceleratorVendor) (*types.AgentAcceleratorReport, error) {
	if s.reportErr != nil {
		return nil, s.reportErr
	}
	return s.Store.GetAcceleratorReport(ctx, node, vendor)
}

func TestBuildRuntimeContractCoverageStoreReadErrorIsUnavailableNotMissing(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	lastSeen := time.Now().UTC().Add(-time.Minute)
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid", Labels: map[string]string{"pool": "a100"}, AgentLastSeen: lastSeen}); err != nil {
		t.Fatal(err)
	}
	report := runtimeContractReport("node-a", "node-uid", lastSeen)
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A failing report read is distinct from a missing report row: the row is
	// present, so answering Missing would misdescribe the evidence.
	readErr := errors.New("sqlite: database is locked")
	c := New(faultyReportStore{Store: st, reportErr: readErr}, nil, nil, nil, nil, nil, nil, nil, log)
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{runtimeContractProfile()}); err != nil {
		t.Fatal(err)
	}
	got, err := c.BuildRuntimeContractCoverage(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if !errors.Is(err, operations.ErrUnavailable) || !errors.Is(err, readErr) {
		t.Fatalf("report read failure error = %v, want ErrUnavailable wrapping the store error", err)
	}
	if !strings.Contains(err.Error(), `nvidia report for node "node-a"`) {
		t.Fatalf("report read failure error %v does not name the report and node", err)
	}
	if got.NodeName != "" || got.Selection != "" || got.Attestation != "" || got.VerificationDepth != "" {
		t.Fatalf("report read failure returned an assessment %#v, want none", got)
	}

	// A failing node load that is not ErrNotFound is likewise unavailable, not
	// a 404-shaped unknown node.
	nodeErr := errors.New("sqlite: disk I/O error")
	c = New(faultyReportStore{Store: st, nodeErr: nodeErr}, nil, nil, nil, nil, nil, nil, nil, log)
	_, err = c.BuildRuntimeContractCoverage(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if !errors.Is(err, operations.ErrUnavailable) || !errors.Is(err, nodeErr) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("node load failure error = %v, want ErrUnavailable wrapping the store error and not ErrNotFound", err)
	}

	// The same store answers healthily once its reads succeed, proving the
	// failures above came from the injected errors and not the fixture.
	c = New(faultyReportStore{Store: st}, nil, nil, nil, nil, nil, nil, nil, log)
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{runtimeContractProfile()}); err != nil {
		t.Fatal(err)
	}
	got, err = c.BuildRuntimeContractCoverage(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if err != nil || got.VerificationDepth != config.RuntimeContractVerificationFull {
		t.Fatalf("healthy wrapped store coverage = %#v, %v; want Full", got, err)
	}
}

// labelingPlatform stands in for a Kubernetes platform: it can read one exact
// node's labels straight from the Node object (platform.NodeLabeler), which is
// the only source that says what the machine carries right now.
type labelingPlatform struct {
	platform.Platform
	labels map[string]string
	found  bool
	err    error
	calls  []string
}

func (p *labelingPlatform) Name() string { return "labeling" }
func (p *labelingPlatform) NodeLabels(_ context.Context, node string) (map[string]string, bool, error) {
	p.calls = append(p.calls, node)
	if p.err != nil {
		return nil, false, p.err
	}
	if !p.found {
		return nil, false, nil
	}
	return p.labels, true, nil
}

var _ platform.NodeLabeler = (*labelingPlatform)(nil)

// unlabeledPlatform is a platform without the NodeLabeler capability, such as
// bare metal: the stored registration labels remain the only source.
type unlabeledPlatform struct{ platform.Platform }

func (unlabeledPlatform) Name() string { return "unlabeled" }

func newLabeledRuntimeContractController(t *testing.T, plat platform.Platform) (*Controller, *storesqlite.Store) {
	t.Helper()
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := New(st, nil, nil, nil, nil, plat, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{runtimeContractProfile()}); err != nil {
		t.Fatal(err)
	}
	return c, st
}

// TestBuildRuntimeContractCoverageSelectsProfileFromLiveLabels pins profile
// selection to the labels the Node object carries right now, not to the store's
// node row. Agent registration never persists labels, so on a live Kubernetes
// cluster the row's label set is empty (or, after a relabel, stale) and judging
// selection against it made every node Uncovered while unit tests that seed
// labels straight into the store stayed green.
func TestBuildRuntimeContractCoverageSelectsProfileFromLiveLabels(t *testing.T) {
	cases := []struct {
		name          string
		stored        map[string]string
		live          map[string]string
		found         bool
		wantSelection config.RuntimeContractSelection
		wantDepth     config.RuntimeContractVerificationDepth
		why           string
	}{
		{
			name:          "registration stored no labels, live labels match",
			stored:        nil,
			live:          map[string]string{"pool": "a100", "kubernetes.io/hostname": "node-a"},
			found:         true,
			wantSelection: config.RuntimeContractSelectionExact,
			wantDepth:     config.RuntimeContractVerificationFull,
			why:           "registration never writes labels, so a stored-only lookup could never select a profile on Kubernetes",
		},
		{
			name:          "stored labels stale, live labels match",
			stored:        map[string]string{"pool": "h100"},
			live:          map[string]string{"pool": "a100"},
			found:         true,
			wantSelection: config.RuntimeContractSelectionExact,
			wantDepth:     config.RuntimeContractVerificationFull,
			why:           "a node the operator just relabelled into the profile must be assessed against the profile",
		},
		{
			name:          "stored labels match, live labels do not",
			stored:        map[string]string{"pool": "a100"},
			live:          map[string]string{"pool": "h100"},
			found:         true,
			wantSelection: config.RuntimeContractSelectionUncovered,
			wantDepth:     config.RuntimeContractVerificationReduced,
			why:           "a stale stored label must not keep selecting a profile the node no longer carries",
		},
		{
			name:          "node object gone",
			stored:        map[string]string{"pool": "a100"},
			live:          nil,
			found:         false,
			wantSelection: config.RuntimeContractSelectionUncovered,
			wantDepth:     config.RuntimeContractVerificationReduced,
			why:           "a resolved absence is an empty label set, which selects no profile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plat := &labelingPlatform{labels: tc.live, found: tc.found}
			c, st := newLabeledRuntimeContractController(t, plat)
			ctx := context.Background()
			lastSeen := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid", Labels: tc.stored, AgentLastSeen: lastSeen}); err != nil {
				t.Fatal(err)
			}
			report := runtimeContractReport("node-a", "node-uid", lastSeen)
			if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
				t.Fatal(err)
			}

			got, err := c.BuildRuntimeContractCoverage(ctx, "node-a", types.AcceleratorVendorNVIDIA)
			if err != nil {
				t.Fatalf("BuildRuntimeContractCoverage() error = %v", err)
			}
			if got.Selection != tc.wantSelection || got.VerificationDepth != tc.wantDepth {
				t.Fatalf("selection = %s depth = %s, want %s/%s: %s\ncoverage = %#v", got.Selection, got.VerificationDepth, tc.wantSelection, tc.wantDepth, tc.why, got)
			}
			if tc.wantSelection == config.RuntimeContractSelectionExact && got.ProfileName != runtimeContractProfile().Name {
				t.Fatalf("profile = %q, want %q", got.ProfileName, runtimeContractProfile().Name)
			}
			if len(plat.calls) != 1 || plat.calls[0] != "node-a" {
				t.Fatalf("NodeLabels calls = %v, want exactly one for node-a", plat.calls)
			}
		})
	}
}

// TestBuildRuntimeContractCoverageLabelLookupErrorIsUnavailable pins the
// fail-closed behaviour: a platform that cannot read labels right now is an
// unavailable assessment, never an Uncovered node, because a qualification
// observation recording drift on evidence nobody read would be permanent.
func TestBuildRuntimeContractCoverageLabelLookupErrorIsUnavailable(t *testing.T) {
	lookupErr := errors.New("apiserver: connection refused")
	plat := &labelingPlatform{err: lookupErr}
	c, st := newLabeledRuntimeContractController(t, plat)
	ctx := context.Background()
	lastSeen := time.Now().UTC().Add(-time.Minute)
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid", Labels: map[string]string{"pool": "a100"}, AgentLastSeen: lastSeen}); err != nil {
		t.Fatal(err)
	}
	report := runtimeContractReport("node-a", "node-uid", lastSeen)
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}

	got, err := c.BuildRuntimeContractCoverage(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if !errors.Is(err, operations.ErrUnavailable) || !errors.Is(err, lookupErr) {
		t.Fatalf("label lookup failure error = %v, want ErrUnavailable wrapping the platform error", err)
	}
	if !strings.Contains(err.Error(), `labels for node "node-a"`) {
		t.Fatalf("label lookup failure error %v does not name the labels and node", err)
	}
	if got.NodeName != "" || got.Selection != "" || got.Attestation != "" || got.VerificationDepth != "" {
		t.Fatalf("label lookup failure returned an assessment %#v, want none", got)
	}
	// The matching stored labels were not consulted as a fallback: the error
	// is the whole answer.
	if errors.Is(err, store.ErrNotFound) {
		t.Fatalf("label lookup failure error = %v, must not read as an unknown node", err)
	}

	// The same platform answers healthily once its reads succeed, proving the
	// failure above came from the injected error and not the fixture.
	plat.err, plat.found, plat.labels = nil, true, map[string]string{"pool": "a100"}
	got, err = c.BuildRuntimeContractCoverage(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if err != nil || got.VerificationDepth != config.RuntimeContractVerificationFull {
		t.Fatalf("healthy platform coverage = %#v, %v; want Full", got, err)
	}
}

// TestBuildRuntimeContractCoverageWithoutNodeLabelerKeepsStoredLabels pins the
// bare-metal path: a platform that cannot read exact node labels leaves the
// stored registration labels as the source, exactly like no platform at all.
func TestBuildRuntimeContractCoverageWithoutNodeLabelerKeepsStoredLabels(t *testing.T) {
	c, st := newLabeledRuntimeContractController(t, unlabeledPlatform{})
	ctx := context.Background()
	lastSeen := time.Now().UTC().Add(-time.Minute)
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid", Labels: map[string]string{"pool": "a100"}, AgentLastSeen: lastSeen}); err != nil {
		t.Fatal(err)
	}
	report := runtimeContractReport("node-a", "node-uid", lastSeen)
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}
	got, err := c.BuildRuntimeContractCoverage(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if err != nil || got.Selection != config.RuntimeContractSelectionExact || got.VerificationDepth != config.RuntimeContractVerificationFull {
		t.Fatalf("coverage without NodeLabeler = %#v, %v; want Exact/Full from stored labels", got, err)
	}
}

// TestBuildDecisionSnapshotSelectsProfileFromLiveLabels pins the decision
// snapshot — the single input to preview, simulation, and live admission — to
// the labels the Node object carries right now. The store row has none on a
// live cluster, so a snapshot built from it selected no profile (observe-only,
// never Eligible) and handed a candidate preview a label-less Node against
// which no candidate profile could ever be an Exact static selection.
func TestBuildDecisionSnapshotSelectsProfileFromLiveLabels(t *testing.T) {
	cases := []struct {
		name        string
		stored      map[string]string
		live        map[string]string
		found       bool
		wantProfile bool
		wantState   decision.State
	}{
		{name: "registration stored no labels, live labels match", live: map[string]string{"pool": "a100"}, found: true, wantProfile: true, wantState: decision.StateEligible},
		{name: "stored labels match, live labels do not", stored: map[string]string{"pool": "a100"}, live: map[string]string{"pool": "h100"}, found: true, wantState: decision.StateObservedOnly},
		{name: "node object gone", stored: map[string]string{"pool": "a100"}, found: false, wantState: decision.StateObservedOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plat := &labelingPlatform{labels: tc.live, found: tc.found}
			c, st := newLabeledRuntimeContractController(t, plat)
			ctx := context.Background()
			lastSeen := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid", Labels: tc.stored, AgentLastSeen: lastSeen}); err != nil {
				t.Fatal(err)
			}
			report := runtimeContractReport("node-a", "node-uid", lastSeen)
			if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
				t.Fatal(err)
			}
			snapshot, err := c.BuildDecisionSnapshot(ctx, "node-a", decision.Request{Class: decision.ActionObserve})
			if err != nil {
				t.Fatalf("BuildDecisionSnapshot() error = %v", err)
			}
			wantLabels := tc.live
			if !tc.found {
				wantLabels = map[string]string{}
			}
			if len(snapshot.Node.Labels) != len(wantLabels) || (len(wantLabels) > 0 && snapshot.Node.Labels["pool"] != wantLabels["pool"]) {
				t.Fatalf("snapshot node labels = %v, want the live labels %v", snapshot.Node.Labels, wantLabels)
			}
			if (snapshot.Profile != nil) != tc.wantProfile {
				t.Fatalf("snapshot profile = %#v, want selected=%v", snapshot.Profile, tc.wantProfile)
			}
			if got := decision.Evaluate(snapshot); got.State != tc.wantState {
				t.Fatalf("decision = %s (%v), want %s", got.State, got.ReasonCodes, tc.wantState)
			}
		})
	}

	// A platform that cannot read labels right now is a failed capture, not a
	// label-less node that silently selects nothing.
	lookupErr := errors.New("apiserver: connection refused")
	plat := &labelingPlatform{err: lookupErr}
	c, st := newLabeledRuntimeContractController(t, plat)
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid", AgentLastSeen: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BuildDecisionSnapshot(ctx, "node-a", decision.Request{Class: decision.ActionObserve}); !errors.Is(err, lookupErr) || !errors.Is(err, operations.ErrUnavailable) {
		t.Fatalf("label lookup failure error = %v, want ErrUnavailable wrapping the platform error", err)
	}
}

func TestControllerOperationsExposeRuntimeContractCoverage(t *testing.T) {
	c, st := newRuntimeContractController(t)
	ctx := context.Background()
	lastSeen := time.Now().UTC().Add(-time.Minute)
	for _, name := range []string{"node-b", "node-a"} {
		if err := st.UpsertNode(ctx, &types.Node{Name: name, UID: "uid-" + name, Labels: map[string]string{"pool": "a100", "kubeneuron.io/tenant": "team-a"}, AgentLastSeen: lastSeen}); err != nil {
			t.Fatal(err)
		}
	}
	report := runtimeContractReport("node-a", "uid-node-a", lastSeen)
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}
	manager := c.Operations()
	if manager == nil {
		t.Fatal("Operations() = nil for an operational store")
	}
	item, err := manager.NodeRuntimeContractCoverage(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if err != nil || item.VerificationDepth != config.RuntimeContractVerificationFull {
		t.Fatalf("node coverage through operations = %#v, %v", item, err)
	}
	items, next, err := manager.FleetRuntimeContractCoveragePage(ctx, operations.RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA, Tenant: "team-a", Limit: 1})
	if err != nil || len(items) != 1 || items[0].NodeName != "node-a" || next != "node-a" {
		t.Fatalf("fleet page through operations = %#v, %q, %v", items, next, err)
	}
	items, next, err = manager.FleetRuntimeContractCoveragePage(ctx, operations.RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA, Tenant: "team-a", Limit: 1, AfterNode: next})
	if err != nil || len(items) != 1 || items[0].NodeName != "node-b" || next != "" || items[0].Attestation != config.RuntimeContractAttestationMissing {
		t.Fatalf("second fleet page through operations = %#v, %q, %v", items, next, err)
	}
}
