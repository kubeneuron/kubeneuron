package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type registrationBackend struct {
	arming        types.AgentArming
	err           error
	signalErr     error
	registrations []types.AgentRegistration
	events        []types.AgentEvent
	signals       []types.Signal
}

type fixedAgentAuthenticator struct {
	principal AgentPrincipal
	err       error
}

func (a fixedAgentAuthenticator) AuthenticateAgent(*http.Request) (AgentPrincipal, error) {
	return a.principal, a.err
}

func authenticatedAgentRoutes(backend Backend) http.Handler {
	return New(backend).AgentRoutes(fixedAgentAuthenticator{principal: AgentPrincipal{NodeName: "node-a", NodeUID: "node-uid-a"}})
}

func (b *registrationBackend) HandleSignal(_ *http.Request, sig types.Signal) {
	b.signals = append(b.signals, sig)
}

func (b *registrationBackend) IngestSignal(_ context.Context, sig types.Signal) error {
	b.signals = append(b.signals, sig)
	return b.signalErr
}

func (b *registrationBackend) HandleAgentEvent(_ *http.Request, event types.AgentEvent) error {
	b.events = append(b.events, event)
	return b.err
}

func (b *registrationBackend) RegisterNode(_ *http.Request, registration types.AgentRegistration) (types.AgentArming, error) {
	b.registrations = append(b.registrations, registration)
	return b.arming, b.err
}

func TestRegister(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		backendErr error
		wantStatus int
		wantCalls  int
	}{
		{
			name:       "durable success",
			body:       `{"name":"node-a","gpus":[{"index":0,"uuid":"GPU-a"}],"boot_id":"boot-a"}`,
			wantStatus: http.StatusNoContent,
			wantCalls:  1,
		},
		{
			name:       "persistence failure",
			body:       `{"name":"node-a"}`,
			backendErr: errors.New("database unavailable"),
			wantStatus: http.StatusServiceUnavailable,
			wantCalls:  1,
		},
		{
			name:       "unknown field",
			body:       `{"name":"node-a","platform":"baremetal"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing name",
			body:       `{"boot_id":"boot-a"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "whitespace name",
			body:       `{"name":"  \t\n "}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "name with surrounding whitespace",
			body:       `{"name":" node-a "}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "malformed JSON",
			body:       `{"name":`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "trailing JSON value",
			body:       `{"name":"node-a"} {"name":"node-b"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "oversized payload",
			body:       `{"name":"node-a","boot_id":"` + strings.Repeat("x", maxAgentRegistrationBytes) + `"}`,
			wantStatus: http.StatusRequestEntityTooLarge,
		},
		{
			name:       "oversized trailing payload",
			body:       `{"name":"node-a"}` + strings.Repeat(" ", maxAgentRegistrationBytes+1),
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &registrationBackend{err: tt.backendErr}
			req := httptest.NewRequest(http.MethodPost, types.AgentRegistrationPath, strings.NewReader(tt.body))
			res := httptest.NewRecorder()

			authenticatedAgentRoutes(backend).ServeHTTP(res, req)

			if res.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body = %q", res.Code, tt.wantStatus, res.Body.String())
			}
			if len(backend.registrations) != tt.wantCalls {
				t.Errorf("RegisterNode calls = %d, want %d", len(backend.registrations), tt.wantCalls)
			}
			if tt.wantStatus == http.StatusNoContent && backend.registrations[0].NodeUID != "node-uid-a" {
				t.Errorf("registration NodeUID = %q, want server-stamped node-uid-a", backend.registrations[0].NodeUID)
			}
			if tt.wantStatus == http.StatusNoContent && res.Body.Len() != 0 {
				t.Errorf("204 response body = %q, want empty", res.Body.String())
			}
		})
	}
}

func TestAlertmanagerPayloadIsBounded(t *testing.T) {
	backend := &registrationBackend{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/alertmanager", strings.NewReader(
		`{"alerts":[]}`+strings.Repeat(" ", maxAlertmanagerBytes+1),
	))
	response := httptest.NewRecorder()
	// The webhook fails closed without a token; opt into the development-only
	// insecure mode so this test exercises the body-size guard, not the auth gate.
	s := New(backend)
	s.AllowInsecureWebhook()
	s.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %q", response.Code, response.Body.String())
	}
}

func TestAlertmanagerOnlyAcknowledgesDurableSignalIngest(t *testing.T) {
	backend := &registrationBackend{signalErr: errors.New("database unavailable")}
	s := New(backend)
	s.AllowInsecureWebhook()
	// GpuRowRemapFailure is a built-in normalized signal, so the request reaches
	// the durable backend instead of being ignored as an unknown alert.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/alertmanager",
		strings.NewReader(`{"alerts":[{"status":"firing","labels":{"alertname":"GpuRowRemapFailure","node":"node-a"}}]}`))
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("durable ingest failure status = %d, want 503", rec.Code)
	}
	if len(backend.signals) != 1 {
		t.Fatalf("durable ingest calls = %d, want 1", len(backend.signals))
	}
}

func TestAgentRoutesRejectDirectStandbyAccess(t *testing.T) {
	backend := &registrationBackend{}
	s := New(backend)
	s.SetReadyCheck(func() bool { return false })
	handler := s.AgentRoutes(fixedAgentAuthenticator{principal: AgentPrincipal{NodeName: "node-a", NodeUID: "node-uid-a"}})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, types.AgentRegistrationPath, strings.NewReader(`{"name":"node-a"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("standby agent route status = %d, want 503", rec.Code)
	}
	if len(backend.registrations) != 0 {
		t.Fatal("standby must not persist an agent registration")
	}
}

func TestAgentRoutesAuthenticateAndBindBodyNode(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		body       string
		principal  AgentPrincipal
		authErr    error
		wantStatus int
		wantCalls  int
	}{
		{
			name:       "registration node mismatch",
			path:       types.AgentRegistrationPath,
			body:       `{"name":"node-b"}`,
			principal:  AgentPrincipal{NodeName: "node-a"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "event node mismatch",
			path:       "/api/v1/events",
			body:       `{"node":"node-b","xid":79}`,
			principal:  AgentPrincipal{NodeName: "node-a"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "authenticated event",
			path:       "/api/v1/events",
			body:       `{"node":"node-a","xid":79}`,
			principal:  AgentPrincipal{NodeName: "node-a"},
			wantStatus: http.StatusAccepted,
			wantCalls:  1,
		},
		{
			// Exactly one fault identity per event: classification is
			// Fault-first, so a nonzero XID beside a Fault would be silently
			// ignored. Ambiguity is rejected, never interpreted.
			name:       "event with both an XID and a neutral fault",
			path:       "/api/v1/events",
			body:       `{"node":"node-a","xid":79,"fault":{"vendor":"nvidia","source":"nvidia-smi","code":"ecc-dbe"}}`,
			principal:  AgentPrincipal{NodeName: "node-a"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "neutral-fault event",
			path:       "/api/v1/events",
			body:       `{"node":"node-a","xid":0,"fault":{"vendor":"nvidia","source":"nvidia-smi","code":"ecc-dbe"}}`,
			principal:  AgentPrincipal{NodeName: "node-a"},
			wantStatus: http.StatusAccepted,
			wantCalls:  1,
		},
		{
			name:       "missing credentials",
			path:       types.AgentRegistrationPath,
			body:       `{"name":"node-a"}`,
			authErr:    testHTTPStatusError{status: http.StatusUnauthorized},
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &registrationBackend{}
			handler := New(backend).AgentRoutes(fixedAgentAuthenticator{principal: tt.principal, err: tt.authErr})
			request := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, tt.wantStatus, response.Body.String())
			}
			calls := len(backend.registrations) + len(backend.events)
			if calls != tt.wantCalls {
				t.Fatalf("backend mutation calls = %d, want %d", calls, tt.wantCalls)
			}
			if tt.wantStatus == http.StatusUnauthorized && response.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("401 response is missing WWW-Authenticate")
			}
		})
	}
}

func TestAgentEventRequiresDurableBackendAcknowledgment(t *testing.T) {
	backend := &registrationBackend{err: errors.New("database unavailable")}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/events", strings.NewReader(`{"node":"node-a","xid":79}`))
	response := httptest.NewRecorder()

	authenticatedAgentRoutes(backend).ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if len(backend.events) != 1 {
		t.Fatalf("HandleAgentEvent calls = %d, want 1", len(backend.events))
	}
}

func TestPublicRoutesDoNotExposeAgentEndpoints(t *testing.T) {
	backend := &registrationBackend{}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, types.AgentRegistrationPath, nil),
		httptest.NewRequest(http.MethodPost, types.AgentRegistrationPath, strings.NewReader(`{"name":"node-a"}`)),
		httptest.NewRequest(http.MethodPost, "/api/v1/events", strings.NewReader(`{"node":"node-a","xid":79}`)),
	} {
		response := httptest.NewRecorder()
		New(backend).Routes().ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", request.Method, request.URL.Path, response.Code)
		}
	}
	if len(backend.registrations)+len(backend.events) != 0 {
		t.Fatal("public listener invoked an agent backend")
	}
}

func TestAgentRoutesDoNotExposePublicEndpoints(t *testing.T) {
	backend := &registrationBackend{}
	handler := authenticatedAgentRoutes(backend)
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/healthz", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/alertmanager", strings.NewReader(`{"alerts":[]}`)),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", request.Method, request.URL.Path, response.Code)
		}
	}
}

type testHTTPStatusError struct{ status int }

func (e testHTTPStatusError) Error() string   { return http.StatusText(e.status) }
func (e testHTTPStatusError) HTTPStatus() int { return e.status }

func TestRegistrationCapability(t *testing.T) {
	backend := &registrationBackend{}
	req := httptest.NewRequest(http.MethodGet, types.AgentRegistrationPath, nil)
	res := httptest.NewRecorder()

	authenticatedAgentRoutes(backend).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
	if got, want := res.Body.String(), types.AgentRegistrationProtocol+"\n"; got != want {
		t.Fatalf("body = %q, want exact capability %q", got, want)
	}
	if got := res.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if len(backend.registrations) != 0 {
		t.Fatalf("RegisterNode calls = %d, want 0", len(backend.registrations))
	}
}

func TestRegisterPassesNarrowPayload(t *testing.T) {
	backend := &registrationBackend{}
	req := httptest.NewRequest(http.MethodPost, types.AgentRegistrationPath, strings.NewReader(
		`{"name":"node-a","gpus":[{"index":2,"uuid":"GPU-a","model":"test"}],"boot_id":"boot-a"}`,
	))
	res := httptest.NewRecorder()

	authenticatedAgentRoutes(backend).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %q", res.Code, http.StatusNoContent, res.Body.String())
	}
	if len(backend.registrations) != 1 {
		t.Fatalf("RegisterNode calls = %d, want 1", len(backend.registrations))
	}
	got := backend.registrations[0]
	if got.Name != "node-a" || got.BootID != "boot-a" || len(got.GPUs) != 1 || got.GPUs[0].UUID != "GPU-a" {
		t.Errorf("registration = %+v, want decoded narrow payload", got)
	}
}

func TestLegacyRegistrationPathFailsClosed(t *testing.T) {
	backend := &registrationBackend{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", strings.NewReader(
		`{"name":"node-a","platform":"baremetal","paused":true}`,
	))
	res := httptest.NewRecorder()

	New(backend).Routes().ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("legacy path status = %d, want %d", res.Code, http.StatusNotFound)
	}
	if len(backend.registrations) != 0 {
		t.Fatalf("RegisterNode calls = %d, want 0", len(backend.registrations))
	}
}

// Action-queue stubs so registrationBackend keeps satisfying Backend.
func (b *registrationBackend) NextAction(*http.Request, string) (*types.QueuedAction, error) {
	return nil, nil
}

func (b *registrationBackend) CompleteAction(*http.Request, string, string, string, types.ActionResult) error {
	return nil
}

// Round-7 item C: the v2 registration protocol requires the arming
// declaration; the v1 protocol must keep REJECTING it — the field is known to
// the shared Go struct, so without the explicit check strict decoding alone
// would quietly erode v1's mixed-version corruption guard.
func TestRegistrationV2RequiresArmingAndV1RejectsIt(t *testing.T) {
	armed := true
	v2Body := func(a *bool) string {
		reg := types.AgentRegistration{Name: "node-a", DestructiveArmed: a}
		b, _ := json.Marshal(reg)
		return string(b)
	}

	backend := &registrationBackend{}
	routes := New(backend).AgentRoutes(fixedAgentAuthenticator{principal: AgentPrincipal{NodeName: "node-a"}})

	// v2 capability serves its exact token.
	capability := httptest.NewRecorder()
	routes.ServeHTTP(capability, httptest.NewRequest(http.MethodGet, types.AgentRegistrationV2Path, nil))
	if capability.Code != http.StatusOK || capability.Body.String() != types.AgentRegistrationV2Protocol+"\n" {
		t.Fatalf("v2 capability = %d %q", capability.Code, capability.Body.String())
	}

	for _, tt := range []struct {
		name, path, body string
		wantStatus       int
		wantArming       *bool
	}{
		{"v2 with arming accepted", types.AgentRegistrationV2Path, v2Body(&armed), http.StatusNoContent, &armed},
		{"v2 without arming rejected", types.AgentRegistrationV2Path, v2Body(nil), http.StatusBadRequest, nil},
		{"v1 with arming rejected", types.AgentRegistrationPath, v2Body(&armed), http.StatusBadRequest, nil},
		{"v1 without arming accepted as unknown", types.AgentRegistrationPath, v2Body(nil), http.StatusNoContent, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(backend.registrations)
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body)))
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %q", response.Code, tt.wantStatus, response.Body.String())
			}
			if tt.wantStatus != http.StatusNoContent {
				if len(backend.registrations) != before {
					t.Fatal("a rejected registration must not reach the backend")
				}
				return
			}
			got := backend.registrations[len(backend.registrations)-1]
			switch {
			case tt.wantArming == nil && got.DestructiveArmed != nil:
				t.Fatalf("arming = %v, want absent (unknown)", *got.DestructiveArmed)
			case tt.wantArming != nil && (got.DestructiveArmed == nil || *got.DestructiveArmed != *tt.wantArming):
				t.Fatalf("arming = %v, want %v", got.DestructiveArmed, *tt.wantArming)
			}
		})
	}
}

// Round-8 (N4 retirement): the v2 registration response carries the
// controller-served arming answer; v1 responses stay bare.
func TestV2RegistrationResponseCarriesServedArming(t *testing.T) {
	armed := true
	body, _ := json.Marshal(types.AgentRegistration{Name: "node-a", DestructiveArmed: &armed})
	backend := &registrationBackend{arming: types.AgentArmingArmed}
	routes := New(backend).AgentRoutes(fixedAgentAuthenticator{principal: AgentPrincipal{NodeName: "node-a"}})

	v2 := httptest.NewRecorder()
	routes.ServeHTTP(v2, httptest.NewRequest(http.MethodPost, types.AgentRegistrationV2Path, strings.NewReader(string(body))))
	if v2.Code != http.StatusNoContent || v2.Header().Get(types.AgentArmingHeader) != "armed" {
		t.Fatalf("v2 response = %d, arming header %q; want 204 with 'armed'", v2.Code, v2.Header().Get(types.AgentArmingHeader))
	}

	v1Body, _ := json.Marshal(types.AgentRegistration{Name: "node-a"})
	v1 := httptest.NewRecorder()
	routes.ServeHTTP(v1, httptest.NewRequest(http.MethodPost, types.AgentRegistrationPath, strings.NewReader(string(v1Body))))
	if v1.Code != http.StatusNoContent || v1.Header().Get(types.AgentArmingHeader) != "" {
		t.Fatalf("v1 response = %d, arming header %q; want 204 with no header (frozen v1 contract)", v1.Code, v1.Header().Get(types.AgentArmingHeader))
	}
}
