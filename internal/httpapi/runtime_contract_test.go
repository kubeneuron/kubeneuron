package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/operations"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var runtimeContractNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// runtimeContractBuilder is a deterministic stand-in for the controller
// adapter. gpu-a has a fresh report, gpu-b has none, gpu-c has a report from a
// prior node incarnation (blank UID), and ghost does not exist.
func runtimeContractBuilder(_ context.Context, node string, vendor types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
	if node == "ghost" {
		return config.RuntimeContractCoverage{}, store.ErrNotFound
	}
	profile := operationalProfile()
	var report *types.AgentAcceleratorReport
	uid := "node-uid"
	switch node {
	case "gpu-a", "gpu-c":
		reportUID := uid
		if node == "gpu-c" {
			reportUID = ""
		}
		report = &types.AgentAcceleratorReport{
			Node: node, NodeUID: reportUID, Vendor: types.AcceleratorVendorNVIDIA, ObservedAt: runtimeContractNow.Add(-time.Minute),
			ProfileDigest: profile.ProfileDigest, ProfileUID: profile.ProfileUID, ProfileGeneration: profile.ProfileGeneration,
			DriverVersion: profile.DriverVersion, RuntimeVersion: profile.RuntimeVersion,
			TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned, Readiness: types.AcceleratorReadinessReady,
			Devices:      []types.AgentAcceleratorDevice{{ID: "GPU-a", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU}},
			Capabilities: []types.AgentAcceleratorCapability{{Action: types.AcceleratorActionResetDevice, Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice}}},
		}
	}
	return config.AssessRuntimeContractCoverage(config.RuntimeContractCoverageInput{
		Now: runtimeContractNow, ConfigDigest: "sha256:live-config",
		NodeName: node, NodeUID: uid, NodeLabels: map[string]string{"pool": "a100"}, Vendor: vendor,
		AgentLastSeen: runtimeContractNow.Add(-time.Minute), AgentMaxAge: 5 * time.Minute,
		Report: report, Profiles: []config.AcceleratorRuntimeProfile{*profile},
	}), nil
}

func newRuntimeContractHTTPServer(t *testing.T, builder operations.RuntimeContractCoverageBuilder) http.Handler {
	t.Helper()
	manager := operations.New(operations.Options{
		BuildRuntimeContractCoverage: builder,
		ListNodes: func(context.Context) ([]*types.Node, error) {
			return []*types.Node{
				{Name: "gpu-c", Labels: map[string]string{"kubeneuron.io/tenant": "team-a", "kubeneuron.io/cluster": "east"}},
				{Name: "gpu-a", Labels: map[string]string{"kubeneuron.io/tenant": "team-a", "kubeneuron.io/cluster": "east"}},
				{Name: "gpu-b", Labels: map[string]string{"kubeneuron.io/tenant": "team-b", "kubeneuron.io/cluster": "east"}},
			}, nil
		},
	})
	op := &operationalOperator{fakeOperator: &fakeOperator{}, manager: manager}
	server := New(&registrationBackend{})
	server.EnableOperatorAPI(op, "secret")
	return server.Routes()
}

func TestNodeRuntimeContractExactFullResponse(t *testing.T) {
	handler := newRuntimeContractHTTPServer(t, runtimeContractBuilder)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-a/runtime-contract?vendor=nvidia", "secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("node runtime contract = %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	want := `{"version":"runtime-contract-coverage/v1","config_digest":"sha256:live-config","evaluated_at":"2026-09-05T12:00:00Z",` +
		`"node_name":"gpu-a","node_uid":"node-uid","vendor":"nvidia",` +
		`"profile_name":"nvidia-a100","profile_uid":"profile-uid","profile_generation":1,` +
		`"profile_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
		`"selection":"Exact","attestation":"FreshCompatible","verification_depth":"Full",` +
		`"summary":"node \"gpu-a\" vendor \"nvidia\": selection=Exact (profile \"nvidia-a100\" uid \"profile-uid\" generation 1), attestation=FreshCompatible, verification=Full; no findings",` +
		`"agent_last_seen":"2026-09-05T11:59:00Z","report_observed_at":"2026-09-05T11:59:00Z"}` + "\n"
	if rec.Body.String() != want {
		t.Fatalf("node runtime contract body mismatch\n got: %s\nwant: %s", rec.Body.String(), want)
	}
}

func TestNodeRuntimeContractMissingReportAndStrictUID(t *testing.T) {
	handler := newRuntimeContractHTTPServer(t, runtimeContractBuilder)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-b/runtime-contract?vendor=nvidia", "secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("missing report = %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["selection"] != "Exact" || body["attestation"] != "Missing" || body["verification_depth"] != "Reduced" {
		t.Fatalf("missing report body = %v", body)
	}
	if reasons, _ := body["reasons"].([]any); len(reasons) != 1 || reasons[0] != "ReportMissing" {
		t.Fatalf("reasons = %v, want [ReportMissing]", body["reasons"])
	}
	if _, present := body["report_observed_at"]; present {
		t.Fatalf("report_observed_at must be omitted without a report: %v", body)
	}
	if body["node_uid"] != "node-uid" {
		t.Fatalf("node_uid = %v, want the current node identity even without a report", body["node_uid"])
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-c/runtime-contract?vendor=nvidia", "secret", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"attestation":"Mismatch"`) || !strings.Contains(rec.Body.String(), `"reasons":["ReportNodeMismatch"]`) {
		t.Fatalf("blank report UID against a nonblank node UID = %d %s, want Mismatch/ReportNodeMismatch", rec.Code, rec.Body.String())
	}
}

func TestRuntimeContractRoutesValidateVendor(t *testing.T) {
	handler := newRuntimeContractHTTPServer(t, runtimeContractBuilder)
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/api/v1/nodes/gpu-a/runtime-contract", "vendor is required"},
		{"/api/v1/nodes/gpu-a/runtime-contract?vendor=", "vendor is required"},
		{"/api/v1/nodes/gpu-a/runtime-contract?vendor=cuda", "vendor must be nvidia, amd, intel, or google"},
		{"/api/v1/runtime-contracts/coverage", "vendor is required"},
		{"/api/v1/runtime-contracts/coverage?vendor=NVIDIA", "vendor must be nvidia, amd, intel, or google"},
		{"/api/v1/runtime-contracts/coverage?vendor=nvidia&limit=0", "limit must be an integer between 1 and 500"},
		{"/api/v1/runtime-contracts/coverage?vendor=nvidia&limit=501", "limit must be an integer between 1 and 500"},
		{"/api/v1/runtime-contracts/coverage?vendor=nvidia&cursor=%21%21", "cursor is invalid"},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest(http.MethodGet, tc.path, "secret", ""))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s = %d %q, want 400 containing %q", tc.path, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestRuntimeContractRoutesRequireOperatorAuthorization(t *testing.T) {
	handler := newRuntimeContractHTTPServer(t, runtimeContractBuilder)
	for _, path := range []string{"/api/v1/nodes/gpu-a/runtime-contract?vendor=nvidia", "/api/v1/runtime-contracts/coverage?vendor=nvidia"} {
		for _, token := range []string{"", "wrong"} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, operatorRequest(http.MethodGet, path, token, ""))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s with token %q = %d, want 401", path, token, rec.Code)
			}
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest(http.MethodPost, path, "secret", ""))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405: the runtime contract surface is read-only", path, rec.Code)
		}
	}
	// A per-caller authenticator sees the read verb, never a mutation verb.
	auth := &fakeOperatorAuthenticator{identity: OperatorIdentity{Actor: "viewer", Method: "kubernetes"}}
	server := New(&registrationBackend{})
	server.EnableOperatorAPI(&operationalOperator{fakeOperator: &fakeOperator{}, manager: operations.New(operations.Options{
		BuildRuntimeContractCoverage: runtimeContractBuilder,
	})}, "secret")
	server.SetOperatorAuthenticator(auth)
	rec := httptest.NewRecorder()
	server.Routes().ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-a/runtime-contract?vendor=nvidia", "per-caller", ""))
	if rec.Code != http.StatusOK || len(auth.verbs) != 1 || auth.verbs[0] != "get" {
		t.Fatalf("per-caller read = %d verbs=%v", rec.Code, auth.verbs)
	}
}

func TestRuntimeContractRoutesFailClosedWithoutProvider(t *testing.T) {
	handler := operatorServer(&fakeOperator{}, "secret")
	for _, path := range []string{"/api/v1/nodes/gpu-a/runtime-contract?vendor=nvidia", "/api/v1/runtime-contracts/coverage?vendor=nvidia"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest(http.MethodGet, path, "secret", ""))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without operations provider = %d, want 503", path, rec.Code)
		}
	}
	// A manager without the coverage builder is likewise unavailable, not a
	// healthy-looking empty page.
	noBuilder := newRuntimeContractHTTPServer(t, nil)
	for _, path := range []string{"/api/v1/nodes/gpu-a/runtime-contract?vendor=nvidia", "/api/v1/runtime-contracts/coverage?vendor=nvidia"} {
		rec := httptest.NewRecorder()
		noBuilder.ServeHTTP(rec, operatorRequest(http.MethodGet, path, "secret", ""))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "runtime contract coverage builder is unavailable") {
			t.Errorf("%s without builder = %d %s, want 503", path, rec.Code, rec.Body.String())
		}
	}
	unavailable := newRuntimeContractHTTPServer(t, func(context.Context, string, types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
		return config.RuntimeContractCoverage{}, errors.New("accelerator report store is not configured: " + operations.ErrUnavailable.Error())
	})
	rec := httptest.NewRecorder()
	unavailable.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-a/runtime-contract?vendor=nvidia", "secret", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("plain builder error = %d, want the generic 400 mapping", rec.Code)
	}
	wrapped := newRuntimeContractHTTPServer(t, func(context.Context, string, types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
		return config.RuntimeContractCoverage{}, errors.Join(operations.ErrUnavailable, errors.New("accelerator report store is not configured"))
	})
	rec = httptest.NewRecorder()
	wrapped.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/runtime-contracts/coverage?vendor=nvidia", "secret", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable report store = %d %s, want 503", rec.Code, rec.Body.String())
	}
}

func TestNodeRuntimeContractUnknownNodeIs404(t *testing.T) {
	handler := newRuntimeContractHTTPServer(t, runtimeContractBuilder)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/ghost/runtime-contract?vendor=nvidia", "secret", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown node = %d %s, want 404", rec.Code, rec.Body.String())
	}
}

func TestFleetRuntimeContractCoveragePaginationAndScope(t *testing.T) {
	handler := newRuntimeContractHTTPServer(t, runtimeContractBuilder)
	type page struct {
		Items           []config.RuntimeContractCoverage `json:"items"`
		CoverageVersion string                           `json:"coverage_version"`
		NextCursor      string                           `json:"next_cursor"`
	}
	get := func(path string) page {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest(http.MethodGet, path, "secret", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
		var out page
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	names := func(items []config.RuntimeContractCoverage) string {
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.NodeName)
		}
		return strings.Join(out, ",")
	}

	all := get("/api/v1/runtime-contracts/coverage?vendor=nvidia")
	if all.CoverageVersion != config.RuntimeContractCoverageVersion || all.NextCursor != "" || names(all.Items) != "gpu-a,gpu-b,gpu-c" {
		t.Fatalf("full page = %+v", all)
	}
	if all.Items[0].VerificationDepth != config.RuntimeContractVerificationFull || all.Items[1].Attestation != config.RuntimeContractAttestationMissing || all.Items[2].Attestation != config.RuntimeContractAttestationMismatch {
		t.Fatalf("per-node results = %+v", all.Items)
	}

	first := get("/api/v1/runtime-contracts/coverage?vendor=nvidia&limit=2")
	if first.NextCursor == "" || names(first.Items) != "gpu-a,gpu-b" {
		t.Fatalf("first page = %+v", first)
	}
	if strings.Contains(first.NextCursor, "gpu-b") {
		t.Fatalf("cursor %q leaks the node name; it must be opaque", first.NextCursor)
	}
	second := get("/api/v1/runtime-contracts/coverage?vendor=nvidia&limit=2&cursor=" + first.NextCursor)
	if second.NextCursor != "" || names(second.Items) != "gpu-c" {
		t.Fatalf("second page = %+v", second)
	}

	scoped := get("/api/v1/runtime-contracts/coverage?vendor=nvidia&tenant=team-a&cluster=east")
	if scoped.NextCursor != "" || names(scoped.Items) != "gpu-a,gpu-c" {
		t.Fatalf("scoped page = %+v", scoped)
	}
	empty := get("/api/v1/runtime-contracts/coverage?vendor=nvidia&tenant=team-a&cluster=west")
	if len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("unmatched scope = %+v, want an empty page", empty)
	}
}
