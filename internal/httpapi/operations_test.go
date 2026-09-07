package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/internal/operations"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type operationalOperator struct {
	*fakeOperator
	manager *operations.Manager
}

func (o *operationalOperator) Operations() *operations.Manager { return o.manager }

func operationalProfile() *config.AcceleratorRuntimeProfile {
	return &config.AcceleratorRuntimeProfile{
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

func newOperationalHTTPServer(t *testing.T, authenticators ...OperatorAuthenticator) (http.Handler, func()) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	manager := operations.New(operations.Options{
		Resources: st,
		Workflow:  st,
		BuildSnapshot: func(_ context.Context, node string, request decision.Request) (decision.Snapshot, error) {
			now := time.Now().UTC()
			profile := operationalProfile()
			return decision.Snapshot{
				Version: decision.EvaluatorVersion, EvaluatedAt: now, ConfigDigest: "sha256:live-config",
				Node: types.Node{Name: node, UID: "node-uid", Labels: map[string]string{"pool": "a100"}, AgentLastSeen: now.Add(-time.Minute)},
				Report: &types.AgentAcceleratorReport{
					Node: node, NodeUID: "node-uid", Vendor: types.AcceleratorVendorNVIDIA, ObservedAt: now.Add(-time.Minute),
					ProfileDigest: profile.ProfileDigest, ProfileUID: profile.ProfileUID, ProfileGeneration: profile.ProfileGeneration,
					DriverVersion: profile.DriverVersion, RuntimeVersion: profile.RuntimeVersion,
					TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned, Readiness: types.AcceleratorReadinessReady,
					Devices:      []types.AgentAcceleratorDevice{{ID: "GPU-a", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU}},
					Capabilities: []types.AgentAcceleratorCapability{{Action: types.AcceleratorActionResetDevice, Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice}}},
				},
				Profile: profile, Request: request,
			}, nil
		},
		ListNodes: func(context.Context) ([]*types.Node, error) { return []*types.Node{{Name: "gpu-a"}}, nil },
		CreateIncident: func(_ context.Context, signal types.Signal) (*types.Incident, error) {
			return &types.Incident{ID: "inc-from-simulation", Target: signal.Target, Class: signal.Class}, nil
		},
	})
	op := &operationalOperator{fakeOperator: &fakeOperator{}, manager: manager}
	server := New(&registrationBackend{})
	server.EnableOperatorAPI(op, "secret")
	if len(authenticators) > 0 && authenticators[0] != nil {
		server.SetOperatorAuthenticator(authenticators[0])
	}
	return server.Routes(), func() { _ = st.Close() }
}

type roleTokenAuthenticator map[string]OperatorIdentity

func (a roleTokenAuthenticator) AuthenticateOperator(r *http.Request, _ string) (OperatorIdentity, error) {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	identity, ok := a[token]
	if !ok {
		return OperatorIdentity{}, &statusError{status: http.StatusUnauthorized}
	}
	return identity, nil
}

func TestOperationalAPIsRequireDurableProvider(t *testing.T) {
	handler := operatorServer(&fakeOperator{}, "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/readiness", "secret", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness without operations provider = %d, want 503", rec.Code)
	}
}

func TestOperationalCandidateReadinessAndHealthRoutes(t *testing.T) {
	handler, closeStore := newOperationalHTTPServer(t)
	defer closeStore()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/readiness", "secret", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items"`) {
		t.Fatalf("readiness = %d %s", rec.Code, rec.Body.String())
	}

	content := `apiVersion: kubeneuron.io/v1alpha1
kind: CandidateConfiguration
accelerator_profiles:
  - name: nvidia-a100
    node_selector: {pool: a100}
    vendor: nvidia
    profile_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    driver_version: "550.54.15"
    runtime_version: dcgm-3.3.5
    profile_uid: profile-uid
    profile_generation: 1
    max_report_age: 5m
    allowed_actions:
      - action: reset-device
        scopes: [physical-device]
        require_verified_unpartitioned_topology: true
`
	request := operatorRequest(http.MethodPost, "/api/v1/candidates?actor=alice", "secret", content)
	request.Header.Set("Content-Type", "application/yaml")
	request.Header.Set("Idempotency-Key", "candidate-route")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusCreated {
		t.Fatalf("candidate create = %d %s", rec.Code, rec.Body.String())
	}
	var candidate operations.CandidateConfiguration
	if err := json.Unmarshal(rec.Body.Bytes(), &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate.ID == "" {
		t.Fatalf("candidate = %#v", candidate)
	}

	request = operatorRequest(http.MethodPost, "/api/v1/candidates/"+candidate.ID+"/preview", "secret", `{"actor":"alice"}`)
	request.Header.Set("Idempotency-Key", "preview-route")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"inventory_snapshot_id"`) {
		t.Fatalf("candidate preview = %d %s", rec.Code, rec.Body.String())
	}

	request = operatorRequest(http.MethodPost, "/api/v1/health-checks", "secret", `{"actor":"alice","node":"gpu-a","profile":"Passive","reason":"capture baseline"}`)
	request.Header.Set("Idempotency-Key", "health-route")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"state":"completed"`) {
		t.Fatalf("passive health check = %d %s", rec.Code, rec.Body.String())
	}

	request = operatorRequest(http.MethodPost, "/api/v1/simulations", "secret", `{"actor":"alice","node":"gpu-a","device_id":"GPU-a","action":"reset-device","scope":"physical-device","class":"ecc-dbe","rationale":"check path"}`)
	request.Header.Set("Idempotency-Key", "simulation-route")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"decision_snapshot_id"`) {
		t.Fatalf("simulation = %d %s", rec.Code, rec.Body.String())
	}
}

func TestExtendedHealthCheckRequiresVerifiedRoles(t *testing.T) {
	handler, closeStore := newOperationalHTTPServer(t, roleTokenAuthenticator{
		"diagnostics-only": {Actor: "diag-user", Method: "kubernetes", Roles: []string{roleExtendedDiagnostics}},
		"both-roles":       {Actor: "ops-user", Method: "kubernetes", Roles: []string{roleExtendedDiagnostics, roleDisruptionBudgetApprover}},
	})
	defer closeStore()

	body := `{"actor":"claimed","node":"gpu-a","profile":"Extended","reason":"investigate reset"}`
	request := operatorRequest(http.MethodPost, "/api/v1/health-checks", "secret", body)
	request.Header.Set("Idempotency-Key", "extended-static")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("static extended health check = %d %s, want 403", rec.Code, rec.Body.String())
	}

	request = operatorRequest(http.MethodPost, "/api/v1/health-checks", "diagnostics-only", body)
	request.Header.Set("Idempotency-Key", "extended-one-role")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("one-role extended health check = %d %s, want 403", rec.Code, rec.Body.String())
	}

	// The previous client-controlled grants are not part of the public JSON
	// contract, so strict decoding rejects attempts to self-authorize.
	request = operatorRequest(http.MethodPost, "/api/v1/health-checks", "both-roles", `{"actor":"claimed","node":"gpu-a","profile":"Extended","reason":"investigate reset","elevated_authorization_granted":true}`)
	request.Header.Set("Idempotency-Key", "extended-forged-grant")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("client grant = %d %s, want 400", rec.Code, rec.Body.String())
	}

	request = operatorRequest(http.MethodPost, "/api/v1/health-checks", "both-roles", body)
	request.Header.Set("Idempotency-Key", "extended-verified")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusCreated {
		t.Fatalf("verified extended health check = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"actor":"ops-user"`) {
		t.Fatalf("extended health actor was not the verified identity: %s", rec.Body.String())
	}
}

func TestAutonomyPlanRoutesRequireDistinctApprovalAndVersion(t *testing.T) {
	handler, closeStore := newOperationalHTTPServer(t, roleTokenAuthenticator{
		"platform-credential": {Actor: "platform-user", Method: "kubernetes", Roles: []string{"platform"}},
		"safety-credential":   {Actor: "safety-user", Method: "kubernetes", Roles: []string{"safety"}},
	})
	defer closeStore()
	simulationBody := `{"actor":"alice","node":"gpu-a","device_id":"GPU-a","action":"reset-device","scope":"physical-device","class":"ecc-dbe","rationale":"qualify autonomy"}`
	request := operatorRequest(http.MethodPost, "/api/v1/simulations", "secret", simulationBody)
	request.Header.Set("Idempotency-Key", "autonomy-simulation")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusCreated {
		t.Fatalf("simulation = %d %s", rec.Code, rec.Body.String())
	}
	var simulation operations.RemediationSimulation
	if err := json.Unmarshal(rec.Body.Bytes(), &simulation); err != nil {
		t.Fatal(err)
	}
	planBody := `{"actor":"author","selector":{"pool":"a100"},"policy_ref":"ecc@sha256:policy","profile_ref":"a100#1","allowed_actions":["reset-device"],"evidence":{"max_age":"5m","required_sources":["agent"]},"guardrails":{"max_concurrent_nodes":1,"max_actions_per_hour":2,"error_budget":0,"no_active_incident":true},"rollout":{"canary_nodes":1,"bake_duration":"1m"},"approvals":{"required_roles":["platform","safety"],"distinct_subjects":true},"simulation_id":"` + simulation.ID + `","expires_at":"2099-01-01T00:00:00Z"}`
	request = operatorRequest(http.MethodPost, "/api/v1/autonomy/plans", "secret", planBody)
	request.Header.Set("Idempotency-Key", "autonomy-plan")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create plan = %d %s", rec.Code, rec.Body.String())
	}
	var plan operations.GPUAutonomyPlan
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	// A shared token has no verifiable person or role. The request body must
	// not be able to turn it into an approver.
	request = operatorRequest(http.MethodPost, "/api/v1/autonomy/plans/"+plan.ID+"/approve", "secret", `{"actor":"platform-user","role":"platform","resource_version":`+strconv.Itoa(plan.ResourceVersion)+`}`)
	request.Header.Set("Idempotency-Key", "forged-static-approval")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("static approval = %d %s, want 403", rec.Code, rec.Body.String())
	}
	approve := func(token, actor, role, key string) {
		body := `{"actor":"` + actor + `","role":"` + role + `","resource_version":` + strconv.Itoa(plan.ResourceVersion) + `}`
		request = operatorRequest(http.MethodPost, "/api/v1/autonomy/plans/"+plan.ID+"/approve", token, body)
		request.Header.Set("Idempotency-Key", key)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, request)
		if rec.Code != http.StatusOK {
			t.Fatalf("approve %s = %d %s", role, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
			t.Fatal(err)
		}
	}
	approve("platform-credential", "mallory", "platform", "autonomy-approve-platform")
	approve("safety-credential", "mallory", "safety", "autonomy-approve-safety")
	if plan.State != operations.AutonomyCanary || plan.ResourceVersion < 3 {
		t.Fatalf("approved plan = %#v", plan)
	}
	if len(plan.ApprovalRecords) != 2 || plan.ApprovalRecords[0].Actor != "platform-user" || plan.ApprovalRecords[1].Actor != "safety-user" {
		t.Fatalf("approval identities = %#v", plan.ApprovalRecords)
	}
	request = operatorRequest(http.MethodGet, "/api/v1/autonomy/plans/"+plan.ID, "secret", "")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"Canary"`) {
		t.Fatalf("get plan = %d %s", rec.Code, rec.Body.String())
	}
}
