package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type acceleratorReportBackend struct {
	registrationBackend
	reports   []types.AgentAcceleratorReport
	reportErr error
}

func (b *acceleratorReportBackend) HandleAcceleratorReport(_ *http.Request, report types.AgentAcceleratorReport) error {
	b.reports = append(b.reports, report)
	return b.reportErr
}

func TestAcceleratorReportRouteAcceptsOnlyAuthenticatedNode(t *testing.T) {
	backend := &acceleratorReportBackend{}
	handler := authenticatedAgentRoutes(backend)

	request := httptest.NewRequest(http.MethodPost, types.AgentAcceleratorReportPath,
		strings.NewReader(acceleratorReportBody(t, "node-a")))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %q", response.Code, response.Body.String())
	}
	if len(backend.reports) != 1 {
		t.Fatalf("HandleAcceleratorReport calls = %d, want 1", len(backend.reports))
	}
	got := backend.reports[0]
	if got.Node != "node-a" || got.NodeUID != "node-uid-a" || got.Vendor != types.AcceleratorVendorNVIDIA || len(got.Devices) != 1 || len(got.Capabilities) != 2 {
		t.Fatalf("report = %+v, want decoded authenticated observation", got)
	}

	request = httptest.NewRequest(http.MethodPost, types.AgentAcceleratorReportPath,
		strings.NewReader(acceleratorReportBody(t, "node-b")))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("mismatched node status = %d, want 403; body = %q", response.Code, response.Body.String())
	}
	if len(backend.reports) != 1 {
		t.Fatalf("HandleAcceleratorReport calls = %d, want no mutation for mismatched node", len(backend.reports))
	}
}

func TestAcceleratorReportRouteRejectsMalformedOrUnsafeBodies(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":     acceleratorReportBody(t, "node-a")[:len(acceleratorReportBody(t, "node-a"))-1] + `,"execute":true}`,
		"caller node UID":   acceleratorReportBody(t, "node-a")[:len(acceleratorReportBody(t, "node-a"))-1] + `,"node_uid":"forged-node-uid"}`,
		"unsafe reset":      strings.Replace(acceleratorReportBody(t, "node-a"), `"topology_safety":"verified-unpartitioned"`, `"topology_safety":"unknown"`, 1),
		"trailing document": acceleratorReportBody(t, "node-a") + ` {}`,
	} {
		t.Run(name, func(t *testing.T) {
			backend := &acceleratorReportBackend{}
			request := httptest.NewRequest(http.MethodPost, types.AgentAcceleratorReportPath, strings.NewReader(body))
			response := httptest.NewRecorder()
			authenticatedAgentRoutes(backend).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %q", response.Code, response.Body.String())
			}
			if len(backend.reports) != 0 {
				t.Fatal("malformed or unsafe report reached backend")
			}
		})
	}
}

func TestAcceleratorReportRouteFailsClosedWithoutDurableHandler(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, types.AgentAcceleratorReportPath,
		strings.NewReader(acceleratorReportBody(t, "node-a")))
	response := httptest.NewRecorder()
	authenticatedAgentRoutes(&registrationBackend{}).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %q", response.Code, response.Body.String())
	}
}

func TestAcceleratorReportRouteRequiresAuthenticatedNodeUID(t *testing.T) {
	backend := &acceleratorReportBackend{}
	handler := New(backend).AgentRoutes(fixedAgentAuthenticator{principal: AgentPrincipal{NodeName: "node-a"}})
	request := httptest.NewRequest(http.MethodPost, types.AgentAcceleratorReportPath,
		strings.NewReader(acceleratorReportBody(t, "node-a")))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %q", response.Code, response.Body.String())
	}
	if len(backend.reports) != 0 {
		t.Fatal("UID-less principal reached accelerator report backend")
	}
}

func TestAcceleratorReportRouteRejectsOversizeAndBackendFailure(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		body := acceleratorReportBody(t, "node-a") + strings.Repeat(" ", maxAgentAcceleratorReportBytes+1)
		request := httptest.NewRequest(http.MethodPost, types.AgentAcceleratorReportPath, strings.NewReader(body))
		response := httptest.NewRecorder()
		authenticatedAgentRoutes(&acceleratorReportBackend{}).ServeHTTP(response, request)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body = %q", response.Code, response.Body.String())
		}
	})
	t.Run("backend failure", func(t *testing.T) {
		backend := &acceleratorReportBackend{reportErr: errors.New("database unavailable")}
		request := httptest.NewRequest(http.MethodPost, types.AgentAcceleratorReportPath,
			strings.NewReader(acceleratorReportBody(t, "node-a")))
		response := httptest.NewRecorder()
		authenticatedAgentRoutes(backend).ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body = %q", response.Code, response.Body.String())
		}
		if len(backend.reports) != 1 {
			t.Fatalf("HandleAcceleratorReport calls = %d, want 1", len(backend.reports))
		}
	})
	t.Run("stale report", func(t *testing.T) {
		backend := &acceleratorReportBackend{reportErr: store.ErrStaleAcceleratorReport}
		request := httptest.NewRequest(http.MethodPost, types.AgentAcceleratorReportPath,
			strings.NewReader(acceleratorReportBody(t, "node-a")))
		response := httptest.NewRecorder()
		authenticatedAgentRoutes(backend).ServeHTTP(response, request)
		if response.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %q", response.Code, response.Body.String())
		}
	})
}

func TestAcceleratorReportIsVersionedAgentOnly(t *testing.T) {
	backend := &acceleratorReportBackend{}
	for _, handler := range []http.Handler{New(backend).Routes(), authenticatedAgentRoutes(backend)} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/agents/accelerators/report",
			strings.NewReader(acceleratorReportBody(t, "node-a")))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("legacy route status = %d, want 404", response.Code)
		}
	}

	request := httptest.NewRequest(http.MethodPost, types.AgentAcceleratorReportPath,
		strings.NewReader(acceleratorReportBody(t, "node-a")))
	response := httptest.NewRecorder()
	New(backend).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("public route status = %d, want 404", response.Code)
	}
}

func acceleratorReportBody(t *testing.T, node string) string {
	t.Helper()
	body, err := json.Marshal(types.AgentAcceleratorReport{
		Node:           node,
		Vendor:         types.AcceleratorVendorNVIDIA,
		ObservedAt:     time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC),
		DriverVersion:  "570.42",
		RuntimeVersion: "dcgm-4.1",
		TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
		Devices: []types.AgentAcceleratorDevice{{
			ID: "GPU-a", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU,
		}},
		Capabilities: []types.AgentAcceleratorCapability{{
			Action: types.AcceleratorActionResetDevice, Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice},
		}, {
			Action: types.AcceleratorActionVerifyHealth, Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopeNode},
		}},
		Readiness:     types.AcceleratorReadinessReady,
		ProfileDigest: "sha256:test",
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	return string(body)
}
