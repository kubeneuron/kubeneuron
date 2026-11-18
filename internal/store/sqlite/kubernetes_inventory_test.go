// SPDX-License-Identifier: Apache-2.0

package sqlite

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
