// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
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

func (p *inventoryPlatform) NodeInventory(context.Context) ([]types.Node, error) {
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

// A node read from Kubernetes that is gone leaves the inventory; an agent's
// row stays.
func TestInventoryRemovesAVanishedNode(t *testing.T) {
	c, st, plat := inventoryFixture(t)
	ctx := context.Background()
	if err := st.UpsertAgentRegistration(ctx, &types.Node{Name: "agent-1", AgentLastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	plat.nodes = []types.Node{{Name: "tpu-1", Devices: tpuDevices}, {Name: "tpu-2", Devices: tpuDevices}}
	c.syncAcceleratorInventory(ctx)
	plat.nodes = []types.Node{{Name: "tpu-2", Devices: tpuDevices}}
	c.syncAcceleratorInventory(ctx)

	if _, err := st.GetNode(ctx, "tpu-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a vanished node's row is still there: %v", err)
	}
	for _, name := range []string{"tpu-2", "agent-1"} {
		if _, err := st.GetNode(ctx, name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A device plugin that stops takes its resource off the Node; the node stays
// in the inventory with its last known capacity and nothing allocatable.
func TestInventoryKeepsTheCapacityADevicePluginDropped(t *testing.T) {
	c, st, plat := inventoryFixture(t)
	ctx := context.Background()
	plat.nodes = []types.Node{{Name: "tpu-1", Devices: tpuDevices}}
	c.syncAcceleratorInventory(ctx)
	plat.nodes = []types.Node{{Name: "tpu-1"}}
	c.syncAcceleratorInventory(ctx)
	c.syncAcceleratorInventory(ctx)

	got, err := st.GetNode(ctx, "tpu-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Allocatable: 0, Unit: types.DeviceUnitTPUChip}}
	if !reflect.DeepEqual(got.Devices, want) {
		t.Fatalf("devices %+v, want %+v", got.Devices, want)
	}

	// A node that never advertised a device is not recorded.
	plat.nodes = append(plat.nodes, types.Node{Name: "cpu-1"})
	c.syncAcceleratorInventory(ctx)
	if _, err := st.GetNode(ctx, "cpu-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a CPU node was recorded: %v", err)
	}
}

// A node-scoped incident on a node without an agent is charged one GPU, and
// the report says so.
func TestInventoryReportMarksTheAssumedGPU(t *testing.T) {
	c, st, plat := inventoryFixture(t)
	ctx := context.Background()
	plat.nodes = []types.Node{{Name: "gpu-1", Devices: []types.DeviceCount{
		{Resource: "amd.com/gpu", Capacity: 8, Allocatable: 8, Unit: types.DeviceUnitGPU},
	}}}
	c.syncAcceleratorInventory(ctx)
	now := time.Now()
	resolved := now.Add(-time.Hour)
	if err := st.CreateIncident(ctx, &types.Incident{ID: "i1", Class: types.ClassFellOffBus, Target: types.Target{Node: "gpu-1"},
		State: types.StateResolved, OpenedAt: now.Add(-3 * time.Hour), UpdatedAt: resolved, StateChangedAt: resolved, ResolvedAt: &resolved}); err != nil {
		t.Fatal(err)
	}
	report, err := c.RecoveryReport(ctx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if report.Incidents != 1 || report.AssumedSingleGPU != 1 || report.DegradedGPUHours == 0 {
		t.Fatalf("report %+v; want one incident charged one assumed GPU", report)
	}
}

// inventoryCloudPlatform is the automatic fixture's cloud platform with the
// node inventory read from Kubernetes.
type inventoryCloudPlatform struct {
	autoCloudPlatform
	nodes []types.Node
}

func (p *inventoryCloudPlatform) NodeInventory(context.Context) ([]types.Node, error) {
	return p.nodes, nil
}

// The breaker's pool comes from the labels recorded from the Node: a fault in
// a healthy node pool is not held by another pool's faults, and faults filling
// one pool trip the breaker. The trip is per policy revision, so pool a goes
// first.
func TestInventoryLabelsPlaceNodesInTheirPool(t *testing.T) {
	settings := autoSettings()
	settings.PoolLabel = "cloud.google.com/gke-nodepool"
	c, st, notifier := autoFixture(t, recoverymode.Automatic, settings, 0)
	plat := &inventoryCloudPlatform{}
	for _, n := range []struct{ name, pool string }{{"a1", "a"}, {"a2", "a"}, {"b1", "b"}, {"b2", "b"}} {
		plat.nodes = append(plat.nodes, types.Node{Name: n.name, UID: "uid-" + n.name,
			Labels:  map[string]string{"pool": "train", "cloud.google.com/gke-nodepool": n.pool},
			Devices: []types.DeviceCount{{Resource: "nvidia.com/gpu", Capacity: 8, Allocatable: 8, Unit: types.DeviceUnitGPU}}})
	}
	c.platform = plat
	c.syncAcceleratorInventory(context.Background())
	now := time.Now()
	if err := st.CreateIncident(context.Background(), &types.Incident{ID: "fault-b2", Class: types.ClassFellOffBus, Target: types.Target{Node: "b2"},
		State: types.StateObserving, OpenedAt: now, UpdatedAt: now, StateChangedAt: now}); err != nil {
		t.Fatal(err)
	}

	if inc := step(t, c, st, openOn(t, c, "a1"), 200); inc.State == types.StateAwaitingApproval &&
		strings.Contains(parkReason(t, st, inc.ID), "circuit breaker") {
		t.Fatalf("pool a was held by pool b's breaker: %q (approvals %v)", parkReason(t, st, inc.ID), notifier.approvals)
	}
	if inc := step(t, c, st, openOn(t, c, "b1"), 200); inc.State != types.StateAwaitingApproval ||
		!strings.Contains(parkReason(t, st, inc.ID), "circuit breaker") {
		t.Fatalf("pool b: state=%s reason=%q, want the breaker to trip", inc.State, parkReason(t, st, inc.ID))
	}
}
