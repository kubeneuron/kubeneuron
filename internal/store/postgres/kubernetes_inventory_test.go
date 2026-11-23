// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// A node without an agent is recorded from Kubernetes; once its agent
// registers, the agent owns the row and Kubernetes no longer writes it.
func TestKubernetesInventoryYieldsToTheAgent(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tpu := []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Allocatable: 3, Unit: types.DeviceUnitTPUChip}}
	if err := st.UpsertKubernetesInventory(ctx, &types.Node{Name: "k1", UID: "uid-1", Devices: tpu}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetNode(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if got.InventorySource != types.InventoryKubernetes || got.Platform != "kubernetes" || got.UID != "uid-1" ||
		!reflect.DeepEqual(got.Devices, tpu) || !got.AgentLastSeen.IsZero() || len(got.GPUs) != 0 {
		t.Fatalf("kubernetes row = %+v", got)
	}

	if err := st.UpsertAgentRegistration(ctx, &types.Node{Name: "k1", AgentLastSeen: time.Now(),
		GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-0"}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertKubernetesInventory(ctx, &types.Node{Name: "k1", UID: "uid-2", Devices: tpu}); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetNode(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if got.InventorySource != types.InventoryAgent || len(got.Devices) != 0 || got.UID != "uid-1" || len(got.GPUs) != 1 {
		t.Fatalf("after the agent registered, row = %+v", got)
	}
}

// A pause config's row keeps its pause when the node is read from Kubernetes.
func TestKubernetesInventoryKeepsAPause(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.ApplyNodeConfigPauses(ctx, []string{"p1"}); err != nil {
		t.Fatal(err)
	}
	gpu := []types.DeviceCount{{Resource: "amd.com/gpu", Capacity: 8, Allocatable: 8, Unit: types.DeviceUnitGPU}}
	if err := st.UpsertKubernetesInventory(ctx, &types.Node{Name: "p1", Devices: gpu}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetNode(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Paused || got.InventorySource != types.InventoryKubernetes || !reflect.DeepEqual(got.Devices, gpu) {
		t.Fatalf("row = %+v", got)
	}
}

// Only a row read from Kubernetes, unpaused and without an agent, is
// removed when its Node is gone.
func TestDeleteKubernetesInventoryKeepsOtherRows(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	gpu := []types.DeviceCount{{Resource: "nvidia.com/gpu", Capacity: 8, Allocatable: 8, Unit: types.DeviceUnitGPU}}
	for _, name := range []string{"gone", "agent", "paused"} {
		if err := st.UpsertKubernetesInventory(ctx, &types.Node{Name: name, Labels: map[string]string{"pool": "a"}, Devices: gpu}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := st.GetNode(ctx, "gone"); err != nil || got.Labels["pool"] != "a" {
		t.Fatalf("labels not recorded: %+v, %v", got, err)
	}
	if err := st.UpsertAgentRegistration(ctx, &types.Node{Name: "agent", AgentLastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyNodeConfigPauses(ctx, []string{"paused"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gone", "agent", "paused"} {
		if err := st.DeleteKubernetesInventory(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.GetNode(ctx, "gone"); err == nil {
		t.Fatal("the vanished node's row is still there")
	}
	for _, name := range []string{"agent", "paused"} {
		if _, err := st.GetNode(ctx, name); err != nil {
			t.Fatalf("%s was removed: %v", name, err)
		}
	}
}

// A registered agent's row takes the labels its Node object carries now,
// while its UID is the agent's or unknown; another UID is another machine.
// A row no agent registered is not written.
func TestUpdateNodeLabelsWritesOnlyTheAgentsNode(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for name, uid := range map[string]string{"same": "uid-1", "unknown": "", "other": "uid-1"} {
		if err := st.UpsertAgentRegistration(ctx, &types.Node{Name: name, UID: uid, AgentLastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ApplyNodeConfigPauses(ctx, []string{"paused"}); err != nil {
		t.Fatal(err)
	}
	now := map[string]string{"pool": "b"}
	for name, uid := range map[string]string{"same": "uid-1", "unknown": "uid-1", "other": "uid-2", "paused": "uid-1", "absent": "uid-1"} {
		if err := st.UpdateNodeLabels(ctx, name, uid, now); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]map[string]string{"same": now, "unknown": now, "other": {}, "paused": {}} {
		got, err := st.GetNode(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Labels) != len(want) || got.Labels["pool"] != want["pool"] {
			t.Errorf("%s labels = %v, want %v", name, got.Labels, want)
		}
	}
	if got, _ := st.GetNode(ctx, "unknown"); got.UID != "" {
		t.Errorf("the unknown UID became %q", got.UID)
	}
	if _, err := st.GetNode(ctx, "absent"); err == nil {
		t.Error("a row was created for a node no agent registered")
	}
}

func TestNodeDeletionMarkerDefaultsToNull(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertAgentRegistration(ctx, &types.Node{Name: "registered", UID: "uid", AgentLastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var live bool
	if err := st.sqlDB.QueryRowContext(ctx, "SELECT deleted_at IS NULL FROM nodes WHERE name = 'registered'").Scan(&live); err != nil {
		t.Fatal(err)
	}
	if !live {
		t.Fatal("a newly registered node has a deletion marker")
	}
	stamp := time.Now().UTC()
	if err := st.MarkAgentNodeDeleted(ctx, "registered", "uid", stamp); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAgentNodeDeleted(ctx, "registered", "uid", stamp.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	nodes, err := st.ListNodes(ctx)
	if err != nil || len(nodes) != 0 {
		t.Fatalf("live nodes = %v, err = %v", nodes, err)
	}
	retained, err := st.ListRetainedNodes(ctx)
	if err != nil || len(retained) != 1 || retained[0].UID != "uid" {
		t.Fatalf("retained nodes = %v, err = %v", retained, err)
	}
	node, err := st.GetNode(ctx, "registered")
	if err != nil || node.DeletedAt == nil || !node.DeletedAt.Equal(stamp) {
		t.Fatalf("retained record = %+v, err = %v", node, err)
	}
	if err := st.UpsertAgentRegistration(ctx, &types.Node{Name: "registered", UID: "replacement", AgentLastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAgentNodeDeleted(ctx, "registered", "uid", stamp); err != nil {
		t.Fatal(err)
	}
	nodes, err = st.ListNodes(ctx)
	if err != nil || len(nodes) != 1 || nodes[0].UID != "replacement" || nodes[0].DeletedAt != nil {
		t.Fatalf("replacement after delayed sweep = %v, err = %v", nodes, err)
	}

}
