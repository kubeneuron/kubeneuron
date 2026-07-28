package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// actionBackend serves one queued action and records completions.
type actionBackend struct {
	registrationBackend
	queued    map[string]*types.QueuedAction // by node
	completed []string
	rejected  bool
}

func (b *actionBackend) NextAction(_ *http.Request, node string) (*types.QueuedAction, error) {
	return b.queued[node], nil
}

func (b *actionBackend) CompleteAction(_ *http.Request, node, actionID, leaseToken string, res types.ActionResult) error {
	qa := b.queued[node]
	if qa == nil || qa.Action.ID != actionID || qa.LeaseToken != leaseToken {
		b.rejected = true
		return http.ErrNotSupported
	}
	b.completed = append(b.completed, actionID)
	return nil
}

func agentRoutesFor(backend Backend, node string) http.Handler {
	return New(backend).AgentRoutes(fixedAgentAuthenticator{principal: AgentPrincipal{NodeName: node}})
}

func TestNextActionServesOnlyAuthenticatedNode(t *testing.T) {
	backend := &actionBackend{queued: map[string]*types.QueuedAction{
		"node-a": {Node: "node-a", Action: types.Action{ID: "act-1", Type: types.ActionGPUReset}, LeaseToken: "lease-a", LeaseExpiresAt: time.Now().Add(time.Minute)},
	}}

	// node-a sees its action.
	rec := httptest.NewRecorder()
	agentRoutesFor(backend, "node-a").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, types.AgentActionLeasePath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var action types.Action
	if err := json.Unmarshal(rec.Body.Bytes(), &action); err != nil || action.ID != "act-1" {
		t.Fatalf("body = %s, err %v", rec.Body.String(), err)
	}
	if got := rec.Header().Get(types.AgentActionLeaseHeader); got != "lease-a" {
		t.Fatalf("lease header = %q, want lease-a", got)
	}
	if got := rec.Header().Get(types.AgentActionLeaseExpiresHeader); got == "" {
		t.Fatal("lease expiry header is empty")
	} else if expiry, err := time.Parse(time.RFC3339Nano, got); err != nil || !expiry.After(time.Now()) {
		t.Fatalf("lease expiry header = %q, parsed %v, err %v; want future RFC3339Nano time", got, expiry, err)
	}

	// node-b (a different authenticated identity) gets an empty queue, not
	// node-a's work.
	rec = httptest.NewRecorder()
	agentRoutesFor(backend, "node-b").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, types.AgentActionLeasePath, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("foreign node status = %d, want 204", rec.Code)
	}
}

func TestUnleasedActionRoutesFailClosed(t *testing.T) {
	backend := &actionBackend{queued: map[string]*types.QueuedAction{}}
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/agents/actions", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/agents/actions/act-1/result", strings.NewReader(`{}`)),
	} {
		rec := httptest.NewRecorder()
		agentRoutesFor(backend, "node-a").ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s status = %d, want 404", req.Method, req.URL.Path, rec.Code)
		}
	}
}

func TestActionResultRoundTrip(t *testing.T) {
	backend := &actionBackend{queued: map[string]*types.QueuedAction{
		"node-a": {Node: "node-a", Action: types.Action{ID: "act-1", Type: types.ActionGPUReset}, LeaseToken: "lease-a"},
	}}
	body := `{"action_id":"act-1","ok":true,"output":"done","started_at":"2026-07-14T00:00:00Z","finished_at":"2026-07-14T00:00:01Z"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, types.AgentActionLeasePath+"/act-1/result", strings.NewReader(body))
	req.Header.Set(types.AgentActionLeaseHeader, "lease-a")
	agentRoutesFor(backend, "node-a").ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(backend.completed) != 1 || backend.completed[0] != "act-1" {
		t.Fatalf("completed = %v", backend.completed)
	}
}

func TestActionResultRejectsIDMismatchAndForeignNode(t *testing.T) {
	backend := &actionBackend{queued: map[string]*types.QueuedAction{
		"node-a": {Node: "node-a", Action: types.Action{ID: "act-1", Type: types.ActionGPUReset}, LeaseToken: "lease-a"},
	}}

	// Body/path ID mismatch is a 400.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, types.AgentActionLeasePath+"/act-1/result",
		strings.NewReader(`{"action_id":"other","ok":true,"started_at":"2026-07-14T00:00:00Z","finished_at":"2026-07-14T00:00:00Z"}`))
	agentRoutesFor(backend, "node-a").ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatch status = %d, want 400", rec.Code)
	}

	// A different node posting a result for node-a's action is rejected.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, types.AgentActionLeasePath+"/act-1/result",
		strings.NewReader(`{"action_id":"act-1","ok":true,"started_at":"2026-07-14T00:00:00Z","finished_at":"2026-07-14T00:00:00Z"}`))
	req.Header.Set(types.AgentActionLeaseHeader, "lease-a")
	agentRoutesFor(backend, "node-b").ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign node status = %d, want 403", rec.Code)
	}
	if len(backend.completed) != 0 {
		t.Fatalf("completed = %v, want none", backend.completed)
	}
}

func TestActionResultRequiresLeaseHeader(t *testing.T) {
	backend := &actionBackend{queued: map[string]*types.QueuedAction{
		"node-a": {Node: "node-a", Action: types.Action{ID: "act-1", Type: types.ActionGPUReset}, LeaseToken: "lease-a"},
	}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, types.AgentActionLeasePath+"/act-1/result",
		strings.NewReader(`{"action_id":"act-1","ok":true,"started_at":"2026-07-14T00:00:00Z","finished_at":"2026-07-14T00:00:00Z"}`))
	agentRoutesFor(backend, "node-a").ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if len(backend.completed) != 0 {
		t.Fatalf("completed = %v, want none", backend.completed)
	}
}
