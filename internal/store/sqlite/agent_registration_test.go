package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestUpsertAgentRegistrationInsertsAgentNode(t *testing.T) {
	st := openTestStore(t)
	lastSeen := time.Date(2026, time.July, 13, 10, 11, 12, 345, time.UTC)
	registration := &types.Node{
		Name: "agent-new",
		UID:  "server-derived-node-uid",
		GPUs: []types.GPUInfo{
			{Index: 0, UUID: "GPU-new", Model: "test-model"},
		},
		BootID:        "boot-new",
		AgentLastSeen: lastSeen,
		// These fields are deliberately populated to prove that registration
		// cannot smuggle controller-owned values into a new row.
		Platform: "smuggled",
		Labels:   map[string]string{"smuggled": "true"},
		SSHAddr:  "smuggled:22",
		BMCAddr:  "smuggled-bmc",
		Paused:   true,
	}

	if err := st.UpsertAgentRegistration(context.Background(), registration); err != nil {
		t.Fatalf("UpsertAgentRegistration() error = %v", err)
	}

	got, err := st.GetNode(context.Background(), registration.Name)
	if err != nil {
		t.Fatalf("GetNode() error = %v", err)
	}
	if got.Platform != "agent" {
		t.Errorf("Platform = %q, want agent", got.Platform)
	}
	if got.UID != registration.UID {
		t.Errorf("UID = %q, want server-stamped %q", got.UID, registration.UID)
	}
	if len(got.Labels) != 0 || got.SSHAddr != "" || got.BMCAddr != "" || got.Paused {
		t.Errorf("controller-owned fields were inserted from registration: %+v", got)
	}
	if !reflect.DeepEqual(got.GPUs, registration.GPUs) {
		t.Errorf("GPUs = %#v, want %#v", got.GPUs, registration.GPUs)
	}
	if got.BootID != registration.BootID {
		t.Errorf("BootID = %q, want %q", got.BootID, registration.BootID)
	}
	if !got.AgentLastSeen.Equal(lastSeen) {
		t.Errorf("AgentLastSeen = %s, want %s", got.AgentLastSeen, lastSeen)
	}
}

func TestUpsertAgentRegistrationPreservesControllerOwnedFields(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	originalLastSeen := time.Date(2026, time.July, 12, 1, 2, 3, 0, time.UTC)
	original := &types.Node{
		Name:          "managed-node",
		UID:           "controller-node-uid",
		Platform:      "baremetal",
		Labels:        map[string]string{"rack": "r42", "owner": "ops"},
		SSHAddr:       "10.0.0.42:22",
		BMCAddr:       "10.1.0.42",
		GPUs:          []types.GPUInfo{{Index: 0, UUID: "GPU-old"}},
		BootID:        "boot-old",
		Paused:        true,
		AgentLastSeen: originalLastSeen,
	}
	if err := st.UpsertNode(ctx, original); err != nil {
		t.Fatalf("UpsertNode() error = %v", err)
	}

	refreshedLastSeen := time.Date(2026, time.July, 13, 4, 5, 6, 789, time.UTC)
	registration := &types.Node{
		Name:          original.Name,
		Platform:      "agent",
		Labels:        map[string]string{"overwrite": "attempt"},
		SSHAddr:       "overwrite:22",
		BMCAddr:       "overwrite-bmc",
		GPUs:          []types.GPUInfo{{Index: 1, UUID: "GPU-new", Model: "new-model"}},
		BootID:        "boot-new",
		Paused:        false,
		AgentLastSeen: refreshedLastSeen,
	}
	if err := st.UpsertAgentRegistration(ctx, registration); err != nil {
		t.Fatalf("UpsertAgentRegistration() error = %v", err)
	}

	got, err := st.GetNode(ctx, original.Name)
	if err != nil {
		t.Fatalf("GetNode() error = %v", err)
	}
	if got.Platform != original.Platform || !reflect.DeepEqual(got.Labels, original.Labels) ||
		got.SSHAddr != original.SSHAddr || got.BMCAddr != original.BMCAddr || got.Paused != original.Paused {
		t.Errorf("controller-owned fields changed: got %+v, original %+v", got, original)
	}
	if got.UID != original.UID {
		t.Errorf("UID = %q, want preserved %q when registration has no server identity", got.UID, original.UID)
	}
	if !reflect.DeepEqual(got.GPUs, registration.GPUs) {
		t.Errorf("GPUs = %#v, want %#v", got.GPUs, registration.GPUs)
	}
	if got.BootID != registration.BootID {
		t.Errorf("BootID = %q, want %q", got.BootID, registration.BootID)
	}
	if !got.AgentLastSeen.Equal(refreshedLastSeen) {
		t.Errorf("AgentLastSeen = %s, want %s", got.AgentLastSeen, refreshedLastSeen)
	}
}

func TestUpsertNodePreservesKnownUIDWhenInventoryRefreshHasNone(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{
		Name: "node-a", UID: "immutable-node-uid", Platform: "kubernetes",
		Labels: map[string]string{"accelerator": "nvidia"},
	}); err != nil {
		t.Fatalf("initial UpsertNode() error = %v", err)
	}
	if err := st.UpsertNode(ctx, &types.Node{
		Name: "node-a", Platform: "config", Labels: map[string]string{"rack": "r42"},
	}); err != nil {
		t.Fatalf("UID-less UpsertNode() error = %v", err)
	}
	got, err := st.GetNode(ctx, "node-a")
	if err != nil {
		t.Fatalf("GetNode() error = %v", err)
	}
	if got.UID != "immutable-node-uid" {
		t.Fatalf("UID = %q, want preserved immutable identity", got.UID)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return st
}
