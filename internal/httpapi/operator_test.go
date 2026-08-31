package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// fakeOperator records operator-API calls.
type fakeOperator struct {
	incidents []*types.Incident
	decisions []string
	resolved  []string
	paused    bool
}

type acceleratorReportOperator struct {
	*fakeOperator
	reports []types.AgentAcceleratorReport
	err     error
}

func (f *acceleratorReportOperator) AcceleratorReports(_ context.Context, _ string) ([]*types.AgentAcceleratorReport, error) {
	if f.err != nil {
		return nil, f.err
	}
	reports := make([]*types.AgentAcceleratorReport, 0, len(f.reports))
	for i := range f.reports {
		report := f.reports[i]
		reports = append(reports, &report)
	}
	return reports, nil
}

func (f *fakeOperator) ListIncidents(_ context.Context, states []string, node string, limit int) ([]*types.Incident, error) {
	return f.incidents, nil
}

func (f *fakeOperator) IncidentDetail(_ context.Context, id string) (*types.Incident, []*types.AuditEntry, error) {
	return &types.Incident{ID: id}, []*types.AuditEntry{{IncidentID: id, Action: "open"}}, nil
}

func (f *fakeOperator) DecideApproval(_ context.Context, id, actor, channel string, decision types.ApprovalDecision, expectedEpoch int, reason string) error {
	f.decisions = append(f.decisions, fmt.Sprintf("%s:%s:%s:%d", id, decision, actor, expectedEpoch))
	return nil
}

func (f *fakeOperator) ResolveIncident(_ context.Context, id, actor, reason string) error {
	f.resolved = append(f.resolved, id+":"+actor)
	return nil
}

func (f *fakeOperator) Nodes(context.Context) ([]*types.Node, error) {
	return []*types.Node{{Name: "n1"}}, nil
}

func (f *fakeOperator) Node(_ context.Context, name string) (*types.Node, error) {
	return &types.Node{Name: name}, nil
}

func (f *fakeOperator) SetPaused(paused bool, _ string) error { f.paused = paused; return nil }
func (f *fakeOperator) Paused() bool                          { return f.paused }

func operatorServer(op OperatorBackend, token string) http.Handler {
	s := New(&registrationBackend{})
	s.EnableOperatorAPI(op, token)
	return s.Routes()
}

func operatorRequest(method, path, token, body string) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestOperatorAPIFailsClosedWithoutToken(t *testing.T) {
	// No token configured: disabled entirely.
	s := New(&registrationBackend{})
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents", "any", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled API status = %d, want 503", rec.Code)
	}

	// Token configured but caller presents a wrong one.
	handler := operatorServer(&fakeOperator{}, "secret")
	for _, presented := range []string{"", "wrong"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents", presented, ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q status = %d, want 401", presented, rec.Code)
		}
	}
}

func TestOperatorAPIAcceleratorReportVisibilityIsAuthenticatedAndFailClosed(t *testing.T) {
	plain := operatorServer(&fakeOperator{}, "secret")
	rec := httptest.NewRecorder()
	plain.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/n1/accelerators", "secret", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing report backend status = %d, want 503", rec.Code)
	}

	op := &acceleratorReportOperator{
		fakeOperator: &fakeOperator{},
		reports: []types.AgentAcceleratorReport{{
			Node: "n1", Vendor: types.AcceleratorVendorNVIDIA, Readiness: types.AcceleratorReadinessDegraded,
		}},
	}
	handler := operatorServer(op, "secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/n1/accelerators", "", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated report status = %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/n1/accelerators", "secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("report visibility status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var reports []types.AgentAcceleratorReport
	if err := json.Unmarshal(rec.Body.Bytes(), &reports); err != nil || len(reports) != 1 || reports[0].Node != "n1" {
		t.Fatalf("reports = %+v, err=%v", reports, err)
	}
}

func TestOperatorAPIReadsAndDecisions(t *testing.T) {
	op := &fakeOperator{incidents: []*types.Incident{{ID: "inc-1", State: types.StateNeedsHuman}}}
	handler := operatorServer(op, "secret")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents?state=NEEDS_HUMAN", "secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var incidents []*types.Incident
	if err := json.Unmarshal(rec.Body.Bytes(), &incidents); err != nil || len(incidents) != 1 {
		t.Fatalf("list body = %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents/inc-1", "secret", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"audit"`) {
		t.Fatalf("detail = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/incidents/inc-1/approve", "secret", `{"actor":"alice"}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("approve status = %d %s", rec.Code, rec.Body.String())
	}
	if len(op.decisions) != 1 || op.decisions[0] != "inc-1:approved:token:alice:0" {
		t.Fatalf("decisions = %v", op.decisions)
	}

	// A round-pinned click (park_epoch from the panel/notification) reaches
	// the backend intact so a mid-click re-park can refuse it.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/incidents/inc-1/reject", "secret", `{"actor":"alice","park_epoch":3}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("pinned reject status = %d %s", rec.Code, rec.Body.String())
	}
	if op.decisions[len(op.decisions)-1] != "inc-1:rejected:token:alice:3" {
		t.Fatalf("decisions = %v, want the pinned round forwarded", op.decisions)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/incidents/inc-1/approve", "secret", `{}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("approve without actor = %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/incidents/inc-1/resolve", "secret", `{"actor":"bob","reason":"replaced GPU"}`))
	if rec.Code != http.StatusNoContent || len(op.resolved) != 1 {
		t.Fatalf("resolve = %d, resolved %v", rec.Code, op.resolved)
	}
}

func TestOperatorAPIPauseResume(t *testing.T) {
	op := &fakeOperator{}
	handler := operatorServer(op, "secret")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/pause", "secret", `{"actor":"alice"}`))
	if rec.Code != http.StatusNoContent || !op.paused {
		t.Fatalf("pause = %d, paused %v", rec.Code, op.paused)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/pause", "secret", ""))
	if !strings.Contains(rec.Body.String(), `"paused":true`) {
		t.Fatalf("pause state body = %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("DELETE", "/api/v1/pause?actor=alice", "secret", ""))
	if rec.Code != http.StatusNoContent || op.paused {
		t.Fatalf("resume = %d, paused %v", rec.Code, op.paused)
	}
}

// A Service normally removes a standby from endpoints through readiness, but
// port-forwarding or a direct Pod address bypasses that routing rule. Mutations
// must still be fenced to the elected leader rather than returning a successful
// process-local pause that the active replica never sees.
func TestOperatorMutationIsRejectedOnStandby(t *testing.T) {
	op := &fakeOperator{}
	s := New(&registrationBackend{})
	s.EnableOperatorAPI(op, "secret")
	s.SetReadyCheck(func() bool { return false })

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, operatorRequest("POST", "/api/v1/pause", "secret", `{"actor":"alice"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("standby pause status = %d, want 503", rec.Code)
	}
	if op.paused {
		t.Fatal("a standby must not mutate its private pause state")
	}
}

func TestManualIncidentTriggersSignal(t *testing.T) {
	backend := &registrationBackend{}
	s := New(backend)
	s.EnableOperatorAPI(&fakeOperator{}, "secret")
	handler := s.Routes()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/incidents", "secret",
		`{"node":"n1","class":"ecc-dbe","actor":"alice"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("manual incident = %d %s", rec.Code, rec.Body.String())
	}
	if len(backend.signals) != 1 || backend.signals[0].Source != types.SourceManual {
		t.Fatalf("signals = %+v, want one manual signal", backend.signals)
	}
}

func TestWebhookTokenEnforcement(t *testing.T) {
	s := New(&registrationBackend{})
	s.SetWebhookToken("hook-secret")
	handler := s.Routes()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/webhooks/alertmanager", "", `{"alerts":[]}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated webhook = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/webhooks/alertmanager", "hook-secret", `{"alerts":[]}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("authenticated webhook = %d, want 202", rec.Code)
	}
}

// inventoryOperator knows only an explicit set of nodes, so the webhook's
// node-inventory cross-check can be exercised.
type inventoryOperator struct {
	*fakeOperator
	known map[string]bool
}

func (o *inventoryOperator) Node(_ context.Context, name string) (*types.Node, error) {
	if o.known[name] {
		return &types.Node{Name: name}, nil
	}
	return nil, errors.New("node not found")
}

func TestWebhookFailsClosedWithoutToken(t *testing.T) {
	// No token configured and no explicit dev opt-in: the webhook must reject,
	// because an unauthenticated caller could POST a firing critical alert for an
	// arbitrary node and drive cordon/drain.
	s := New(&registrationBackend{})
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, operatorRequest("POST", "/api/v1/webhooks/alertmanager", "", `{"alerts":[]}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless webhook = %d, want 401 (fail closed)", rec.Code)
	}
}

func TestWebhookDropsAlertsForUnknownNodes(t *testing.T) {
	backend := &registrationBackend{}
	s := New(backend)
	s.EnableOperatorAPI(&inventoryOperator{fakeOperator: &fakeOperator{}, known: map[string]bool{"known-node": true}}, "op")
	s.SetWebhookToken("hook")
	handler := s.Routes()

	firing := func(node string) string {
		return `{"alerts":[{"status":"firing","labels":{"alertname":"GpuRowRemapFailure","node":"` + node + `"}}]}`
	}

	// A spoofed node label the controller has never seen must not drive a signal.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/webhooks/alertmanager", "hook", firing("ghost-node")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook status = %d, want 202", rec.Code)
	}
	if len(backend.signals) != 0 {
		t.Fatalf("an alert for an unknown node must not drive remediation; got %d signals", len(backend.signals))
	}

	// A real node still drives exactly one signal.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/webhooks/alertmanager", "hook", firing("known-node")))
	if len(backend.signals) != 1 || backend.signals[0].Target.Node != "known-node" {
		t.Fatalf("a known-node alert must drive one signal; got %+v", backend.signals)
	}
}

func TestTargetsServesHTTPSD(t *testing.T) {
	handler := operatorServer(&fakeOperator{}, "secret")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/targets", "secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("targets status = %d", rec.Code)
	}
	var groups []struct {
		Targets []string          `json:"targets"`
		Labels  map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Targets[0] != "n1:9400" || groups[0].Labels["node"] != "n1" {
		t.Fatalf("groups = %+v", groups)
	}

	// Custom exporter port; auth still required.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/targets?port=9100", "secret", ""))
	if !strings.Contains(rec.Body.String(), "n1:9100") {
		t.Fatalf("custom port body = %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/targets", "", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated targets = %d, want 401", rec.Code)
	}
}

type fakeOperatorAuthenticator struct {
	identity OperatorIdentity
	err      error
	verbs    []string
}

func (f *fakeOperatorAuthenticator) AuthenticateOperator(_ *http.Request, verb string) (OperatorIdentity, error) {
	f.verbs = append(f.verbs, verb)
	if f.err != nil {
		return OperatorIdentity{}, f.err
	}
	return f.identity, nil
}

type statusError struct{ status int }

func (e *statusError) Error() string   { return "denied" }
func (e *statusError) HTTPStatus() int { return e.status }

func TestOperatorAPIAuthenticatedIdentityOverridesClaimedActor(t *testing.T) {
	op := &fakeOperator{}
	s := New(&registrationBackend{})
	s.EnableOperatorAPI(op, "secret")
	auth := &fakeOperatorAuthenticator{identity: OperatorIdentity{
		Actor: "system:serviceaccount:ops:sre-bot", Method: "kubernetes",
	}}
	s.SetOperatorAuthenticator(auth)
	handler := s.Routes()

	// A per-caller credential (not the static token) authenticates, and the
	// verified principal wins over the self-asserted body actor.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/incidents/inc-1/approve", "caller-credential", `{"actor":"mallory"}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("approve status = %d %s", rec.Code, rec.Body.String())
	}
	if len(op.decisions) != 1 || op.decisions[0] != "inc-1:approved:system:serviceaccount:ops:sre-bot:0" {
		t.Fatalf("decisions = %v, want the authenticated principal", op.decisions)
	}
	// The authenticated caller does not need to claim any actor at all.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/incidents/inc-1/approve", "caller-credential", `{}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("approve without claim = %d %s", rec.Code, rec.Body.String())
	}

	// Mutations are authorized as "update", reads as "get".
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents", "caller-credential", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	if len(auth.verbs) != 3 || auth.verbs[0] != "update" || auth.verbs[1] != "update" || auth.verbs[2] != "get" {
		t.Fatalf("authorized verbs = %v, want [update update get]", auth.verbs)
	}

	// The static token keeps working beside the authenticator, and its
	// self-asserted actor stays visibly unverified.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/incidents/inc-1/approve", "secret", `{"actor":"alice"}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("static-token approve = %d", rec.Code)
	}
	if op.decisions[len(op.decisions)-1] != "inc-1:approved:token:alice:0" {
		t.Fatalf("static-token decision = %v", op.decisions)
	}
}

func TestOperatorAPIAuthenticatorErrorsKeepStatus(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want int
	}{
		{err: &statusError{status: http.StatusForbidden}, want: http.StatusForbidden},
		{err: errors.New("bad token"), want: http.StatusUnauthorized},
	} {
		s := New(&registrationBackend{})
		s.EnableOperatorAPI(&fakeOperator{}, "secret")
		s.SetOperatorAuthenticator(&fakeOperatorAuthenticator{err: tt.err})
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents", "caller-credential", ""))
		if rec.Code != tt.want {
			t.Fatalf("error %v status = %d, want %d", tt.err, rec.Code, tt.want)
		}
	}
}

func TestOperatorAPIThrottlesRepeatedAuthFailures(t *testing.T) {
	handler := operatorServer(&fakeOperator{}, "secret")
	failed := func() int {
		rec := httptest.NewRecorder()
		req := operatorRequest("GET", "/api/v1/incidents", "wrong-token", "")
		req.RemoteAddr = "203.0.113.7:41000"
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < authFailureLimit; i++ {
		if code := failed(); code != http.StatusUnauthorized {
			t.Fatalf("failure %d status = %d, want 401", i+1, code)
		}
	}
	if code := failed(); code != http.StatusTooManyRequests {
		t.Fatalf("post-budget status = %d, want 429", code)
	}

	// The right credential is never throttled, from any source.
	rec := httptest.NewRecorder()
	good := operatorRequest("GET", "/api/v1/incidents", "secret", "")
	good.RemoteAddr = "203.0.113.7:41001"
	handler.ServeHTTP(rec, good)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid credential status = %d, want 200", rec.Code)
	}

	// Another source keeps its own failure budget.
	rec = httptest.NewRecorder()
	other := operatorRequest("GET", "/api/v1/incidents", "wrong-token", "")
	other.RemoteAddr = "198.51.100.9:52000"
	handler.ServeHTTP(rec, other)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("other source status = %d, want 401", rec.Code)
	}
}

func TestOperatorTokenProviderEnablesHotRotation(t *testing.T) {
	op := &fakeOperator{}
	s := New(&registrationBackend{})
	s.EnableOperatorAPI(op, "old-token")
	current := "old-token"
	s.SetOperatorTokenProvider(func() string { return current })
	handler := s.Routes()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents", "old-token", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-rotation status = %d", rec.Code)
	}
	current = "new-token"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents", "new-token", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-rotation new token status = %d, want 200 without restart", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents", "old-token", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-rotation old token status = %d, want 401", rec.Code)
	}
}
