package controller

import (
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

func TestRegisterNodeServerStampsLastSeen(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	c := New(st, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest("POST", types.AgentRegistrationPath, nil)
	registration := types.AgentRegistration{
		Name:   "node-a",
		GPUs:   []types.GPUInfo{{Index: 0, UUID: "GPU-a"}},
		BootID: "boot-a",
	}

	before := time.Now()
	if _, err := c.RegisterNode(req, registration); err != nil {
		t.Fatalf("RegisterNode() error = %v", err)
	}
	after := time.Now()

	got, err := st.GetNode(req.Context(), registration.Name)
	if err != nil {
		t.Fatalf("GetNode() error = %v", err)
	}
	if got.AgentLastSeen.Before(before) || got.AgentLastSeen.After(after) {
		t.Errorf("AgentLastSeen = %s, want between %s and %s", got.AgentLastSeen, before, after)
	}
}

func TestActionResultRequiresCurrentLease(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := New(st, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest("GET", types.AgentActionLeasePath, nil)
	if err := st.EnqueueAction(req.Context(), "node-a", types.Action{IncidentID: "incident-a",
		ID: "leased-action", Type: types.ActionGPUReset,
	}); err != nil {
		t.Fatal(err)
	}

	claimed, err := c.NextAction(req, "node-a")
	if err != nil || claimed == nil || claimed.LeaseToken == "" {
		t.Fatalf("NextAction() = %+v, %v; want a claimed action", claimed, err)
	}
	result := types.ActionResult{ActionID: claimed.Action.ID, OK: true}
	if err := c.CompleteAction(req, "node-a", claimed.Action.ID, "wrong-token", result); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("wrong token completion = %v, want ErrLeaseLost", err)
	}
	if err := c.CompleteAction(req, "node-a", claimed.Action.ID, claimed.LeaseToken, result); err != nil {
		t.Fatalf("current token completion = %v", err)
	}
	stored, err := st.GetAction(req.Context(), claimed.Action.ID)
	if err != nil || !stored.Done || stored.Result == nil || !stored.Result.OK {
		t.Fatalf("stored action = %+v, %v; want completed result", stored, err)
	}
}
