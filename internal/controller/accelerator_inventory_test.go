// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kubeneuron/kubeneuron/internal/httpapi"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
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
		if row.Status != "unknown" || row.Reason != "no cloud provider configured" || row.ReplacementReason != row.Reason || row.CheckedAt != nil {
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
	nodes := []types.Node{{Name: "worker", UID: "current", Labels: map[string]string{"eks.amazonaws.com/nodegroup": "train"}, Devices: []types.DeviceCount{{Resource: "amd.com/gpu"}}}}
	for _, tc := range []struct{ uid, want string }{
		{"previous", "AddFirst"}, {"", "AddFirst"}, {"current", "mixed"},
	} {
		reports := []*types.AgentAcceleratorReport{{Node: "worker", NodeUID: tc.uid, Vendor: "amd"}}
		c.inventory.recordPools(nodes, reports, &RuntimeConfig{Engine: engine}, time.Now(), nil, nil)
		if got := c.PoolAvailability().Pools[0].ReplacementPolicy; got != tc.want {
			t.Fatalf("UID %q: got %s want %s", tc.uid, got, tc.want)
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
	check := func(types.Node, bool, recoverymode.SuccessorZone, *[]string) error {
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
		c.inventory.recordPools(nodes, nil, &RuntimeConfig{Engine: poolTestEngine(t)}, now.Add(tc.after), check, nil)
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
func (p *poolAssessmentPlatform) AssessAddFirstPool(_ context.Context, pin cloud.NodeIdentity) error {
	p.checks = append(p.checks, pin.Node)
	return &cloud.PoolRefusal{Code: cloud.PoolFixedSizeUndeclared, Reason: "EKS does not say whether an autoscaler manages this node group", Action: "tag the node group kubeneuron.io/fixed-size=true"}
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
	if len(rows) != 3 || rows[0].Status != "available" || rows[1].ReasonCode != cloud.PoolFixedSizeUndeclared || rows[1].CapabilityEnabled || !strings.Contains(rows[1].Action, "kubeneuron.io/fixed-size=true") || rows[2].ReasonCode != cloud.PoolAutopilot || !strings.Contains(rows[2].Action, "GKE Standard") || strings.Contains(rows[2].Action, "RemoveFirst") {
		t.Fatalf("unexpected pool assessments: %+v", rows)
	}
	if len(p.checks) != 3 || !slices.Contains(p.checks, "eks") {
		t.Fatalf("unexpected checks %v", p.checks)
	}
}

type permissionHoldPlatform struct {
	*poolAssessmentPlatform
}

func (p *permissionHoldPlatform) ReplacePermission(context.Context, cloud.NodeIdentity) error {
	return &cloud.RecoveryHold{Code: cloud.HoldAPIDisabled, Cause: errors.New("googleapi: Error 403"),
		Reason: "KubeNeuron could not check the controller's permissions", PoolReason: "could not check permissions: cloudresourcemanager.googleapis.com is disabled in the project",
		PoolAction: "enable cloudresourcemanager.googleapis.com in the cluster's project"}
}

// A pool is not "available" while the incident path would hold on the
// controller's permissions.
func TestPoolCheckAsksForThePermissionsAReplacementNeeds(t *testing.T) {
	c, _, inv := inventoryFixture(t)
	c.SetEngine(poolTestEngine(t))
	c.platform = &permissionHoldPlatform{&poolAssessmentPlatform{inventoryPlatform: inv}}
	inv.nodes = append(inv.nodes, types.Node{Name: "g1", UID: "g1-uid", Labels: map[string]string{"cloud.google.com/gke-nodepool": "train"}, Devices: tpuDevices})
	c.syncAcceleratorInventory(t.Context())
	c.syncAcceleratorInventory(t.Context())
	rows := c.PoolAvailability().Pools
	if len(rows) != 1 || rows[0].Status != "unavailable" || rows[0].ReasonCode != cloud.HoldAPIDisabled ||
		!strings.Contains(rows[0].Action, "enable cloudresourcemanager.googleapis.com") || strings.Contains(rows[0].Reason, "googleapi:") {
		t.Fatalf("pool rows %+v", rows)
	}
}

type eligibilityPermissionPlatform struct {
	*poolAssessmentPlatform
	verdict     cloud.RecoveryEligibility
	permissions int
}

func (p *eligibilityPermissionPlatform) PoolRecoveryEligibility(context.Context, cloud.NodeIdentity) (cloud.RecoveryEligibility, *cloud.RecoveryHold, error) {
	return p.verdict, nil, nil
}

func (p *eligibilityPermissionPlatform) ReplacePermission(context.Context, cloud.NodeIdentity) error {
	p.permissions++
	return nil
}

// A pool check asks for the replace permissions once: the add-first check
// does not repeat what the eligibility check already asked.
func TestPoolCheckAsksForThePermissionsOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict cloud.RecoveryEligibility
		want    int
	}{
		{"eligibility checked them", cloud.RecoveryEligibility{Mode: cloud.RepairOff, ReplaceAllowed: true}, 0},
		{"eligibility stopped before them", cloud.RecoveryEligibility{Mode: cloud.RepairOff}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, inv := inventoryFixture(t)
			c.SetEngine(poolTestEngine(t))
			p := &eligibilityPermissionPlatform{poolAssessmentPlatform: &poolAssessmentPlatform{inventoryPlatform: inv}, verdict: tc.verdict}
			c.platform = p
			inv.nodes = append(inv.nodes, types.Node{Name: "g1", UID: "g1-uid", Labels: map[string]string{"cloud.google.com/gke-nodepool": "train"}, Devices: tpuDevices})
			c.syncAcceleratorInventory(t.Context())
			if rows := c.PoolAvailability().Pools; len(rows) != 1 || rows[0].CheckedAt == nil {
				t.Fatalf("pool rows %+v", rows)
			}
			if p.permissions != tc.want {
				t.Fatalf("ReplacePermission called %d times, want %d", p.permissions, tc.want)
			}
		})
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
	check := func(types.Node, bool, recoverymode.SuccessorZone, *[]string) error { calls++; return nil }
	for _, want := range []int{4, 6} {
		c.inventory.recordPools(nodes, nil, &RuntimeConfig{Engine: poolTestEngine(t)}, now, check, nil)
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
	later := now.Add(11 * time.Minute)
	c.inventory.recordPools(nodes, nil, &RuntimeConfig{Engine: poolTestEngine(t)}, later, check, nil)
	stale := 0
	for _, row := range c.PoolAvailability().Pools {
		if row.Status != "available" || row.CheckedAt == nil {
			t.Fatalf("lost assessment: %+v", row)
		}
		if row.CheckedAt.Equal(now) {
			stale++
		} else if !row.CheckedAt.Equal(later) {
			t.Fatalf("unexpected check time: %+v", row)
		}
	}
	if stale != 2 || calls != 10 {
		t.Fatalf("stale=%d calls=%d", stale, calls)
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
	c.inventory.recordPools(nodes, nil, &RuntimeConfig{}, time.Now(), func(types.Node, bool, recoverymode.SuccessorZone, *[]string) error {
		t.Fatal("unexpected cloud check")
		return nil
	}, nil)
	row := c.PoolAvailability().Pools[0]
	if row.ReplacementPolicy != "none" || row.Status != "not applicable" || row.CheckedAt != nil {
		t.Fatal(row)
	}
}

func TestTrainiumPoolOrderMatchesNodeHealthIncident(t *testing.T) {
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"repair": {Steps: []playbook.Step{{Action: "platform.replace_node"}}}}, []playbook.Policy{
		{Class: types.ClassAcceleratorFault, Vendor: "aws", Playbook: "repair"},
		{Class: types.ClassAcceleratorFault, Playbook: "repair", ReplacementStrategy: recoverymode.RemoveFirst},
	})
	if err != nil {
		t.Fatal(err)
	}
	signal := types.Signal{Class: types.ClassAcceleratorFault, Source: types.SourceNodeHealth, Evidence: map[string]string{"vendor": "aws"}}
	policy, ok := engine.PolicyFor(signal.Class, signal.Vendor())
	if !ok || policy.ReplacementStrategy != recoverymode.RemoveFirst {
		t.Fatalf("unexpected node-health policy: %+v", policy)
	}
	for _, resource := range []string{"aws.amazon.com/neuron", "aws.amazon.com/neurondevice"} {
		var c Controller
		nodes := []types.Node{{Name: "trainium", UID: "trn-uid", Labels: map[string]string{"eks.amazonaws.com/nodegroup": "train"}, Devices: []types.DeviceCount{{Resource: resource, Capacity: 1, Unit: types.DeviceUnitAWSDevice}}}}
		c.inventory.recordPools(nodes, nil, &RuntimeConfig{Engine: engine}, time.Now(), nil, nil)
		if row := c.PoolAvailability().Pools[0]; row.ReplacementPolicy != "RemoveFirst" {
			t.Fatalf("%s: %+v", resource, row)
		}
		reports := []*types.AgentAcceleratorReport{{Node: "trainium", NodeUID: "trn-uid", Vendor: "aws"}}
		c.inventory.recordPools(nodes, reports, &RuntimeConfig{Engine: engine}, time.Now(), nil, nil)
		if row := c.PoolAvailability().Pools[0]; row.ReplacementPolicy != "mixed" {
			t.Fatalf("%s with authenticated AWS report: %+v", resource, row)
		}

	}
}

type liveRepairInventory struct {
	*inventoryPlatform
	uid     string
	readErr error
}

func (p *liveRepairInventory) LiveNodeUID(context.Context, string) (string, bool, error) {
	return p.uid, p.uid != "", p.readErr
}

func TestRepairInventoryRequiresLiveConfirmation(t *testing.T) {
	c, st, p := inventoryFixture(t)
	live := &liveRepairInventory{inventoryPlatform: p, uid: "original"}
	c.platform = live
	ctx := t.Context()
	inc := &types.Incident{ID: "owner", Target: types.Target{Node: "worker"}, State: types.StateEvaluating}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := st.WithTx(ctx, func(tx store.Tx) error {
		_, err := tx.ClaimNodeRemediation(ctx, "worker", "original", inc.ID, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		c.syncAcceleratorInventory(ctx)
		rows, err := st.ListRepairNodeIdentities(ctx)
		if err != nil || len(rows) != want {
			t.Fatalf("claims=%v err=%v", rows, err)
		}
	}
	check(1)
	live.readErr = errors.New("unavailable")
	live.uid = ""
	check(1)
	live.readErr = nil
	check(0)
}

func TestPoolRowShowsRepairHold(t *testing.T) {
	nodes := []types.Node{{Name: "eks", UID: "eks-uid", Labels: map[string]string{"eks.amazonaws.com/nodegroup": "train"}, Devices: tpuDevices}}
	on := &cloud.RecoveryHold{Code: cloud.HoldNativeRepair, Reason: "EKS node auto repair is enabled for node group train, so KubeNeuron does not replace its nodes.", Action: "Leave repairs of this group to EKS.", PoolReason: "EKS node auto repair is enabled for this node group", PoolAction: "leave repairs to EKS"}
	denied := &cloud.RecoveryHold{Code: cloud.HoldReadFailed, Reason: "KubeNeuron could not read node group train: eks:ListUpdates was denied.", Action: "Allow eks:ListUpdates in the controller's IAM role. KubeNeuron retries every minute.", PoolReason: "could not read the node group: eks:ListUpdates was denied", PoolAction: "allow eks:ListUpdates in the controller's IAM role", Cause: errors.New("raw")}
	failed := &cloud.RecoveryHold{Code: cloud.HoldReadFailed, Reason: "KubeNeuron could not read node group train: eks:ListUpdates failed.", Action: "KubeNeuron retries every minute.", PoolReason: "could not read the node group: eks:ListUpdates failed"}
	unconfirmed := &cloud.RecoveryHold{Code: cloud.HoldUnconfirmed, Reason: "KubeNeuron could not confirm that EKS is not repairing node group train: EKS, Auto Scaling or EC2 did not confirm the node's group and state.", Action: "KubeNeuron checks again every minute."}
	for _, tc := range []struct {
		hold                   *cloud.RecoveryHold
		status, reason, action string
	}{
		{on, "unavailable", on.PoolReason, on.PoolAction},
		{denied, "unknown", denied.PoolReason, denied.PoolAction},
		{failed, "unknown", failed.PoolReason, ""},
		{unconfirmed, "unknown", unconfirmed.Reason, unconfirmed.Action},
	} {
		var c Controller
		c.inventory.recordPools(nodes, nil, &RuntimeConfig{Engine: poolTestEngine(t)}, time.Now().UTC(), func(types.Node, bool, recoverymode.SuccessorZone, *[]string) error {
			return fmt.Errorf("assess: %w", tc.hold)
		}, nil)
		row := c.PoolAvailability().Pools[0]
		if row.Status != tc.status || row.ReasonCode != tc.hold.Code || row.Reason != tc.reason || row.Action != tc.action {
			t.Fatalf("%s: row %+v", tc.hold.Code, row)
		}
	}
}

func TestNodesAndDiscoveryUseCurrentIdentities(t *testing.T) {
	c, st, inv := inventoryFixture(t)
	ctx := t.Context()
	register := func(uid string) {
		t.Helper()
		if err := st.UpsertAgentRegistration(ctx, &types.Node{Name: "worker", UID: uid, AgentLastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	api := httpapi.New(c)
	api.EnableOperatorAPI(c, "test-token")
	api.SetReadyCheck(func() bool { return true })
	handler := api.Routes()
	check := func(status int, uid string) {
		t.Helper()
		for _, path := range []string{"/api/v1/nodes", "/api/v1/targets"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer test-token")
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != status {
				t.Fatalf("%s: HTTP %d: %s", path, resp.Code, resp.Body.String())
			}
			if status != http.StatusOK {
				continue
			}
			if uid == "" {
				if strings.TrimSpace(resp.Body.String()) != "[]" {
					t.Fatalf("%s: expected empty list, got %s", path, resp.Body.String())
				}
				continue
			}
			if path == "/api/v1/nodes" {
				var rows []types.Node
				if err := json.Unmarshal(resp.Body.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Name != "worker" || rows[0].UID != uid {
					t.Fatalf("%s: %s", path, resp.Body.String())
				}
			} else {
				var groups []struct{ Targets []string }
				if err := json.Unmarshal(resp.Body.Bytes(), &groups); err != nil || len(groups) != 1 || !slices.Equal(groups[0].Targets, []string{"worker:9400"}) {
					t.Fatalf("%s: %s", path, resp.Body.String())
				}
			}
		}
	}
	register("first")
	inv.nodes = []types.Node{{Name: "worker", UID: "first"}}
	check(http.StatusOK, "first")
	inv.nodes = nil
	check(http.StatusOK, "")
	inv.nodes = []types.Node{{Name: "worker", UID: "second"}}
	check(http.StatusOK, "")
	register("second")
	check(http.StatusOK, "second")
	inv.err = errors.New("inventory unavailable")
	check(http.StatusInternalServerError, "")
	c.platform = nil
	check(http.StatusOK, "second")
}

func TestInventorySummaryDropsDeletedRegistrations(t *testing.T) {
	c, st := newIngestTestController(t)
	p := &inventoryPlatform{}
	c.platform = p
	register := func(uid string) {
		t.Helper()
		if err := st.UpsertAgentRegistration(t.Context(), &types.Node{Name: "worker", UID: uid, AgentLastSeen: time.Now(), GPUs: []types.GPUInfo{{UUID: "gpu"}}}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(count int) {
		t.Helper()
		s, err := c.InventorySummary(t.Context())
		if err != nil || s.ReportingNodes != count || s.GPUs != count || s.LastReportAt.IsZero() != (count == 0) {
			t.Fatalf("summary = %+v, error = %v, want %d reporting nodes and GPUs", s, err, count)
		}
	}
	register("old")
	p.nodes = []types.Node{{Name: "worker", UID: "old"}}
	check(1)
	p.nodes = nil
	check(0)
	p.nodes = []types.Node{{Name: "worker", UID: "new"}}
	check(0)
	register("new")
	check(1)
}

// The labels of an agent's node are its Node object's now, while it is the
// node the agent registered. The rest of the row stays the agent's, and a
// new UID under the name waits for its agent.
func TestInventoryRecordsTheLabelsOfTheAgentsNode(t *testing.T) {
	c, st, plat := inventoryFixture(t)
	ctx := context.Background()
	gpus := []types.GPUInfo{{Index: 0, UUID: "GPU-0"}}
	register := func(uid string) {
		t.Helper()
		if err := st.UpsertAgentRegistration(ctx, &types.Node{Name: "gpu-1", UID: uid, GPUs: gpus, AgentLastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	labelsNow := func(want string) {
		t.Helper()
		c.syncAcceleratorInventory(ctx)
		got, err := st.GetNode(ctx, "gpu-1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Labels["pool"] != want || got.InventorySource != types.InventoryAgent || !reflect.DeepEqual(got.GPUs, gpus) ||
			len(got.Devices) != 0 || got.AgentLastSeen.IsZero() {
			t.Fatalf("row = %+v, want pool %q and the agent's row otherwise", got, want)
		}
	}
	register("uid-1")
	live := func(uid, pool string) {
		plat.imu.Lock()
		plat.nodes = []types.Node{{Name: "gpu-1", UID: uid, Labels: map[string]string{"pool": pool}, Devices: []types.DeviceCount{
			{Resource: "nvidia.com/gpu", Capacity: 8, Allocatable: 8, Unit: types.DeviceUnitGPU},
		}}}
		plat.imu.Unlock()
	}
	live("uid-1", "a")
	labelsNow("a")
	live("uid-1", "b")
	labelsNow("b")
	// The node recreated: the earlier machine's row keeps its labels until
	// the new machine's agent registers.
	live("uid-2", "c")
	labelsNow("b")
	register("uid-2")
	labelsNow("c")
	if st.writes != 0 {
		t.Fatalf("%d Kubernetes inventory writes to the agent's row", st.writes)
	}
}
