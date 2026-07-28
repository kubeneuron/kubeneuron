package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type registrationBackend struct {
	err           error
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

func (b *registrationBackend) HandleAgentEvent(_ *http.Request, event types.AgentEvent) error {
	b.events = append(b.events, event)
	return b.err
}

func (b *registrationBackend) RegisterNode(_ *http.Request, registration types.AgentRegistration) error {
	b.registrations = append(b.registrations, registration)
	return b.err
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
	New(backend).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %q", response.Code, response.Body.String())
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
