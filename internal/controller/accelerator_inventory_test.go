// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// inventoryPlatform lists scripted accelerator nodes.
type inventoryPlatform struct {
	basePlatform
	imu   sync.Mutex
	nodes []types.Node
	err   error
}

func (p *inventoryPlatform) AcceleratorNodes(context.Context) ([]types.Node, error) {
	p.imu.Lock()
	defer p.imu.Unlock()
	return append([]types.Node(nil), p.nodes...), p.err
}

// inventoryWrites counts the Kubernetes inventory writes.
type inventoryWrites struct {
	store.Store
	mu     sync.Mutex
	writes int
}

func (s *inventoryWrites) UpsertKubernetesInventory(ctx context.Context, n *types.Node) error {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return s.Store.UpsertKubernetesInventory(ctx, n)
}

func inventoryFixture(t *testing.T) (*Controller, *inventoryWrites, *inventoryPlatform) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plat := &inventoryPlatform{}
	c := New(st, st, engine, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 8, MaxConcurrentReboots: 8}), safety.NewFlapDetector(100, time.Hour), plat, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ws := &inventoryWrites{Store: st}
	c.store = ws
	return c, ws, plat
}

var tpuDevices = []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Allocatable: 4, Unit: types.DeviceUnitTPUChip}}

func TestInventoryRecordsANodeWithoutAnAgent(t *testing.T) {
	c, st, plat := inventoryFixture(t)
	ctx := context.Background()
	plat.nodes = []types.Node{{Name: "tpu-1", UID: "uid-1", Devices: tpuDevices}}
	c.syncAcceleratorInventory(ctx)

	got, err := st.GetNode(ctx, "tpu-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.InventorySource != types.InventoryKubernetes || got.UID != "uid-1" || !reflect.DeepEqual(got.Devices, tpuDevices) {
		t.Fatalf("row = %+v", got)
	}
	if !got.AgentLastSeen.IsZero() || len(got.GPUs) != 0 {
		t.Fatalf("a node read from Kubernetes claims an agent: %+v", got)
	}

	// An unchanged node is not written again; a changed one is.
	c.syncAcceleratorInventory(ctx)
	if st.writes != 1 {
		t.Fatalf("%d writes for an unchanged node, want 1", st.writes)
	}
	degraded := []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Allocatable: 3, Unit: types.DeviceUnitTPUChip}}
	plat.nodes = []types.Node{{Name: "tpu-1", UID: "uid-1", Devices: degraded}}
	c.syncAcceleratorInventory(ctx)
	if got, _ := st.GetNode(ctx, "tpu-1"); st.writes != 2 || !reflect.DeepEqual(got.Devices, degraded) {
		t.Fatalf("writes %d, devices %+v; want the change recorded", st.writes, got.Devices)
	}
}

// The agent is the source of truth for its node: the Kubernetes view never
// overwrites it.
func TestInventoryLeavesTheAgentsNodeAlone(t *testing.T) {
	c, st, plat := inventoryFixture(t)
	ctx := context.Background()
	gpus := []types.GPUInfo{{Index: 0, UUID: "GPU-0"}}
	if err := st.UpsertAgentRegistration(ctx, &types.Node{Name: "gpu-1", UID: "uid-agent", GPUs: gpus, AgentLastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	plat.nodes = []types.Node{{Name: "gpu-1", UID: "uid-k8s", Devices: []types.DeviceCount{
		{Resource: "nvidia.com/gpu", Capacity: 8, Allocatable: 8, Unit: types.DeviceUnitGPU},
	}}}
	c.syncAcceleratorInventory(ctx)
	c.syncAcceleratorInventory(ctx)

	got, err := st.GetNode(ctx, "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.writes != 0 || got.InventorySource != types.InventoryAgent || got.UID != "uid-agent" ||
		len(got.Devices) != 0 || !reflect.DeepEqual(got.GPUs, gpus) {
		t.Fatalf("writes %d, row %+v; want the agent's row untouched", st.writes, got)
	}
}

func TestInventoryReadFailureChangesNothing(t *testing.T) {
	c, st, plat := inventoryFixture(t)
	ctx := context.Background()
	plat.err = errors.New("apiserver unavailable")
	c.syncAcceleratorInventory(ctx)
	if nodes, err := st.ListNodes(ctx); err != nil || len(nodes) != 0 || st.writes != 0 {
		t.Fatalf("nodes %v, writes %d, err %v", nodes, st.writes, err)
	}
}
