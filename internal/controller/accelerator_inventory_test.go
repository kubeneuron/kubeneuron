// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kubeneuron/kubeneuron/internal/httpapi"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
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

// inventoryWrites counts the Kubernetes inventory writes and the node
// listings.
type inventoryWrites struct {
	store.Store
	mu     sync.Mutex
	writes int
	lists  int
}

func (s *inventoryWrites) ListNodes(ctx context.Context) ([]*types.Node, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.Store.ListNodes(ctx)
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

// The store is listed for vanished nodes only when the set of Node names
// changed, and once again after a new term as leader.
func TestInventorySweepsOnlyWhenNodesComeOrGo(t *testing.T) {
	c, st, plat := inventoryFixture(t)
	ctx := context.Background()
	plat.nodes = []types.Node{{Name: "tpu-1", Devices: tpuDevices}, {Name: "tpu-2", Devices: tpuDevices}}
	c.syncAcceleratorInventory(ctx)
	c.syncAcceleratorInventory(ctx)
	if st.lists != 1 {
		t.Fatalf("listed %d times for an unchanged set of nodes, want 1", st.lists)
	}
	plat.nodes = []types.Node{{Name: "tpu-2", Devices: tpuDevices}}
	c.syncAcceleratorInventory(ctx)
	if st.lists != 2 {
		t.Fatalf("listed %d times after a node went, want 2", st.lists)
	}
	c.inventory.reset()
	c.syncAcceleratorInventory(ctx)
	if st.lists != 3 {
		t.Fatalf("listed %d times after a new term, want 3", st.lists)
	}
}

// A new UID under the same name is another machine: the devices the old one
// lost are not carried over.
func TestInventoryNewMachineDoesNotInheritDevices(t *testing.T) {
	c, st, plat := inventoryFixture(t)
	ctx := context.Background()
	plat.nodes = []types.Node{{Name: "tpu-1", UID: "old", Devices: tpuDevices}}
	c.syncAcceleratorInventory(ctx)
	gpus := []types.DeviceCount{{Resource: "nvidia.com/gpu", Capacity: 8, Allocatable: 8, Unit: types.DeviceUnitGPU}}
	plat.nodes = []types.Node{{Name: "tpu-1", UID: "new", Devices: gpus}}
	c.syncAcceleratorInventory(ctx)

	got, err := st.GetNode(ctx, "tpu-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Devices, gpus) {
		t.Fatalf("devices %+v, want only the new machine's %+v", got.Devices, gpus)
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

func TestAcceleratorPoolInventoryBeforeIncident(t *testing.T) {
	c, _, p := inventoryFixture(t)
	c.SetEngine(poolTestEngine(t))
	for i, label := range []string{"eks.amazonaws.com/nodegroup", "cloud.google.com/gke-nodepool", "kubernetes.azure.com/agentpool", "cloud.google.com/gke-nodepool"} {
		p.nodes = append(p.nodes, types.Node{Name: []string{"e", "g1", "a", "g2"}[i], Labels: map[string]string{label: "train"}, Devices: []types.DeviceCount{{Resource: "nvidia.com/gpu", Capacity: 4}}})
	}
	p.nodes = append(p.nodes, types.Node{Name: "cpu", Labels: map[string]string{"cloud.google.com/gke-nodepool": "train"}})
	c.syncAcceleratorInventory(t.Context())
	rows := c.PoolAvailability().Pools
	if len(rows) != 3 || rows[0].Cloud != "AKS" || rows[1].Cloud != "EKS" || rows[2].Cloud != "GKE" || rows[2].AcceleratorNodes != 2 {
		t.Fatalf("unexpected pools: %+v", rows)
	}
	for _, row := range rows {
		if row.Cloud == "EKS" {
			if row.Status != "unavailable" || row.Reason != "add-first on EKS is not enabled in this release" || row.CheckedAt == nil || row.Action != "" {
				t.Fatalf("unexpected EKS release assessment: %+v", row)
			}
			continue
		}
		if row.Status != "unknown" || row.Reason != "no cloud provider configured" || row.CheckedAt != nil {
			t.Fatalf("unexpected assessment: %+v", row)
		}
	}
	p.nodes = nil
	c.syncAcceleratorInventory(t.Context())
	if got := c.PoolAvailability().Pools; len(got) != 0 {
		t.Fatalf("removed pools still present: %+v", got)
	}
}

func TestPoolReplacementOrders(t *testing.T) {
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"repair": {Steps: []playbook.Step{{Action: "platform.replace_node"}}}}, []playbook.Policy{
		{Class: types.ClassFellOffBus, Playbook: "repair"},
		{Class: types.ClassFellOffBus, Vendor: "amd", Playbook: "repair", ReplacementStrategy: recoverymode.RemoveFirst},
	})
	if err != nil {
		t.Fatal(err)
	}
	var c Controller
	for _, tc := range []struct {
		resources []string
		want      string
	}{
		{[]string{"amd.com/gpu"}, "RemoveFirst"}, {[]string{"nvidia.com/gpu"}, "AddFirst"}, {[]string{"amd.com/gpu", "nvidia.com/gpu"}, "mixed"},
	} {
		var nodes []types.Node
		for _, resource := range tc.resources {
			nodes = append(nodes, types.Node{Name: resource, Labels: map[string]string{"eks.amazonaws.com/nodegroup": "train"}, Devices: []types.DeviceCount{{Resource: resource}}})
		}
		c.inventory.recordPools(nodes, engine, time.Now(), nil)
		row := c.PoolAvailability().Pools[0]
		if tc.want == "RemoveFirst" && (row.Status != "" || row.Reason != "" || row.CheckedAt != nil) {
			t.Fatalf("unexpected check for remove-first: %+v", row)
		}
		if got := row.ReplacementPolicy; got != tc.want {
			t.Fatalf("%v: got %q, want %q", tc.resources, got, tc.want)
		}
	}
}

func TestPoolCheckInterval(t *testing.T) {
	var c Controller
	node := types.Node{Name: "gpu1", Labels: map[string]string{"cloud.google.com/gke-nodepool": "train"}, Devices: tpuDevices}
	second := node
	second.Name = "gpu2"
	nodes := []types.Node{node, second}
	now := time.Now().UTC()
	calls := 0
	check := func(types.Node) error {
		calls++
		if calls == 1 {
			return errors.New("read failure")
		}
		return nil
	}
	for _, tc := range []struct {
		after  time.Duration
		calls  int
		status string
	}{{0, 1, "unknown"}, {9 * time.Minute, 1, "unknown"}, {10 * time.Minute, 2, "available"}} {
		c.inventory.recordPools(nodes, poolTestEngine(t), now.Add(tc.after), check)
		row := c.PoolAvailability().Pools[0]
		if row.Status != tc.status || calls != tc.calls || row.CheckedAt == nil || row.AcceleratorNodes != 2 {
			t.Fatalf("after %s: calls %d row %+v", tc.after, calls, row)
		}
	}
}

type poolAssessmentPlatform struct {
	*inventoryPlatform
	checks []string
}

func (p *poolAssessmentPlatform) CloudNode(_ context.Context, name string) (cloud.NodeRef, error) {
	return cloud.NodeRef{Identity: cloud.NodeIdentity{Node: name, NodeUID: name + "-uid"}}, nil
}
func (p *poolAssessmentPlatform) AddFirstAvailable(_ context.Context, pin cloud.NodeIdentity) error {
	p.checks = append(p.checks, pin.Node)
	if pin.Node == "gke" {
		return &cloud.PoolRefusal{Code: cloud.PoolAutopilot, Reason: "GKE Autopilot manages its nodes"}
	}
	return nil
}
func TestPoolChecksBeforeIncident(t *testing.T) {
	c, _, inv := inventoryFixture(t)
	c.SetEngine(poolTestEngine(t))
	p := &poolAssessmentPlatform{inventoryPlatform: inv}
	c.platform = p
	for name, label := range map[string]string{"aks": "kubernetes.azure.com/agentpool", "gke": "cloud.google.com/gke-nodepool", "eks": "eks.amazonaws.com/nodegroup"} {
		p.nodes = append(p.nodes, types.Node{Name: name, UID: name + "-uid", Labels: map[string]string{label: "train"}, Devices: tpuDevices})
	}
	c.syncAcceleratorInventory(t.Context())
	c.syncAcceleratorInventory(t.Context())
	api := httpapi.New(c)
	api.EnableOperatorAPI(c, "test-token")
	api.SetReadyCheck(func() bool { return true })
	server := httptest.NewServer(api.Routes())
	defer server.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+types.PoolAvailabilityPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pool status HTTP %d", resp.StatusCode)
	}
	var result types.PoolAvailabilityView
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	rows := result.Pools
	if len(rows) != 3 || rows[0].Status != "available" || rows[1].ReasonCode != "eks-add-first-disabled" || rows[2].ReasonCode != cloud.PoolAutopilot || !strings.Contains(rows[2].Action, "GKE Standard") || strings.Contains(rows[2].Action, "RemoveFirst") {
		t.Fatalf("unexpected pool assessments: %+v", rows)
	}
	if len(p.checks) != 2 || p.checks[0] == p.checks[1] || p.checks[0] == "eks" || p.checks[1] == "eks" {
		t.Fatalf("unexpected checks %v", p.checks)
	}
}

type blockingPoolPlatform struct {
	*poolAssessmentPlatform
	started chan struct{}
}

func (p *blockingPoolPlatform) AddFirstAvailable(ctx context.Context, _ cloud.NodeIdentity) error {
	close(p.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestInventoryWrittenBeforePoolTimeout(t *testing.T) {
	c, st, inv := inventoryFixture(t)
	c.SetEngine(poolTestEngine(t))
	p := &blockingPoolPlatform{&poolAssessmentPlatform{inventoryPlatform: inv}, make(chan struct{})}
	p.nodes = []types.Node{{Name: "gke", UID: "gke-uid", Labels: map[string]string{"cloud.google.com/gke-nodepool": "train"}, Devices: tpuDevices}}
	c.platform = p
	done := make(chan struct{})
	go func() { defer close(done); c.syncAcceleratorInventory(t.Context()) }()
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("pool check did not start")
	}
	if _, err := st.GetNode(t.Context(), "gke"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		t.Fatal("pool check exceeded timeout")
	}
}

func TestPoolCheckBudget(t *testing.T) {
	var c Controller
	var nodes []types.Node
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		nodes = append(nodes, types.Node{Name: name, Labels: map[string]string{"cloud.google.com/gke-nodepool": name}, Devices: tpuDevices})
	}
	now := time.Now().UTC()
	calls := 0
	check := func(types.Node) error { calls++; return nil }
	for _, want := range []int{4, 6} {
		c.inventory.recordPools(nodes, poolTestEngine(t), now, check)
		pending := 0
		for _, row := range c.PoolAvailability().Pools {
			if row.CheckedAt == nil && row.Status == "unknown" {
				pending++
			}
		}
		if calls != want || pending != len(nodes)-want {
			t.Fatalf("calls=%d pending=%d want %d checked", calls, pending, want)
		}
	}
}

func poolTestEngine(t *testing.T) *playbook.Engine {
	t.Helper()
	e, err := playbook.NewEngine(map[string]*playbook.Playbook{"replace": {Steps: []playbook.Step{{Action: "platform.replace_node"}}}}, []playbook.Policy{{Class: types.ClassFellOffBus, Playbook: "replace"}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestPoolWithoutReplacementPolicySkipsCheck(t *testing.T) {
	var c Controller
	nodes := []types.Node{{Name: "a", Labels: map[string]string{"kubernetes.azure.com/agentpool": "train"}, Devices: tpuDevices}}
	c.inventory.recordPools(nodes, nil, time.Now(), func(types.Node) error { t.Fatal("unexpected cloud check"); return nil })
	row := c.PoolAvailability().Pools[0]
	if row.ReplacementPolicy != "none" || row.Status != "not applicable" || row.CheckedAt != nil {
		t.Fatal(row)
	}
}
