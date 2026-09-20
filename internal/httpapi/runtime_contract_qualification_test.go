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
	"github.com/kubeneuron/kubeneuron/internal/operations"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// qualificationHTTPFixture is the deterministic inventory and coverage
// builder behind the qualification routes. It runs the real pure coverage
// assessment against a controllable clock so a test can advance time past an
// expiry or drop a report without any live controller state.
type qualificationHTTPFixture struct {
	now            time.Time
	nodes          []*types.Node
	missingReports map[string]bool
}

func newQualificationHTTPFixture() *qualificationHTTPFixture {
	node := func(name, uid, tenant, cluster string) *types.Node {
		return &types.Node{Name: name, UID: uid, Labels: map[string]string{"pool": "a100", "kubeneuron.io/tenant": tenant, "kubeneuron.io/cluster": cluster}}
	}
	return &qualificationHTTPFixture{
		now: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
		nodes: []*types.Node{
			node("gpu-a", "uid-a", "team-a", "east"), node("gpu-b", "uid-b", "team-a", "east"), node("gpu-c", "uid-c", "team-b", "east"),
		},
		missingReports: map[string]bool{},
	}
}

func (f *qualificationHTTPFixture) list(context.Context) ([]*types.Node, error) { return f.nodes, nil }

func (f *qualificationHTTPFixture) build(_ context.Context, name string, vendor types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
	profile := operationalProfile()
	input := config.RuntimeContractCoverageInput{
		Now: f.now, ConfigDigest: "sha256:live-config", NodeName: name, Vendor: vendor,
		AgentLastSeen: f.now.Add(-time.Minute), AgentMaxAge: 5 * time.Minute, Profiles: []config.AcceleratorRuntimeProfile{*profile},
	}
	for _, node := range f.nodes {
		if node.Name == name {
			input.NodeUID, input.NodeLabels = node.UID, node.Labels
		}
	}
	if !f.missingReports[name] {
		input.Report = &types.AgentAcceleratorReport{
			Node: name, NodeUID: input.NodeUID, Vendor: types.AcceleratorVendorNVIDIA, ObservedAt: f.now.Add(-time.Minute),
			ProfileDigest: profile.ProfileDigest, ProfileUID: profile.ProfileUID, ProfileGeneration: profile.ProfileGeneration,
			DriverVersion: profile.DriverVersion, RuntimeVersion: profile.RuntimeVersion,
			TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned, Readiness: types.AcceleratorReadinessReady,
			Devices:      []types.AgentAcceleratorDevice{{ID: "GPU-a", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU}},
			Capabilities: []types.AgentAcceleratorCapability{{Action: types.AcceleratorActionResetDevice, Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice}}},
		}
	}
	return config.AssessRuntimeContractCoverage(input), nil
}

func newQualificationHTTPServer(t *testing.T, authenticators ...OperatorAuthenticator) (*Server, *qualificationHTTPFixture, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := newQualificationHTTPFixture()
	manager := operations.New(operations.Options{
		Resources: st, Workflow: st,
		Now:                          func() time.Time { return fixture.now },
		ListNodes:                    fixture.list,
		BuildRuntimeContractCoverage: fixture.build,
	})
	server := New(&registrationBackend{})
	server.EnableOperatorAPI(&operationalOperator{fakeOperator: &fakeOperator{}, manager: manager}, "secret")
	if len(authenticators) > 0 && authenticators[0] != nil {
		server.SetOperatorAuthenticator(authenticators[0])
	}
	return server, fixture, st
}

const qualificationRoute = "/api/v1/runtime-contract-qualifications"

func qualificationCreateBody(nodes string, extra string) string {
	body := `{"actor":"alice","nodes":[` + nodes + `],"vendor":"nvidia","requirements":{"min_samples":1,"min_duration":"1m"},"expires_at":"2026-09-05T12:20:00Z"`
	if extra != "" {
		body += "," + extra
	}
	return body + "}"
}

// qualificationRequest sends one operator request with the given
// Idempotency-Key ("" omits the header) and decodes a JSON object body.
func qualificationRequest(t *testing.T, handler http.Handler, method, path, token, key, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	request := operatorRequest(method, path, token, body)
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	var decoded map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("%s %s: body is not a JSON object: %v\n%s", method, path, err, rec.Body.String())
		}
	}
	return rec, decoded
}

func createQualification(t *testing.T, handler http.Handler, key, nodes string) map[string]any {
	t.Helper()
	rec, body := qualificationRequest(t, handler, http.MethodPost, qualificationRoute, "secret", key, qualificationCreateBody(nodes, ""))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s = %d %s", key, rec.Code, rec.Body.String())
	}
	return body
}

func TestRuntimeContractQualificationCreateGetObserveLifecycle(t *testing.T) {
	server, fixture, st := newQualificationHTTPServer(t)
	handler := server.Routes()

	rec, created := qualificationRequest(t, handler, http.MethodPost, qualificationRoute, "secret", "create-1", qualificationCreateBody(`"gpu-b","gpu-a"`, `"tenant":"team-a","cluster":"east"`))
	if rec.Code != http.StatusCreated || rec.Header().Get("Idempotent-Replay") != "" {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	id, _ := created["id"].(string)
	if id == "" || rec.Header().Get("Location") != qualificationRoute+"/"+id {
		t.Fatalf("created body/location = %v / %q", created, rec.Header().Get("Location"))
	}
	for key, want := range map[string]any{
		"version": operations.RuntimeContractQualificationVersion, "state": "Observing", "effective_state": "Observing",
		"expired": false, "expiry_pending": false, "ready_for_approval": false, "resource_version": 1.0,
		"actor": "token:alice", "tenant": "team-a", "cluster": "east", "vendor": "nvidia", "evaluated_at": "2026-09-05T12:00:00Z",
	} {
		if created[key] != want {
			t.Errorf("created %s = %v, want %v", key, created[key], want)
		}
	}
	requirements, _ := created["requirements"].(map[string]any)
	if requirements["min_samples"] != 1.0 || requirements["min_duration"] != "1m0s" {
		t.Fatalf("created requirements = %v, want the human-readable duration", created["requirements"])
	}
	cohort, _ := created["cohort"].([]any)
	if len(cohort) != 2 || cohort[0].(map[string]any)["name"] != "gpu-a" || cohort[0].(map[string]any)["uid"] != "uid-a" {
		t.Fatalf("cohort = %v, want sorted frozen UIDs", created["cohort"])
	}

	// Replaying the same key and body returns the same resource, marked.
	rec, replayed := qualificationRequest(t, handler, http.MethodPost, qualificationRoute, "secret", "create-1", qualificationCreateBody(`"gpu-b","gpu-a"`, `"tenant":"team-a","cluster":"east"`))
	if rec.Code != http.StatusCreated || rec.Header().Get("Idempotent-Replay") != "true" || replayed["id"] != id {
		t.Fatalf("replay = %d replay=%q id=%v", rec.Code, rec.Header().Get("Idempotent-Replay"), replayed["id"])
	}
	// The same key with a different request is a conflict, not a second resource.
	rec, _ = qualificationRequest(t, handler, http.MethodPost, qualificationRoute, "secret", "create-1", qualificationCreateBody(`"gpu-a"`, ""))
	if rec.Code != http.StatusConflict {
		t.Fatalf("key reuse = %d %s, want 409", rec.Code, rec.Body.String())
	}

	// GET serves the stored record with the effective projection.
	rec, got := qualificationRequest(t, handler, http.MethodGet, qualificationRoute+"/"+id, "secret", "", "")
	if rec.Code != http.StatusOK || got["id"] != id || got["effective_state"] != "Observing" {
		t.Fatalf("get = %d %v", rec.Code, got)
	}
	rec, _ = qualificationRequest(t, handler, http.MethodGet, qualificationRoute+"/rcq-missing", "secret", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get unknown = %d, want 404", rec.Code)
	}

	// Observe: the first sample counts but the duration bar is not met yet.
	fixture.now = fixture.now.Add(time.Minute)
	rec, observed := qualificationRequest(t, handler, http.MethodPost, qualificationRoute+"/"+id+"/observe", "secret", "observe-1", `{"actor":"bob","resource_version":1}`)
	if rec.Code != http.StatusOK || observed["state"] != "Observing" || observed["successful_samples"] != 1.0 || observed["resource_version"] != 2.0 {
		t.Fatalf("observe 1 = %d %v", rec.Code, observed)
	}
	if last, _ := observed["last_observed_at"].(string); last != "2026-09-05T12:01:00Z" {
		t.Fatalf("last_observed_at = %v", observed["last_observed_at"])
	}
	observations, _ := observed["observations"].([]any)
	if len(observations) != 1 || observations[0].(map[string]any)["actor"] != "token:bob" || observations[0].(map[string]any)["successful"] != true {
		t.Fatalf("observations = %v", observed["observations"])
	}
	// A replay of the observation returns the recorded state without sampling again.
	rec, replayedObserve := qualificationRequest(t, handler, http.MethodPost, qualificationRoute+"/"+id+"/observe", "secret", "observe-1", `{"actor":"bob","resource_version":1}`)
	if rec.Code != http.StatusOK || rec.Header().Get("Idempotent-Replay") != "true" || replayedObserve["total_observations"] != 1.0 || replayedObserve["resource_version"] != 2.0 {
		t.Fatalf("observe replay = %d replay=%q %v", rec.Code, rec.Header().Get("Idempotent-Replay"), replayedObserve)
	}
	// A stale resource_version is an optimistic conflict and writes nothing.
	rec, _ = qualificationRequest(t, handler, http.MethodPost, qualificationRoute+"/"+id+"/observe", "secret", "observe-stale", `{"actor":"bob","resource_version":1}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale observe = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if envelope, err := st.GetOperationalResource(context.Background(), types.ResourceRuntimeContractQualification, id); err != nil || envelope.Version != 2 {
		t.Fatalf("envelope after conflict = (%#v, %v), want version 2", envelope, err)
	}
	// Version 0 means "current"; the second sample past the duration bar is ready.
	fixture.now = fixture.now.Add(time.Minute)
	rec, ready := qualificationRequest(t, handler, http.MethodPost, qualificationRoute+"/"+id+"/observe", "secret", "observe-2", `{"actor":"bob"}`)
	if rec.Code != http.StatusOK || ready["state"] != "ReadyForApproval" || ready["ready_for_approval"] != true || ready["effective_state"] != "ReadyForApproval" || ready["resource_version"] != 3.0 {
		t.Fatalf("observe 2 = %d %v", rec.Code, ready)
	}
	if summary, _ := ready["summary"].(string); !strings.HasPrefix(summary, "ready for approval since 2026-09-05T12:02:00Z until 2026-09-05T12:20:00Z") {
		t.Fatalf("ready summary = %q", summary)
	}
	// Ready is evidence only: no other resource kind was touched.
	events, err := st.ListOperationalAuditEvents(context.Background(), types.OperationalAuditFilter{Limit: 100})
	if err != nil || len(events) == 0 {
		t.Fatalf("audit = (%d, %v)", len(events), err)
	}
	for _, event := range events {
		if event.Kind != types.ResourceRuntimeContractQualification || event.ResourceID != id {
			t.Fatalf("audit touched %s/%s", event.Kind, event.ResourceID)
		}
	}
}

// The safety property of the read surface: a qualification stored as
// ReadyForApproval after its wall-clock expiry is presented as Expired by GET
// and by the list page, GET writes nothing, and only an explicit observation
// persists the transition.
func TestRuntimeContractQualificationGetReportsEffectiveExpiryWithoutWrite(t *testing.T) {
	server, fixture, st := newQualificationHTTPServer(t)
	handler := server.Routes()
	ctx := context.Background()
	created := createQualification(t, handler, "expiry", `"gpu-a"`)
	id := created["id"].(string)
	fixture.now = fixture.now.Add(time.Minute)
	if rec, _ := qualificationRequest(t, handler, http.MethodPost, qualificationRoute+"/"+id+"/observe", "secret", "x1", `{"actor":"bob"}`); rec.Code != http.StatusOK {
		t.Fatalf("observe 1 = %d %s", rec.Code, rec.Body.String())
	}
	fixture.now = fixture.now.Add(time.Minute)
	rec, ready := qualificationRequest(t, handler, http.MethodPost, qualificationRoute+"/"+id+"/observe", "secret", "x2", `{"actor":"bob"}`)
	if rec.Code != http.StatusOK || ready["state"] != "ReadyForApproval" {
		t.Fatalf("observe 2 = %d %v", rec.Code, ready)
	}
	before, err := st.GetOperationalResource(ctx, types.ResourceRuntimeContractQualification, id)
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, _ := st.ListOperationalAudit(ctx, types.ResourceRuntimeContractQualification, id, 100)

	// The window closes. Stored state is still ReadyForApproval.
	fixture.now = time.Date(2026, 9, 5, 12, 20, 0, 0, time.UTC)
	rec, got := qualificationRequest(t, handler, http.MethodGet, qualificationRoute+"/"+id, "secret", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	for key, want := range map[string]any{
		"state": "ReadyForApproval", "effective_state": "Expired", "expired": true, "expiry_pending": true, "ready_for_approval": false,
		"evaluated_at": "2026-09-05T12:20:00Z", "resource_version": 3.0,
	} {
		if got[key] != want {
			t.Errorf("get after expiry %s = %v, want %v", key, got[key], want)
		}
	}
	if _, present := got["expired_at"]; present {
		t.Fatalf("expired_at must stay absent until an observation records it: %v", got)
	}
	if summary, _ := got["summary"].(string); !strings.Contains(summary, "expired at 2026-09-05T12:20:00Z while stored as ReadyForApproval") || !strings.Contains(summary, "not ready for approval") {
		t.Fatalf("summary = %q", summary)
	}
	// The list page projects the same effective state.
	rec, page := qualificationRequest(t, handler, http.MethodGet, qualificationRoute, "secret", "", "")
	items, _ := page["items"].([]any)
	if rec.Code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["effective_state"] != "Expired" || items[0].(map[string]any)["ready_for_approval"] != false || items[0].(map[string]any)["state"] != "ReadyForApproval" {
		t.Fatalf("list after expiry = %d %v", rec.Code, page)
	}
	// Excluding expired rows hides it: the envelope expiry is the wall clock's.
	rec, page = qualificationRequest(t, handler, http.MethodGet, qualificationRoute+"?include_expired=false", "secret", "", "")
	if items, _ := page["items"].([]any); rec.Code != http.StatusOK || len(items) != 0 {
		t.Fatalf("list excluding expired = %d %v, want an empty page", rec.Code, page)
	}
	// Reads wrote nothing: same version, same payload, same audit chain.
	after, err := st.GetOperationalResource(ctx, types.ResourceRuntimeContractQualification, id)
	if err != nil || after.Version != before.Version || after.State != before.State || string(after.Payload) != string(before.Payload) || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("GET changed the stored row:\nbefore %#v\nafter  %#v (%v)", before, after, err)
	}
	if auditAfter, _ := st.ListOperationalAudit(ctx, types.ResourceRuntimeContractQualification, id, 100); len(auditAfter) != len(auditBefore) {
		t.Fatalf("GET appended audit events: %d -> %d", len(auditBefore), len(auditAfter))
	}

	// Only an explicit observation persists Expired.
	rec, expired := qualificationRequest(t, handler, http.MethodPost, qualificationRoute+"/"+id+"/observe", "secret", "x3", `{"actor":"bob","resource_version":3}`)
	if rec.Code != http.StatusOK || expired["state"] != "Expired" || expired["effective_state"] != "Expired" || expired["expiry_pending"] != false || expired["expired"] != true || expired["resource_version"] != 4.0 {
		t.Fatalf("expiring observe = %d %v", rec.Code, expired)
	}
	if expired["expired_at"] != "2026-09-05T12:20:00Z" || expired["total_observations"] != 2.0 {
		t.Fatalf("expired record = %v, want expired_at recorded without a new sample", expired)
	}
	// A terminal qualification refuses further observations.
	rec, _ = qualificationRequest(t, handler, http.MethodPost, qualificationRoute+"/"+id+"/observe", "secret", "x4", `{"actor":"bob"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("observe after expiry = %d %s, want 409", rec.Code, rec.Body.String())
	}
}

func TestRuntimeContractQualificationRoutesRequireAuthorizationAndLeadership(t *testing.T) {
	server, _, _ := newQualificationHTTPServer(t)
	handler := server.Routes()
	created := createQualification(t, handler, "auth", `"gpu-a"`)
	id := created["id"].(string)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, qualificationRoute, qualificationCreateBody(`"gpu-a"`, "")},
		{http.MethodGet, qualificationRoute, ""},
		{http.MethodGet, qualificationRoute + "/" + id, ""},
		{http.MethodPost, qualificationRoute + "/" + id + "/observe", `{"actor":"bob"}`},
	} {
		for _, token := range []string{"", "wrong"} {
			rec, _ := qualificationRequest(t, handler, tc.method, tc.path, token, "k", tc.body)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q = %d, want 401", tc.method, tc.path, token, rec.Code)
			}
		}
	}
	// Without the operations provider every route fails closed.
	plain := operatorServer(&fakeOperator{}, "secret")
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, qualificationRoute}, {http.MethodGet, qualificationRoute},
		{http.MethodGet, qualificationRoute + "/" + id}, {http.MethodPost, qualificationRoute + "/" + id + "/observe"},
	} {
		rec, _ := qualificationRequest(t, plain, tc.method, tc.path, "secret", "k", `{"actor":"bob"}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without provider = %d, want 503", tc.method, tc.path, rec.Code)
		}
	}
	// Mutations are fenced to the elected leader; reads keep serving.
	server.SetReadyCheck(func() bool { return false })
	rec, _ := qualificationRequest(t, handler, http.MethodPost, qualificationRoute, "secret", "standby-create", qualificationCreateBody(`"gpu-b"`, ""))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "standby") {
		t.Fatalf("standby create = %d %s, want 503", rec.Code, rec.Body.String())
	}
	rec, _ = qualificationRequest(t, handler, http.MethodPost, qualificationRoute+"/"+id+"/observe", "secret", "standby-observe", `{"actor":"bob"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("standby observe = %d, want 503", rec.Code)
	}
	rec, page := qualificationRequest(t, handler, http.MethodGet, qualificationRoute, "secret", "", "")
	if items, _ := page["items"].([]any); rec.Code != http.StatusOK || len(items) != 1 {
		t.Fatalf("standby list = %d %v, want the one existing qualification", rec.Code, page)
	}
	rec, _ = qualificationRequest(t, handler, http.MethodGet, qualificationRoute+"/"+id, "secret", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("standby get = %d", rec.Code)
	}
	server.SetReadyCheck(nil)
	if rec, _ := qualificationRequest(t, handler, http.MethodGet, qualificationRoute+"/"+id, "secret", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("get after standby = %d", rec.Code)
	}

	// A per-caller authenticator sees "get" for reads and "update" for
	// mutations, and its verified identity is the audited actor: the body
	// actor cannot override it.
	auth := &fakeOperatorAuthenticator{identity: OperatorIdentity{Actor: "sre-bot", Method: "kubernetes"}}
	verified, _, _ := newQualificationHTTPServer(t, auth)
	verifiedHandler := verified.Routes()
	rec, body := qualificationRequest(t, verifiedHandler, http.MethodPost, qualificationRoute, "per-caller", "verified-create", qualificationCreateBody(`"gpu-a"`, ""))
	if rec.Code != http.StatusCreated || body["actor"] != "sre-bot" {
		t.Fatalf("verified create = %d %v", rec.Code, body)
	}
	rec, body = qualificationRequest(t, verifiedHandler, http.MethodPost, qualificationRoute+"/"+body["id"].(string)+"/observe", "per-caller", "verified-observe", `{"actor":"mallory"}`)
	observations, _ := body["observations"].([]any)
	if rec.Code != http.StatusOK || len(observations) != 1 || observations[0].(map[string]any)["actor"] != "sre-bot" {
		t.Fatalf("verified observe = %d %v", rec.Code, body)
	}
	if rec, _ := qualificationRequest(t, verifiedHandler, http.MethodGet, qualificationRoute, "per-caller", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("verified list = %d", rec.Code)
	}
	if strings.Join(auth.verbs, ",") != "update,update,get" {
		t.Fatalf("authorization verbs = %v, want update for mutations and get for reads", auth.verbs)
	}
}

func TestRuntimeContractQualificationStrictDecodeAndIdempotencyKey(t *testing.T) {
	server, _, st := newQualificationHTTPServer(t)
	handler := server.Routes()
	created := createQualification(t, handler, "strict", `"gpu-a"`)
	id := created["id"].(string)
	observe := qualificationRoute + "/" + id + "/observe"

	// Missing Idempotency-Key is rejected before anything is decoded.
	for _, tc := range []struct{ path, body string }{{qualificationRoute, qualificationCreateBody(`"gpu-b"`, "")}, {observe, `{"actor":"bob"}`}} {
		rec, _ := qualificationRequest(t, handler, http.MethodPost, tc.path, "secret", "", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Idempotency-Key header is required") {
			t.Errorf("POST %s without key = %d %s", tc.path, rec.Code, rec.Body.String())
		}
	}
	cases := []struct {
		name, path, body, want string
	}{
		{"create unknown field", qualificationRoute, qualificationCreateBody(`"gpu-b"`, `"state":"ReadyForApproval"`), "unknown field"},
		{"create client bindings", qualificationRoute, qualificationCreateBody(`"gpu-b"`, `"cohort":[{"name":"gpu-b","uid":"forged"}]`), "unknown field"},
		{"create nanosecond duration", qualificationRoute, `{"actor":"alice","nodes":["gpu-b"],"vendor":"nvidia","requirements":{"min_samples":1,"min_duration":60000000000},"expires_at":"2026-09-05T12:20:00Z"}`, "min_duration"},
		{"create bad duration", qualificationRoute, `{"actor":"alice","nodes":["gpu-b"],"vendor":"nvidia","requirements":{"min_samples":1,"min_duration":"soon"},"expires_at":"2026-09-05T12:20:00Z"}`, "requirements.min_duration must be a positive Go duration"},
		{"create negative duration", qualificationRoute, `{"actor":"alice","nodes":["gpu-b"],"vendor":"nvidia","requirements":{"min_samples":1,"min_duration":"-1m"},"expires_at":"2026-09-05T12:20:00Z"}`, "requirements.min_duration must be a positive Go duration"},
		{"create bad expiry", qualificationRoute, `{"actor":"alice","nodes":["gpu-b"],"vendor":"nvidia","requirements":{"min_samples":1,"min_duration":"1m"},"expires_at":"tomorrow"}`, "bad request"},
		{"create missing expiry", qualificationRoute, `{"actor":"alice","nodes":["gpu-b"],"vendor":"nvidia","requirements":{"min_samples":1,"min_duration":"1m"}}`, "expiry is required"},
		{"create zero samples", qualificationRoute, `{"actor":"alice","nodes":["gpu-b"],"vendor":"nvidia","requirements":{"min_samples":0,"min_duration":"1m"},"expires_at":"2026-09-05T12:20:00Z"}`, "min_samples"},
		{"create bad vendor", qualificationRoute, `{"actor":"alice","nodes":["gpu-b"],"vendor":"cuda","requirements":{"min_samples":1,"min_duration":"1m"},"expires_at":"2026-09-05T12:20:00Z"}`, "vendor must be"},
		{"create trailing json", qualificationRoute, qualificationCreateBody(`"gpu-b"`, "") + `{}`, "multiple JSON values"},
		{"create missing actor", qualificationRoute, `{"nodes":["gpu-b"],"vendor":"nvidia","requirements":{"min_samples":1,"min_duration":"1m"},"expires_at":"2026-09-05T12:20:00Z"}`, "actor is required"},
		{"observe unknown field", observe, `{"actor":"bob","successful":true}`, "unknown field"},
		{"observe client evidence", observe, `{"actor":"bob","coverage":[]}`, "unknown field"},
		{"observe missing actor", observe, `{}`, "actor is required"},
		{"observe not json", observe, `actor=bob`, "bad request"},
	}
	for _, tc := range cases {
		rec, _ := qualificationRequest(t, handler, http.MethodPost, tc.path, "secret", "strict-"+tc.name, tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s = %d %q, want 400 containing %q", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
	// Unknown nodes are an incomplete inventory, never a partial cohort.
	rec, _ := qualificationRequest(t, handler, http.MethodPost, qualificationRoute, "secret", "ghost", qualificationCreateBody(`"gpu-a","ghost"`, ""))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown node = %d %s, want 422", rec.Code, rec.Body.String())
	}
	// A cohort outside the asserted scope is refused.
	rec, _ = qualificationRequest(t, handler, http.MethodPost, qualificationRoute, "secret", "scope", qualificationCreateBody(`"gpu-a","gpu-c"`, `"tenant":"team-a"`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("cross-scope cohort = %d %s, want 409", rec.Code, rec.Body.String())
	}
	// None of the rejected requests reached the store.
	items, err := st.ListOperationalResources(context.Background(), types.OperationalResourceFilter{Kind: types.ResourceRuntimeContractQualification, IncludeExpired: true})
	if err != nil || len(items) != 1 || items[0].Version != 1 {
		t.Fatalf("stored qualifications = (%d, %v), want only the original at version 1", len(items), err)
	}
}

func TestRuntimeContractQualificationListPaginationAndScope(t *testing.T) {
	server, fixture, _ := newQualificationHTTPServer(t)
	handler := server.Routes()
	// Distinct creation instants make the (created_at, id) page order fixed.
	first := createQualification(t, handler, "p1", `"gpu-a"`)["id"].(string)
	fixture.now = fixture.now.Add(time.Second)
	second := createQualification(t, handler, "p2", `"gpu-b"`)["id"].(string)
	fixture.now = fixture.now.Add(time.Second)
	third := createQualification(t, handler, "p3", `"gpu-c"`)["id"].(string)

	ids := func(page map[string]any) string {
		items, _ := page["items"].([]any)
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.(map[string]any)["id"].(string))
		}
		return strings.Join(out, ",")
	}
	get := func(path string) map[string]any {
		t.Helper()
		rec, page := qualificationRequest(t, handler, http.MethodGet, path, "secret", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
		return page
	}
	all := get(qualificationRoute)
	if ids(all) != first+","+second+","+third || all["next_cursor"] != nil {
		t.Fatalf("full page = %v", all)
	}
	page1 := get(qualificationRoute + "?limit=2")
	cursor, _ := page1["next_cursor"].(string)
	if ids(page1) != first+","+second || cursor == "" {
		t.Fatalf("page 1 = %v", page1)
	}
	if strings.Contains(cursor, second) {
		t.Fatalf("cursor %q leaks the resource ID; it must be opaque", cursor)
	}
	page2 := get(qualificationRoute + "?limit=2&cursor=" + cursor)
	if ids(page2) != third || page2["next_cursor"] != nil {
		t.Fatalf("page 2 = %v", page2)
	}
	if ids(get(qualificationRoute+"?tenant=team-a")) != first+","+second || ids(get(qualificationRoute+"?tenant=team-b&cluster=east")) != third || ids(get(qualificationRoute+"?cluster=west")) != "" {
		t.Fatal("tenant/cluster scope filters are not applied")
	}
	if ids(get(qualificationRoute+"?state=Observing&limit=1")) != first || ids(get(qualificationRoute+"?state=Expired")) != "" {
		t.Fatal("state filter is not applied")
	}
	if ids(get(qualificationRoute+"?since=2026-09-05T12:00:01Z")) != second+","+third || ids(get(qualificationRoute+"?until=2026-09-05T12:00:00Z")) != first {
		t.Fatal("creation-time bounds are not applied")
	}
	for _, path := range []string{qualificationRoute + "?limit=0", qualificationRoute + "?limit=501", qualificationRoute + "?cursor=%21%21", qualificationRoute + "?include_expired=maybe", qualificationRoute + "?since=yesterday", qualificationRoute + "?since=2026-09-05T12:00:01Z&until=2026-09-05T12:00:00Z"} {
		if rec, _ := qualificationRequest(t, handler, http.MethodGet, path, "secret", "", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", path, rec.Code)
		}
	}
}

func TestRuntimeContractQualificationMutationsAreRateLimitedPerSource(t *testing.T) {
	server, _, _ := newQualificationHTTPServer(t)
	handler := server.Routes()
	send := func(addr, key, body string) *httptest.ResponseRecorder {
		request := operatorRequest(http.MethodPost, qualificationRoute, "secret", body)
		request.RemoteAddr = addr
		request.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request)
		return rec
	}
	// Creation shares the 20/min creation quota; the body is deliberately
	// invalid so the quota is counted without creating twenty resources.
	for i := 0; i < 20; i++ {
		if rec := send("203.0.113.9:4000", "rl-"+strconv.Itoa(i), `{"actor":"alice"}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("request %d = %d %s, want 400 from validation", i, rec.Code, rec.Body.String())
		}
	}
	rec := send("203.0.113.9:4001", "rl-over", `{"actor":"alice"}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("21st request = %d retry-after=%q, want 429", rec.Code, rec.Header().Get("Retry-After"))
	}
	// Another source keeps its own quota, and a valid request from it succeeds.
	if rec := send("203.0.113.10:4000", "rl-other", qualificationCreateBody(`"gpu-a"`, "")); rec.Code != http.StatusCreated {
		t.Fatalf("other source = %d %s", rec.Code, rec.Body.String())
	}
}

// TestRuntimeContractQualificationObserveHasIndependentPerSourceQuota pins
// the release-plan bound on observation: 30/min per source, counted on its own
// operation key rather than shared with creation. Every counted request is a
// real, successful observation with its own idempotency key, so a 429 can only
// come from the limiter and not from a validation or version-conflict error
// that happened to arrive after the quota was spent.
func TestRuntimeContractQualificationObserveHasIndependentPerSourceQuota(t *testing.T) {
	server, _, _ := newQualificationHTTPServer(t)
	handler := server.Routes()
	created := createQualification(t, handler, "observe-quota-create", `"gpu-a"`)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %v", created)
	}
	observePath := qualificationRoute + "/" + id + "/observe"
	observe := func(addr, key string) *httptest.ResponseRecorder {
		// No resource_version: the observation is unconditional, so nothing
		// but the limiter can refuse it.
		request := operatorRequest(http.MethodPost, observePath, "secret", `{"actor":"alice"}`)
		request.RemoteAddr = addr
		request.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request)
		return rec
	}

	const observeQuota = 30
	for i := 0; i < observeQuota; i++ {
		rec := observe("203.0.113.20:"+strconv.Itoa(5000+i), "observe-quota-"+strconv.Itoa(i))
		if rec.Code != http.StatusOK || rec.Header().Get("Idempotent-Replay") != "" {
			t.Fatalf("observation %d = %d replay=%q %s, want a fresh 200 inside the quota", i+1, rec.Code, rec.Header().Get("Idempotent-Replay"), rec.Body.String())
		}
	}
	rec := observe("203.0.113.20:5999", "observe-quota-over")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("observation %d = %d %s, want 429 from the per-source quota", observeQuota+1, rec.Code, rec.Body.String())
	}
	if retry, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || retry < 1 {
		t.Fatalf("429 Retry-After = %q, want a positive integer number of seconds", rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "quota exceeded") {
		t.Fatalf("429 body = %q, want the operational quota message", rec.Body.String())
	}

	// Another source keeps its own observation quota on the same resource.
	if rec := observe("203.0.113.21:5000", "observe-quota-other-source"); rec.Code != http.StatusOK {
		t.Fatalf("other source observation = %d %s, want 200", rec.Code, rec.Body.String())
	}
	// The exhausted source is still refused: the other source did not reset it.
	if rec := observe("203.0.113.20:6000", "observe-quota-still-over"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted source after another source observed = %d, want 429", rec.Code)
	}
	// The quota is observe's own: the same exhausted source may still read
	// the resource and still spend its separate creation quota.
	getRequest := operatorRequest(http.MethodGet, qualificationRoute+"/"+id, "secret", "")
	getRequest.RemoteAddr = "203.0.113.20:6001"
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getRequest)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get from the observe-exhausted source = %d %s, want 200", getRec.Code, getRec.Body.String())
	}
	createRequest := operatorRequest(http.MethodPost, qualificationRoute, "secret", qualificationCreateBody(`"gpu-b"`, ""))
	createRequest.RemoteAddr = "203.0.113.20:6002"
	createRequest.Header.Set("Idempotency-Key", "observe-quota-create-after")
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createRequest)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create from the observe-exhausted source = %d %s, want 201 on the independent creation quota", createRec.Code, createRec.Body.String())
	}
}

// The qualification surface is evidence-only: it has no route that could
// approve, promote, delete, apply, or enable anything, and the mux answers
// such paths with 404/405 rather than any handler.
func TestRuntimeContractQualificationHasNoAuthorityRoutes(t *testing.T) {
	server, _, st := newQualificationHTTPServer(t)
	handler := server.Routes()
	created := createQualification(t, handler, "authority", `"gpu-a"`)
	id := created["id"].(string)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, qualificationRoute + "/" + id + "/approve"},
		{http.MethodPost, qualificationRoute + "/" + id + "/promote"},
		{http.MethodPost, qualificationRoute + "/" + id + "/apply"},
		{http.MethodPost, qualificationRoute + "/" + id + "/enable"},
		{http.MethodPost, qualificationRoute + "/" + id + "/authority"},
		{http.MethodPost, qualificationRoute + "/" + id + "/expire"},
		{http.MethodPost, qualificationRoute + "/" + id + "/invalidate"},
		{http.MethodPost, qualificationRoute + "/" + id},
		{http.MethodPut, qualificationRoute + "/" + id},
		{http.MethodPatch, qualificationRoute + "/" + id},
		{http.MethodDelete, qualificationRoute + "/" + id},
		{http.MethodDelete, qualificationRoute},
		{http.MethodPut, qualificationRoute},
	} {
		rec, _ := qualificationRequest(t, handler, tc.method, tc.path, "secret", "authority-"+tc.method, `{"actor":"alice","role":"platform"}`)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 404 or 405: the qualification surface must not carry authority", tc.method, tc.path, rec.Code)
		}
	}
	envelope, err := st.GetOperationalResource(context.Background(), types.ResourceRuntimeContractQualification, id)
	if err != nil || envelope.Version != 1 || envelope.State != string(operations.QualificationObserving) {
		t.Fatalf("envelope after authority probes = (%#v, %v), want untouched", envelope, err)
	}
	// Nothing outside the qualification partition exists either.
	for _, kind := range []types.OperationalResourceKind{types.ResourceAutonomyPlan, types.ResourceAutonomyRollout, types.ResourceAutonomyEffect, types.ResourceRemediationSimulation} {
		if items, err := st.ListOperationalResources(context.Background(), types.OperationalResourceFilter{Kind: kind, IncludeExpired: true}); err != nil || len(items) != 0 {
			t.Fatalf("%s resources = (%d, %v), want none", kind, len(items), err)
		}
	}
}
