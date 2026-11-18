// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// draHealthPlatform serves Node health and scripted DRA devices.
type draHealthPlatform struct {
	healthPlatform
	dmu     sync.Mutex
	devices map[string][]types.DRADevice
	// gone are the Nodes that no longer exist; any other Node named in
	// devices, and gpu-1, exists.
	gone  map[string]bool
	err   error
	calls int
}

func (p *draHealthPlatform) DRAHealth(context.Context) ([]types.NodeHealthSnapshot, error) {
	p.dmu.Lock()
	defer p.dmu.Unlock()
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	var out []types.NodeHealthSnapshot
	if _, ok := p.devices["gpu-1"]; !ok && !p.gone["gpu-1"] {
		out = append(out, types.NodeHealthSnapshot{Node: "gpu-1", ReadAt: p.clock.Now()})
	}
	for node, devs := range p.devices {
		if p.gone[node] {
			continue
		}
		out = append(out, types.NodeHealthSnapshot{Node: node, ReadAt: p.clock.Now(), DRADevices: append([]types.DRADevice(nil), devs...)})
	}
	return out, nil
}

func (p *draHealthPlatform) setDevices(node string, devs ...types.DRADevice) {
	p.dmu.Lock()
	defer p.dmu.Unlock()
	if p.devices == nil {
		p.devices = map[string][]types.DRADevice{}
	}
	delete(p.gone, node)
	if devs == nil {
		delete(p.devices, node)
		return
	}
	p.devices[node] = devs
}

// removeNode deletes the Node, as the end of its replacement does.
func (p *draHealthPlatform) removeNode(node string) {
	p.dmu.Lock()
	defer p.dmu.Unlock()
	delete(p.devices, node)
	if p.gone == nil {
		p.gone = map[string]bool{}
	}
	p.gone[node] = true
}

const draUUID = "GPU-0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d"

func gpuDevice(taints ...types.DRATaint) types.DRADevice {
	return types.DRADevice{Driver: "gpu.nvidia.com", Pool: "gpu-1", Device: "gpu-0", UUID: draUUID, Taints: taints}
}

func draFixture(t *testing.T, enabled bool) (*Controller, *draHealthPlatform, *fakeClock) {
	t.Helper()
	c, _, _, clock := healthFixture(t)
	plat := &draHealthPlatform{healthPlatform: healthPlatform{clock: clock}}
	c.platform = plat
	rc := *c.runtime.Load()
	rc.Health = HealthPolicy{DRA: enabled}
	c.runtime.Store(&rc)
	return c, plat, clock
}

func draPending(c *Controller) []string {
	c.health.mu.Lock()
	defer c.health.mu.Unlock()
	var out []string
	for k := range c.health.pending {
		if strings.HasPrefix(k, "dra/") {
			out = append(out, k)
		}
	}
	return out
}

func openGPUIncident(c *Controller, class types.ProblemClass) *types.Incident {
	inc, err := c.store.GetOpenIncident(context.Background(), types.Target{Node: "gpu-1"}, class)
	if err != nil {
		return nil
	}
	return inc
}

// A fatal XID taint opens one incident on the node after two reads, with the
// device and its UUID in the evidence.
func TestDRAHealthFaultOpensAnIncident(t *testing.T) {
	c, plat, clock := draFixture(t, true)
	ctx := context.Background()
	plat.setDevices("gpu-1", gpuDevice(types.DRATaint{Key: "gpu.nvidia.com/xid", Value: "79", Effect: "NoSchedule", TimeAdded: clock.Now()}))
	c.pollNodeHealth(ctx)
	if inc := openGPUIncident(c, types.ClassFellOffBus); inc != nil {
		t.Fatalf("one read of a new taint opened %+v", inc)
	}
	clock.Advance(30 * time.Second)
	c.pollNodeHealth(ctx)
	inc := openGPUIncident(c, types.ClassFellOffBus)
	if inc == nil {
		t.Fatal("two reads of a fatal XID taint opened no incident")
	}
	if inc.Target.Node != "gpu-1" || inc.Target.IsDeviceScoped() {
		t.Errorf("target %+v: the incident stays on the node", inc.Target)
	}
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil || len(trail) == 0 {
		t.Fatalf("trail %v, %v", trail, err)
	}
	for k, v := range map[string]string{"health_source": "dra", "profile": "nvidia", "rule": "dra-device-taint",
		"signal": "dra:gpu.nvidia.com/gpu-1/gpu-0=xid/79", "reason": "xid-79", "device": "gpu-0", "gpu_uuid": draUUID} {
		if got := trail[0].Params[k]; got != v {
			t.Errorf("evidence %s = %q, want %q", k, got, v)
		}
	}
}

// A device that loses its taint while still published clears the pending
// fault; one that is no longer published, or a failed read, holds it.
func TestDRAHealthOnlyAnUntaintedDeviceClears(t *testing.T) {
	c, plat, clock := draFixture(t, true)
	ctx := context.Background()
	fault := types.DRATaint{Key: "gpu.nvidia.com/xid", Value: "79", Effect: "NoSchedule", TimeAdded: clock.Now()}
	plat.setDevices("gpu-1", gpuDevice(fault))
	c.pollNodeHealth(ctx)
	if len(draPending(c)) != 1 {
		t.Fatalf("pending %v", draPending(c))
	}
	plat.setDevices("gpu-1")
	clock.Advance(30 * time.Second)
	c.pollNodeHealth(ctx)
	if len(draPending(c)) != 1 {
		t.Fatalf("a device gone from the slices cleared its pending fault: %v", draPending(c))
	}
	plat.dmu.Lock()
	plat.err = &platform.DRAHealthError{Reason: "forbidden", Err: errors.New("403")}
	plat.dmu.Unlock()
	c.pollNodeHealth(ctx)
	if len(draPending(c)) != 1 {
		t.Fatalf("a failed read cleared the pending fault: %v", draPending(c))
	}
	if got := c.healthRecoveryRefusal("gpu-1", "platform.cordon"); got != "" {
		t.Fatalf("a failed DRA read holds node steps: %q", got)
	}
	plat.dmu.Lock()
	plat.err = nil
	plat.dmu.Unlock()
	plat.setDevices("gpu-1", gpuDevice())
	c.pollNodeHealth(ctx)
	if p := draPending(c); len(p) != 0 {
		t.Fatalf("the device published without a taint did not clear the fault: %v", p)
	}
}

// A replaced node whose Node is deleted takes its pending fault with it:
// nothing is held for a node that no longer exists, so the incident verifies
// and closes as any other, and no new one opens.
func TestDRAHealthGoneNodeDropsItsFault(t *testing.T) {
	c, plat, clock := draFixture(t, true)
	ctx := context.Background()
	plat.setDevices("gpu-1", gpuDevice(types.DRATaint{Key: "gpu.nvidia.com/xid", Value: "79", Effect: "NoSchedule", TimeAdded: clock.Now().Add(-time.Hour)}))
	c.pollNodeHealth(ctx)
	inc := openGPUIncident(c, types.ClassFellOffBus)
	if inc == nil {
		t.Fatal("no incident")
	}
	inc.State = types.StateVerifying
	if err := c.store.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	plat.removeNode("gpu-1")
	clock.Advance(30 * time.Second)
	c.pollNodeHealth(ctx)
	if p := draPending(c); len(p) != 0 {
		t.Fatalf("a Node that is gone holds %v", p)
	}
	if got := testutil.ToFloat64(metrics.AcceleratorHealthNodes.WithLabelValues("dra", "nvidia", "unknown")); got != 0 {
		t.Fatalf("a Node that is gone is reported unknown: %v", got)
	}
	if inc = openGPUIncident(c, types.ClassFellOffBus); inc == nil || inc.SignalSeen != 1 {
		t.Fatalf("incident %+v: the gone node added a signal", inc)
	}
	if err := c.ResolveIncident(ctx, inc.ID, "operator", "test"); err != nil {
		t.Fatal(err)
	}
	c.pollNodeHealth(ctx)
	if next := openGPUIncident(c, types.ClassFellOffBus); next != nil {
		t.Fatalf("a gone node opened %+v", next)
	}

	// The replacement joins under the same name with a new UID and an
	// untainted device: nothing is pending or unknown.
	plat.setDevices("gpu-1", gpuDevice())
	c.pollNodeHealth(ctx)
	if p := draPending(c); len(p) != 0 {
		t.Fatalf("the replacement holds %v", p)
	}
	if got := testutil.ToFloat64(metrics.AcceleratorHealthNodes.WithLabelValues("dra", "nvidia", "unknown")); got != 0 {
		t.Fatalf("the replacement is reported unknown: %v", got)
	}
	if next := openGPUIncident(c, types.ClassFellOffBus); next != nil {
		t.Fatalf("the replacement opened %+v", next)
	}
}

// A device without a taint is no signal: no observation, not counted as
// healthy or unknown, and it holds no node step.
func TestDRAHealthNoTaintIsNoSignal(t *testing.T) {
	c, plat, _ := draFixture(t, true)
	plat.setDevices("gpu-1", gpuDevice(), types.DRADevice{Driver: "gpu.nvidia.com", Pool: "gpu-1", Device: "gpu-1",
		Taints: []types.DRATaint{{Key: "gpu.nvidia.com/unmonitored", Effect: "None"}}})
	c.pollNodeHealth(context.Background())
	for state, want := range map[string]float64{"no-health-taint": 1, "unmonitored": 1, "unknown": 0, "fault": 0} {
		if got := testutil.ToFloat64(metrics.DRAHealthDevices.WithLabelValues("nvidia", state)); got != want {
			t.Errorf("devices %s = %v, want %v", state, got, want)
		}
	}
	for _, state := range []string{"healthy", "unknown"} {
		if got := testutil.ToFloat64(metrics.AcceleratorHealthNodes.WithLabelValues("dra", "nvidia", state)); got != 0 {
			t.Errorf("nodes %s = %v: a device without a taint says nothing", state, got)
		}
	}
	if got := c.healthRecoveryRefusal("gpu-1", "platform.replace_node"); got != "" {
		t.Fatalf("refused: %q", got)
	}
}

// Ignored taints are counted by reason.
func TestDRAHealthIgnoredByReason(t *testing.T) {
	c, plat, _ := draFixture(t, true)
	plat.setDevices("gpu-1", gpuDevice(types.DRATaint{Key: "example.com/maintenance", Value: "free text", Effect: "NoExecute"}))
	c.pollNodeHealth(context.Background())
	if got := testutil.ToFloat64(metrics.AcceleratorHealthIgnoredNodes.WithLabelValues("dra", "nvidia", "undocumented-taint")); got != 1 {
		t.Errorf("ignored nodes %v, want 1", got)
	}
	if inc := openGPUIncident(c, types.ClassAcceleratorFault); inc != nil {
		t.Fatalf("an undocumented taint opened %+v", inc)
	}
}

// Off, the source is not read and holds nothing; on, node steps wait for
// its first read in the term.
func TestDRAHealthOptIn(t *testing.T) {
	c, plat, _ := draFixture(t, false)
	c.pollNodeHealth(context.Background())
	if plat.calls != 0 {
		t.Fatalf("%d reads while off", plat.calls)
	}
	c, plat, _ = draFixture(t, true)
	plat.setDevices("gpu-1", gpuDevice())
	if got := c.healthRecoveryRefusal("gpu-1", "platform.cordon"); !strings.Contains(got, "has not been read") {
		t.Fatalf("refusal %q, want the hold until the first DRA read", got)
	}
	c.pollNodeHealth(context.Background())
	if got := c.healthRecoveryRefusal("gpu-1", "platform.cordon"); got != "" {
		t.Fatalf("refused after the read: %q", got)
	}
}
