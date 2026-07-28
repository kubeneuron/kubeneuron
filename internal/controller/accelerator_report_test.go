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
