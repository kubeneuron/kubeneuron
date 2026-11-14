// SPDX-License-Identifier: LicenseRef-KubeNeuron

package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestHandleAcceleratorReportPersistsObservationOnly(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := New(st, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	report := controllerAcceleratorReport()
	req := httptest.NewRequest("POST", types.AgentAcceleratorReportPath, nil)

	if err := c.HandleAcceleratorReport(req, report); err != nil {
		t.Fatalf("HandleAcceleratorReport() error = %v", err)
	}
	got, err := st.GetAcceleratorReport(req.Context(), report.Node, report.Vendor)
	if err != nil {
		t.Fatalf("GetAcceleratorReport() error = %v", err)
	}
	if got.Readiness != report.Readiness || got.ProfileDigest != report.ProfileDigest || got.ObservedAt != report.ObservedAt {
		t.Fatalf("persisted report = %+v, want %+v", got, report)
	}

	older := report
	older.ObservedAt = older.ObservedAt.Add(-time.Second)
	if err := c.HandleAcceleratorReport(req, older); !errors.Is(err, store.ErrStaleAcceleratorReport) {
		t.Fatalf("older report error = %v, want ErrStaleAcceleratorReport", err)
	}
}

func TestAcceleratorReportsRequireKnownNodeAndExposeLatestEvidence(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := New(st, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if _, err := c.AcceleratorReports(ctx, "node-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown node reports error = %v, want ErrNotFound", err)
	}
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a"}); err != nil {
		t.Fatal(err)
	}
	report := controllerAcceleratorReport()
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}
	reports, err := c.AcceleratorReports(ctx, "node-a")
	if err != nil || len(reports) != 1 || reports[0].ProfileDigest != report.ProfileDigest {
		t.Fatalf("AcceleratorReports() = %+v, %v", reports, err)
	}
}

func controllerAcceleratorReport() types.AgentAcceleratorReport {
	return types.AgentAcceleratorReport{
		Node:           "node-a",
		Vendor:         types.AcceleratorVendorNVIDIA,
		ObservedAt:     time.Date(2026, time.July, 24, 12, 0, 0, 123, time.UTC),
		DriverVersion:  "570.42",
		RuntimeVersion: "nvidia-container-runtime-3.15",
		TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
		Devices: []types.AgentAcceleratorDevice{{
			ID: "GPU-a", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU,
		}},
		Capabilities: []types.AgentAcceleratorCapability{{
			Action: types.AcceleratorActionResetDevice,
			Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice},
		}},
		Readiness:     types.AcceleratorReadinessReady,
		ProfileDigest: "sha256:controller-test",
	}
}

// A vendor outside the four built-in constants reports like any other: the
// protocol is open to every well-formed vendor id, so an accelerator adapter
// for a new vendor gets its report stored and served back.
func TestAReportFromANewVendorIsStoredAndReturned(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := New(st, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a"}); err != nil {
		t.Fatal(err)
	}
	report := types.AgentAcceleratorReport{
		Node: "node-a", NodeUID: "node-uid-a", Vendor: "acme",
		ObservedAt:       time.Date(2026, time.November, 14, 9, 0, 0, 0, time.UTC),
		DriverVersion:    "acme-drv-2.1",
		TopologySafety:   types.AcceleratorTopologyUnknown,
		Devices:          []types.AgentAcceleratorDevice{{ID: "acme-0", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU}},
		Capabilities:     []types.AgentAcceleratorCapability{},
		Readiness:        types.AcceleratorReadinessDegraded,
		ReadinessReasons: []string{"observation only: the acme adapter provides no runtime preflight"},
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("an acme report must be valid: %v", err)
	}
	req := httptest.NewRequest("POST", types.AgentAcceleratorReportPath, nil)
	if err := c.HandleAcceleratorReport(req, report); err != nil {
		t.Fatalf("HandleAcceleratorReport(acme) = %v", err)
	}
	reports, err := c.AcceleratorReports(ctx, "node-a")
	if err != nil || len(reports) != 1 {
		t.Fatalf("AcceleratorReports() = %+v, %v", reports, err)
	}
	if got := reports[0]; got.Vendor != "acme" || len(got.Devices) != 1 || got.Devices[0].ID != "acme-0" || got.DriverVersion != "acme-drv-2.1" {
		t.Fatalf("returned report = %+v", got)
	}
}
