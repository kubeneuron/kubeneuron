package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// coverageFixture is a deterministic stand-in for the controller builder. It
// runs the real pure assessment so the manager tests exercise the actual
// result shape rather than a hand-typed struct.
func coverageFixture(now time.Time, node string, vendor types.AcceleratorVendor, report *types.AgentAcceleratorReport) config.RuntimeContractCoverage {
	profile := operationProfile()
	return config.AssessRuntimeContractCoverage(config.RuntimeContractCoverageInput{
		Now:           now,
		ConfigDigest:  "sha256:live-config",
		NodeName:      node,
		NodeUID:       "node-uid",
		NodeLabels:    map[string]string{"pool": "a100"},
		Vendor:        vendor,
		AgentLastSeen: now.Add(-time.Minute),
		AgentMaxAge:   5 * time.Minute,
		Report:        report,
		Profiles:      []config.AcceleratorRuntimeProfile{*profile},
	})
}

func TestNodeRuntimeContractCoverageReturnsBuilderResult(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	var calls []string
	mgr := New(Options{
		BuildRuntimeContractCoverage: func(_ context.Context, node string, vendor types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
			calls = append(calls, node+"/"+string(vendor))
			return coverageFixture(now, node, vendor, coverageReportFor(now, node)), nil
		},
	})
	got, err := mgr.NodeRuntimeContractCoverage(context.Background(), "gpu-a", types.AcceleratorVendorNVIDIA)
	if err != nil {
		t.Fatalf("NodeRuntimeContractCoverage() error = %v", err)
	}
	want := coverageFixture(now, "gpu-a", types.AcceleratorVendorNVIDIA, coverageReportFor(now, "gpu-a"))
	if got.Version != config.RuntimeContractCoverageVersion || got.NodeName != "gpu-a" || got.NodeUID != "node-uid" ||
		got.Selection != config.RuntimeContractSelectionExact || got.Attestation != config.RuntimeContractAttestationFreshCompatible ||
		got.VerificationDepth != config.RuntimeContractVerificationFull || len(got.Reasons) != 0 || got.Summary != want.Summary {
		t.Fatalf("coverage = %#v, want full %#v", got, want)
	}
	if len(calls) != 1 || calls[0] != "gpu-a/nvidia" {
		t.Fatalf("builder calls = %v, want exactly one for gpu-a/nvidia", calls)
	}
}

func coverageReportFor(now time.Time, node string) *types.AgentAcceleratorReport {
	profile := operationProfile()
	return &types.AgentAcceleratorReport{
		Node: node, NodeUID: "node-uid", Vendor: types.AcceleratorVendorNVIDIA,
		ObservedAt: now.Add(-time.Minute), ProfileDigest: profile.ProfileDigest,
		ProfileUID: profile.ProfileUID, ProfileGeneration: profile.ProfileGeneration,
		DriverVersion: profile.DriverVersion, RuntimeVersion: profile.RuntimeVersion,
		TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
		Readiness:      types.AcceleratorReadinessReady,
		Devices:        []types.AgentAcceleratorDevice{{ID: "GPU-a", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU}},
		Capabilities:   []types.AgentAcceleratorCapability{{Action: types.AcceleratorActionResetDevice, Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice}}},
	}
}

func TestNodeRuntimeContractCoverageMissingReportIsMissingNotError(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	mgr := New(Options{
		BuildRuntimeContractCoverage: func(_ context.Context, node string, vendor types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
			return coverageFixture(now, node, vendor, nil), nil
		},
	})
	got, err := mgr.NodeRuntimeContractCoverage(context.Background(), "gpu-a", types.AcceleratorVendorNVIDIA)
	if err != nil {
		t.Fatalf("NodeRuntimeContractCoverage() error = %v", err)
	}
	if got.Attestation != config.RuntimeContractAttestationMissing || got.VerificationDepth != config.RuntimeContractVerificationReduced {
		t.Fatalf("coverage = %#v, want Missing attestation with Reduced depth", got)
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != config.RuntimeContractReasonReportMissing {
		t.Fatalf("reasons = %v, want [ReportMissing]", got.Reasons)
	}
	if !got.ReportObservedAt.IsZero() {
		t.Fatalf("report_observed_at = %v, want zero without a report", got.ReportObservedAt)
	}
}

func TestRuntimeContractCoverageValidation(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	builder := func(_ context.Context, node string, vendor types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
		return coverageFixture(now, node, vendor, nil), nil
	}
	mgr := New(Options{
		BuildRuntimeContractCoverage: builder,
		ListNodes:                    func(context.Context) ([]*types.Node, error) { return []*types.Node{{Name: "gpu-a"}}, nil },
	})
	ctx := context.Background()
	if _, err := mgr.NodeRuntimeContractCoverage(ctx, " ", types.AcceleratorVendorNVIDIA); err == nil || !strings.Contains(err.Error(), "node is required") {
		t.Fatalf("blank node error = %v", err)
	}
	if _, err := mgr.NodeRuntimeContractCoverage(ctx, "gpu-a", ""); err == nil || !strings.Contains(err.Error(), "vendor must be") {
		t.Fatalf("missing vendor error = %v", err)
	}
	if _, err := mgr.NodeRuntimeContractCoverage(ctx, "gpu-a", types.AcceleratorVendor("nvidia ")); err == nil || !strings.Contains(err.Error(), "vendor must be") {
		t.Fatalf("invalid vendor error = %v", err)
	}
	if _, _, err := mgr.FleetRuntimeContractCoveragePage(ctx, RuntimeContractCoverageFilter{Vendor: "cuda"}); err == nil || !strings.Contains(err.Error(), "vendor must be") {
		t.Fatalf("fleet invalid vendor error = %v", err)
	}
	if _, _, err := mgr.FleetRuntimeContractCoveragePage(ctx, RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA, Limit: 501}); err == nil || !strings.Contains(err.Error(), "exceeds 500") {
		t.Fatalf("fleet limit error = %v", err)
	}
}

func TestRuntimeContractCoverageFailsUnavailableWithoutDependencies(t *testing.T) {
	ctx := context.Background()
	var nilManager *Manager
	if _, err := nilManager.NodeRuntimeContractCoverage(ctx, "gpu-a", types.AcceleratorVendorNVIDIA); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil manager error = %v, want ErrUnavailable", err)
	}
	noBuilder := New(Options{ListNodes: func(context.Context) ([]*types.Node, error) { return nil, nil }})
	if _, err := noBuilder.NodeRuntimeContractCoverage(ctx, "gpu-a", types.AcceleratorVendorNVIDIA); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no builder node error = %v, want ErrUnavailable", err)
	}
	if _, _, err := noBuilder.FleetRuntimeContractCoveragePage(ctx, RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no builder fleet error = %v, want ErrUnavailable", err)
	}
	noLister := New(Options{BuildRuntimeContractCoverage: func(context.Context, string, types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
		return config.RuntimeContractCoverage{}, nil
	}})
	if _, _, err := noLister.FleetRuntimeContractCoveragePage(ctx, RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no lister fleet error = %v, want ErrUnavailable", err)
	}
}

func TestRuntimeContractCoverageBuilderErrorsSurface(t *testing.T) {
	boom := errors.New("report store read failed")
	mgr := New(Options{
		BuildRuntimeContractCoverage: func(context.Context, string, types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
			return config.RuntimeContractCoverage{}, boom
		},
		ListNodes: func(context.Context) ([]*types.Node, error) { return []*types.Node{{Name: "gpu-a"}}, nil },
	})
	ctx := context.Background()
	if _, err := mgr.NodeRuntimeContractCoverage(ctx, "gpu-a", types.AcceleratorVendorNVIDIA); !errors.Is(err, boom) {
		t.Fatalf("node builder error = %v, want %v", err, boom)
	}
	items, next, err := mgr.FleetRuntimeContractCoveragePage(ctx, RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA})
	if !errors.Is(err, boom) || items != nil || next != "" {
		t.Fatalf("fleet builder error = (%v, %q, %v), want the store error and no partial page", items, next, err)
	}
	if !strings.Contains(err.Error(), `node "gpu-a"`) {
		t.Fatalf("fleet error %v does not name the failing node", err)
	}
}

func TestFleetRuntimeContractCoveragePageIsStableScopedAndPaginated(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	nodes := []*types.Node{
		{Name: "gpu-c", Labels: map[string]string{"kubeneuron.io/tenant": "team-a", "kubeneuron.io/cluster": "east"}},
		{Name: "gpu-a", Labels: map[string]string{"kubeneuron.io/tenant": "team-a", "kubeneuron.io/cluster": "east"}},
		{Name: "gpu-b", Labels: map[string]string{"kubeneuron.io/tenant": "team-b", "kubeneuron.io/cluster": "east"}},
		{Name: "gpu-d", Labels: map[string]string{"kubeneuron.io/tenant": "team-a", "kubeneuron.io/cluster": "west"}},
	}
	var built []string
	mgr := New(Options{
		BuildRuntimeContractCoverage: func(_ context.Context, node string, vendor types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
			built = append(built, node)
			var report *types.AgentAcceleratorReport
			if node != "gpu-c" {
				report = coverageReportFor(now, node)
			}
			return coverageFixture(now, node, vendor, report), nil
		},
		ListNodes: func(context.Context) ([]*types.Node, error) { return nodes, nil },
	})
	ctx := context.Background()
	names := func(items []config.RuntimeContractCoverage) []string {
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.NodeName)
		}
		return out
	}

	all, next, err := mgr.FleetRuntimeContractCoveragePage(ctx, RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA})
	if err != nil || next != "" {
		t.Fatalf("full page = (%v, %q, %v)", names(all), next, err)
	}
	if got := strings.Join(names(all), ","); got != "gpu-a,gpu-b,gpu-c,gpu-d" {
		t.Fatalf("full page order = %s, want stable name order", got)
	}
	for _, item := range all {
		if item.Vendor != types.AcceleratorVendorNVIDIA {
			t.Fatalf("item %q vendor = %q", item.NodeName, item.Vendor)
		}
	}
	if all[2].Attestation != config.RuntimeContractAttestationMissing || all[0].VerificationDepth != config.RuntimeContractVerificationFull {
		t.Fatalf("per-node results were not preserved: %#v", all)
	}

	// Pagination: a page of two proves a third exists without returning it.
	built = nil
	first, next, err := mgr.FleetRuntimeContractCoveragePage(ctx, RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA, Limit: 2})
	if err != nil || next != "gpu-b" || strings.Join(names(first), ",") != "gpu-a,gpu-b" {
		t.Fatalf("first page = (%v, %q, %v)", names(first), next, err)
	}
	if strings.Join(built, ",") != "gpu-a,gpu-b,gpu-c" {
		t.Fatalf("first page built %v, want the page plus one look-ahead node", built)
	}
	second, next, err := mgr.FleetRuntimeContractCoveragePage(ctx, RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA, Limit: 2, AfterNode: next})
	if err != nil || next != "" || strings.Join(names(second), ",") != "gpu-c,gpu-d" {
		t.Fatalf("second page = (%v, %q, %v)", names(second), next, err)
	}

	// Scope comes from controller-owned node labels.
	scoped, next, err := mgr.FleetRuntimeContractCoveragePage(ctx, RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA, Tenant: "team-a", Cluster: "east"})
	if err != nil || next != "" || strings.Join(names(scoped), ",") != "gpu-a,gpu-c" {
		t.Fatalf("scoped page = (%v, %q, %v)", names(scoped), next, err)
	}
	none, next, err := mgr.FleetRuntimeContractCoveragePage(ctx, RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA, Tenant: "team-z"})
	if err != nil || next != "" || len(none) != 0 {
		t.Fatalf("unmatched scope = (%v, %q, %v), want an empty page", names(none), next, err)
	}
}

// TestFleetRuntimeContractCoveragePageBoundsAFleetAtTheMaximumLimit pins the
// release-plan bound: a fleet one node larger than the maximum page, queried
// at that maximum, yields exactly one full page of 500 in stable name order
// plus an opaque cursor, and assesses only the page plus one look-ahead node.
// The 501st node is never returned and nothing beyond the look-ahead is built,
// so the cost of one request stays bounded by the limit rather than the fleet.
func TestFleetRuntimeContractCoveragePageBoundsAFleetAtTheMaximumLimit(t *testing.T) {
	const fleet, limit = 501, 500
	// Listed in reverse so the stable name order of the page is the manager's
	// doing, not the lister's.
	nodes := make([]*types.Node, 0, fleet)
	for i := fleet - 1; i >= 0; i-- {
		nodes = append(nodes, &types.Node{Name: fmt.Sprintf("gpu-%04d", i)})
	}
	var built []string
	mgr := New(Options{
		BuildRuntimeContractCoverage: func(_ context.Context, node string, vendor types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
			built = append(built, node)
			return config.RuntimeContractCoverage{NodeName: node, Vendor: vendor}, nil
		},
		ListNodes: func(context.Context) ([]*types.Node, error) { return nodes, nil },
	})
	ctx := context.Background()

	page, cursor, err := mgr.FleetRuntimeContractCoveragePage(ctx, RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA, Limit: limit})
	if err != nil {
		t.Fatalf("FleetRuntimeContractCoveragePage() error = %v", err)
	}
	if len(page) != limit {
		t.Fatalf("page length = %d, want exactly %d", len(page), limit)
	}
	for i, item := range page {
		if want := fmt.Sprintf("gpu-%04d", i); item.NodeName != want {
			t.Fatalf("page[%d] = %q, want %q (stable name order)", i, item.NodeName, want)
		}
	}
	if cursor == "" || cursor != page[limit-1].NodeName {
		t.Fatalf("cursor = %q, want the opaque resume cursor for the last returned node %q", cursor, page[limit-1].NodeName)
	}
	if len(built) != limit+1 {
		t.Fatalf("built %d assessments, want the page plus exactly one look-ahead (%d)", len(built), limit+1)
	}
	if last := built[limit]; last != "gpu-0500" {
		t.Fatalf("look-ahead assessment = %q, want the 501st node gpu-0500", last)
	}
	for _, item := range page {
		if item.NodeName == "gpu-0500" {
			t.Fatalf("the look-ahead node gpu-0500 leaked into the page")
		}
	}

	// Resuming from the cursor yields the single remaining node and no cursor;
	// the resume assesses only that node.
	built = nil
	rest, next, err := mgr.FleetRuntimeContractCoveragePage(ctx, RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA, Limit: limit, AfterNode: cursor})
	if err != nil || next != "" || len(rest) != 1 || rest[0].NodeName != "gpu-0500" {
		t.Fatalf("resumed page = (%d items, %q, %v), want only gpu-0500 and no cursor", len(rest), next, err)
	}
	if strings.Join(built, ",") != "gpu-0500" {
		t.Fatalf("resume built %v, want only the remaining node", built)
	}
}

func TestFleetRuntimeContractCoveragePageRejectsIncompleteInventory(t *testing.T) {
	// Each inventory places the defect where the sort comparator would have
	// dereferenced it before validation: last, first, and between valid nodes.
	for name, nodes := range map[string][]*types.Node{
		"nil last":         {{Name: "gpu-a"}, nil},
		"nil first":        {nil, {Name: "gpu-b"}, {Name: "gpu-a"}},
		"blank name":       {{Name: "gpu-b"}, {Name: "  "}, {Name: "gpu-a"}},
		"only nil":         {nil},
		"nil after cursor": {{Name: "gpu-a"}, nil, {Name: "gpu-z"}},
	} {
		t.Run(name, func(t *testing.T) {
			var built []string
			mgr := New(Options{
				BuildRuntimeContractCoverage: func(_ context.Context, node string, _ types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
					built = append(built, node)
					return config.RuntimeContractCoverage{NodeName: node}, nil
				},
				ListNodes: func(context.Context) ([]*types.Node, error) { return nodes, nil },
			})
			items, next, err := mgr.FleetRuntimeContractCoveragePage(context.Background(), RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA, AfterNode: "gpu-a"})
			if !errors.Is(err, ErrIncompleteInventory) || items != nil || next != "" {
				t.Fatalf("incomplete inventory = (%v, %q, %v), want ErrIncompleteInventory and no partial page", items, next, err)
			}
			if len(built) != 0 {
				t.Fatalf("built %v before rejecting the inventory; an incomplete inventory must not be assessed at all", built)
			}
		})
	}
}

func TestFleetRuntimeContractCoveragePageDoesNotReorderListerInventory(t *testing.T) {
	nodes := []*types.Node{{Name: "gpu-c"}, {Name: "gpu-a"}, {Name: "gpu-b"}}
	mgr := New(Options{
		BuildRuntimeContractCoverage: func(_ context.Context, node string, _ types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
			return config.RuntimeContractCoverage{NodeName: node}, nil
		},
		ListNodes: func(context.Context) ([]*types.Node, error) { return nodes, nil },
	})
	items, next, err := mgr.FleetRuntimeContractCoveragePage(context.Background(), RuntimeContractCoverageFilter{Vendor: types.AcceleratorVendorNVIDIA})
	if err != nil || next != "" || len(items) != 3 || items[0].NodeName != "gpu-a" || items[1].NodeName != "gpu-b" || items[2].NodeName != "gpu-c" {
		t.Fatalf("page = (%v, %q, %v), want gpu-a,gpu-b,gpu-c", items, next, err)
	}
	if nodes[0].Name != "gpu-c" || nodes[1].Name != "gpu-a" || nodes[2].Name != "gpu-b" {
		t.Fatalf("lister inventory was reordered in place: %v %v %v", nodes[0].Name, nodes[1].Name, nodes[2].Name)
	}
}
