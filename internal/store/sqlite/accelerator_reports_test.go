package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func testAcceleratorReport(node string, vendor types.AcceleratorVendor, observedAt time.Time) *types.AgentAcceleratorReport {
	return &types.AgentAcceleratorReport{
		Node:           node,
		NodeUID:        "node-uid-" + node,
		Vendor:         vendor,
		ObservedAt:     observedAt.UTC(),
		DriverVersion:  "550.54.15",
		RuntimeVersion: "gpu-operator-24.9.2",
		TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
		Capabilities: []types.AgentAcceleratorCapability{
			{Action: types.AcceleratorActionVerifyHealth, Scopes: []types.AcceleratorTargetScope{
				types.AcceleratorScopeNode, types.AcceleratorScopePhysicalDevice,
			}},
			{Action: types.AcceleratorActionResetDevice, Scopes: []types.AcceleratorTargetScope{
				types.AcceleratorScopePhysicalDevice,
			}},
		},
		Readiness:         types.AcceleratorReadinessReady,
		ProfileDigest:     "sha256:runtime-profile-v1",
		ProfileUID:        "runtime-profile-uid",
		ProfileGeneration: 1,
		Devices: []types.AgentAcceleratorDevice{
			{
				ID:         "GPU-0001",
				Kind:       types.AcceleratorDevicePhysical,
				Family:     types.AcceleratorFamilyGPU,
				Model:      "NVIDIA H100",
				Attributes: map[string]string{"pci_bus_id": "0000:01:00.0"},
			},
		},
	}
}

func TestAcceleratorReportsRoundTripGetAndList(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	observedAt := time.Date(2026, time.July, 24, 12, 0, 0, 123, time.UTC)
	nvidia := testAcceleratorReport("node-a", types.AcceleratorVendorNVIDIA, observedAt)
	if err := s.UpsertAcceleratorReport(ctx, nvidia); err != nil {
		t.Fatalf("UpsertAcceleratorReport(nvidia): %v", err)
	}

	got, err := s.GetAcceleratorReport(ctx, nvidia.Node, nvidia.Vendor)
	if err != nil {
		t.Fatalf("GetAcceleratorReport: %v", err)
	}
	if !reflect.DeepEqual(got, nvidia) {
		t.Fatalf("GetAcceleratorReport = %#v, want %#v", got, nvidia)
	}

	amd := testAcceleratorReport("node-a", types.AcceleratorVendorAMD, observedAt.Add(time.Second))
	amd.ProfileDigest = "sha256:runtime-profile-amd-v1"
	if err := s.UpsertAcceleratorReport(ctx, amd); err != nil {
		t.Fatalf("UpsertAcceleratorReport(amd): %v", err)
	}
	otherNode := testAcceleratorReport("node-b", types.AcceleratorVendorNVIDIA, observedAt)
	if err := s.UpsertAcceleratorReport(ctx, otherNode); err != nil {
		t.Fatalf("UpsertAcceleratorReport(other node): %v", err)
	}

	nodeReports, err := s.ListAcceleratorReports(ctx, "node-a")
	if err != nil {
		t.Fatalf("ListAcceleratorReports(node): %v", err)
	}
	if len(nodeReports) != 2 || nodeReports[0].Vendor != types.AcceleratorVendorAMD || nodeReports[1].Vendor != types.AcceleratorVendorNVIDIA {
		t.Fatalf("node reports = %#v, want AMD and NVIDIA in vendor order", nodeReports)
	}
	allReports, err := s.ListAcceleratorReports(ctx, "")
	if err != nil {
		t.Fatalf("ListAcceleratorReports(all): %v", err)
	}
	if len(allReports) != 3 {
		t.Fatalf("all reports count = %d, want 3", len(allReports))
	}
	if _, err := s.GetAcceleratorReport(ctx, "missing", types.AcceleratorVendorNVIDIA); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAcceleratorReport(missing) = %v, want ErrNotFound", err)
	}
}

func TestAcceleratorReportsRejectOutOfOrderAndAmbiguousObservations(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	observedAt := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	current := testAcceleratorReport("node-a", types.AcceleratorVendorNVIDIA, observedAt)
	current.ProfileDigest = "sha256:current"
	if err := s.UpsertAcceleratorReport(ctx, current); err != nil {
		t.Fatalf("initial UpsertAcceleratorReport: %v", err)
	}

	// At-least-once transport replay of precisely the same observation is
	// harmless and must not turn a successful write into an agent retry loop.
	if err := s.UpsertAcceleratorReport(ctx, current); err != nil {
		t.Fatalf("identical timestamp replay: %v", err)
	}

	older := testAcceleratorReport("node-a", types.AcceleratorVendorNVIDIA, observedAt.Add(-time.Second))
	older.ProfileDigest = "sha256:old"
	if err := s.UpsertAcceleratorReport(ctx, older); !errors.Is(err, store.ErrStaleAcceleratorReport) {
		t.Fatalf("older UpsertAcceleratorReport = %v, want ErrStaleAcceleratorReport", err)
	}

	// Equal observation times do not establish an ordering. A different
	// payload is therefore rejected rather than selecting a potentially unsafe
	// readiness or capability declaration by arrival order.
	conflicting := *current
	conflicting.ProfileUID = "replacement-profile-uid"
	if err := s.UpsertAcceleratorReport(ctx, &conflicting); !errors.Is(err, store.ErrStaleAcceleratorReport) {
		t.Fatalf("conflicting same-time UpsertAcceleratorReport = %v, want ErrStaleAcceleratorReport", err)
	}

	got, err := s.GetAcceleratorReport(ctx, current.Node, current.Vendor)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfileDigest != current.ProfileDigest || !got.ObservedAt.Equal(current.ObservedAt) {
		t.Fatalf("stale reports overwrote current report: %#v", got)
	}

	newer := testAcceleratorReport("node-a", types.AcceleratorVendorNVIDIA, observedAt.Add(time.Second))
	newer.ProfileDigest = "sha256:new"
	if err := s.UpsertAcceleratorReport(ctx, newer); err != nil {
		t.Fatalf("newer UpsertAcceleratorReport: %v", err)
	}
	got, err = s.GetAcceleratorReport(ctx, newer.Node, newer.Vendor)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfileDigest != newer.ProfileDigest || !got.ObservedAt.Equal(newer.ObservedAt) {
		t.Fatalf("newer report was not retained: %#v", got)
	}
}

func TestAcceleratorReportsValidateBeforePersisting(t *testing.T) {
	s := openLeaseTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAcceleratorReport(ctx, nil); err == nil {
		t.Fatal("nil accelerator report was accepted")
	}

	invalid := testAcceleratorReport("node-a", types.AcceleratorVendorNVIDIA, time.Now())
	invalid.Readiness = types.AcceleratorReadinessNotReady
	invalid.ReadinessReasons = nil
	if err := s.UpsertAcceleratorReport(ctx, invalid); err == nil {
		t.Fatal("invalid accelerator report was accepted")
	}
	if _, err := s.GetAcceleratorReport(ctx, invalid.Node, invalid.Vendor); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("invalid report persisted: %v", err)
	}
}
