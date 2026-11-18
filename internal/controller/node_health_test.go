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
	"github.com/kubeneuron/kubeneuron/internal/nodehealth"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// healthPlatform serves scripted Node health snapshots.
type healthPlatform struct {
	basePlatform
	hmu   sync.Mutex
	snaps []types.NodeHealthSnapshot
	err   error
	clock *fakeClock
}

func (p *healthPlatform) NodeHealth(context.Context) ([]types.NodeHealthSnapshot, error) {
	p.hmu.Lock()
	defer p.hmu.Unlock()
	out := append([]types.NodeHealthSnapshot(nil), p.snaps...)
	for i := range out {
		out[i].ReadAt = p.clock.Now()
	}
	return out, p.err
}

func (p *healthPlatform) set(snaps ...types.NodeHealthSnapshot) {
	p.hmu.Lock()
	defer p.hmu.Unlock()
	p.snaps = snaps
}

func healthFixture(t *testing.T) (*Controller, store.Store, *healthPlatform, *fakeClock) {
	t.Helper()
	c, st, _ := inventoryFixture(t)
	clock := &fakeClock{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	plat := &healthPlatform{clock: clock}
	c.platform = plat
	c.checkpointNow = clock.Now
	return c, st, plat, clock
}

// neuronNode is a Trainium node whose Neuron node problem detector reports
// status on NeuronHealth, renewed a minute before now.
func neuronNode(clock *fakeClock, status, reason string) types.NodeHealthSnapshot {
	return types.NodeHealthSnapshot{
		Node: "trn-1", UID: "uid-trn-1",
		Devices: []types.DeviceCount{{Resource: "aws.amazon.com/neuron", Capacity: 1, Allocatable: 1, Unit: types.DeviceUnitAWSDevice}},
		Conditions: []types.NodeHealthCondition{{Type: "NeuronHealth", Status: status, Reason: reason,
			LastHeartbeatTime: clock.Now().Add(-time.Minute), LastTransitionTime: clock.Now().Add(-time.Hour)}},
	}
}

const neuronDMA = "NeuronHasError_DMA_ERROR"

func openHealthIncident(t *testing.T, st store.Store, class types.ProblemClass) *types.Incident {
	t.Helper()
	inc, err := st.GetOpenIncident(context.Background(), types.Target{Node: "trn-1"}, class)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return inc
}

func TestNodeHealthFaultOpensAnIncidentWithItsEvidence(t *testing.T) {
	c, st, plat, clock := healthFixture(t)
	ctx := context.Background()
	plat.set(neuronNode(clock, "True", neuronDMA))
	c.pollNodeHealth(ctx)
	if inc := openHealthIncident(t, st, types.ClassAcceleratorFault); inc != nil {
		t.Fatalf("one read opened an incident: %+v", inc)
	}
	clock.Advance(10 * time.Second)
	c.pollNodeHealth(ctx)
	inc := openHealthIncident(t, st, types.ClassAcceleratorFault)
	if inc == nil {
		t.Fatal("two reads of a fault opened no incident")
	}
	trail, err := st.AuditTrail(ctx, inc.ID)
	if err != nil || len(trail) == 0 {
		t.Fatalf("trail %v, %v", trail, err)
	}
	got := trail[0].Params
	want := map[string]string{
		"health_source": "kubernetes-node", "profile": "aws-neuron", "profile_version": "v1",
		"rule": "neuron-npd", "signal": "condition:NeuronHealth=True", "reason": neuronDMA,
		"since": "2026-11-18T11:00:00Z", "other_repairer": "unknown",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("evidence %s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

// A fault that goes away before it is confirmed starts over.
func TestNodeHealthFlapOpensNothing(t *testing.T) {
	c, st, plat, clock := healthFixture(t)
	ctx := context.Background()
	for range 3 {
		plat.set(neuronNode(clock, "True", neuronDMA))
		c.pollNodeHealth(ctx)
		clock.Advance(10 * time.Second)
		plat.set(neuronNode(clock, "False", ""))
		c.pollNodeHealth(ctx)
		clock.Advance(10 * time.Second)
	}
	if inc := openHealthIncident(t, st, types.ClassAcceleratorFault); inc != nil {
		t.Fatalf("a flapping signal opened an incident: %+v", inc)
	}
}

// A stale signal neither confirms a fault nor clears it.
func TestNodeHealthStaleSignalIsHeld(t *testing.T) {
	c, st, plat, clock := healthFixture(t)
	ctx := context.Background()
	plat.set(neuronNode(clock, "True", neuronDMA))
	c.pollNodeHealth(ctx)

	stale := neuronNode(clock, "True", neuronDMA)
	clock.Advance(20 * time.Minute)
	plat.set(stale)
	c.pollNodeHealth(ctx)
	c.pollNodeHealth(ctx)
	if inc := openHealthIncident(t, st, types.ClassAcceleratorFault); inc != nil {
		t.Fatalf("a stale signal confirmed a fault: %+v", inc)
	}
	if got := testutil.ToFloat64(metrics.AcceleratorHealthNodes.WithLabelValues("kubernetes-node", "aws-neuron", "unknown")); got != 1 {
		t.Fatalf("unknown nodes %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.AcceleratorHealthNodes.WithLabelValues("kubernetes-node", "aws-neuron", "healthy")); got != 0 {
		t.Fatalf("a stale node is counted healthy: %v", got)
	}

	// Fresh again: the read before the stale ones still counts.
	plat.set(neuronNode(clock, "True", neuronDMA))
	c.pollNodeHealth(ctx)
	if inc := openHealthIncident(t, st, types.ClassAcceleratorFault); inc == nil {
		t.Fatal("the fault was forgotten while its signal was stale")
	}
}

// allocatable below capacity must hold for five minutes: a device plugin
// restart is no incident.
func TestNodeHealthUnallocatableIsHeldFiveMinutes(t *testing.T) {
	c, st, plat, clock := healthFixture(t)
	ctx := context.Background()
	node := neuronNode(clock, "False", "")
	node.Devices[0].Allocatable = 0
	for range 29 {
		node.Conditions[0].LastHeartbeatTime = clock.Now()
		plat.set(node)
		c.pollNodeHealth(ctx)
		clock.Advance(10 * time.Second)
	}
	if inc := openHealthIncident(t, st, types.ClassAcceleratorUnallocatable); inc != nil {
		t.Fatalf("opened before five minutes: %+v", inc)
	}
	clock.Advance(20 * time.Second)
	node.Conditions[0].LastHeartbeatTime = clock.Now()
	plat.set(node)
	c.pollNodeHealth(ctx)
	if inc := openHealthIncident(t, st, types.ClassAcceleratorUnallocatable); inc == nil {
		t.Fatal("five minutes below capacity opened no incident")
	}
}

// While the incident is open the signal adds nothing; once the incident is
// closed with the signal still there, or verifies a recovery the node
// contradicts, the signal is reported again.
func TestNodeHealthReportsAgainOnlyWhenUnresolved(t *testing.T) {
	c, st, plat, clock := healthFixture(t)
	ctx := context.Background()
	plat.set(neuronNode(clock, "True", neuronDMA))
	for range 5 {
		c.pollNodeHealth(ctx)
		clock.Advance(10 * time.Second)
	}
	inc := openHealthIncident(t, st, types.ClassAcceleratorFault)
	if inc == nil || inc.SignalSeen != 1 {
		t.Fatalf("incident %+v, want one signal", inc)
	}

	inc.State = types.StateVerifying
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	c.pollNodeHealth(ctx)
	if inc = openHealthIncident(t, st, types.ClassAcceleratorFault); inc.SignalSeen != 2 {
		t.Fatalf("a verifying incident was not told the fault is still there: %d signals", inc.SignalSeen)
	}

	if err := c.ResolveIncident(ctx, inc.ID, "operator", "test"); err != nil {
		t.Fatal(err)
	}
	c.pollNodeHealth(ctx)
	next := openHealthIncident(t, st, types.ClassAcceleratorFault)
	if next == nil || next.ID == inc.ID {
		t.Fatalf("a fault still reported after its incident closed opened nothing new: %+v", next)
	}
}

// A signal read and not acted on opens nothing and is counted.
func TestNodeHealthIgnoredSignalOpensNothing(t *testing.T) {
	c, st, plat, clock := healthFixture(t)
	ctx := context.Background()
	node := types.NodeHealthSnapshot{Node: "trn-1",
		Devices: []types.DeviceCount{{Resource: "nvidia.com/gpu", Capacity: 8, Allocatable: 8}},
		Conditions: []types.NodeHealthCondition{{Type: "GPUECCError", Status: "True",
			LastHeartbeatTime: clock.Now(), LastTransitionTime: clock.Now()}}}
	plat.set(node)
	for range 3 {
		c.pollNodeHealth(ctx)
		clock.Advance(10 * time.Second)
	}
	if inc := openHealthIncident(t, st, types.ClassAcceleratorFault); inc != nil {
		t.Fatalf("an ignored signal opened an incident: %+v", inc)
	}
	if got := testutil.ToFloat64(metrics.AcceleratorHealthNodes.WithLabelValues("kubernetes-node", "nvidia", "ignored")); got != 1 {
		t.Fatalf("ignored nodes %v, want 1", got)
	}
}

func TestNodeHealthReadFailureChangesNothing(t *testing.T) {
	c, st, plat, clock := healthFixture(t)
	ctx := context.Background()
	plat.set(neuronNode(clock, "True", neuronDMA))
	c.pollNodeHealth(ctx)
	plat.err = errors.New("apiserver unavailable")
	c.pollNodeHealth(ctx)
	c.pollNodeHealth(ctx)
	plat.err = nil
	c.pollNodeHealth(ctx)
	if inc := openHealthIncident(t, st, types.ClassAcceleratorFault); inc == nil {
		t.Fatal("a failed read lost the pending fault")
	}
}

// Another repairer on the node holds every step that takes it out of
// service; a host of a multi-host TPU slice is never recycled or replaced
// alone.
func TestNodeHealthRefusesRecoveryItCannotOwn(t *testing.T) {
	c, _, plat, clock := healthFixture(t)
	ctx := context.Background()
	amd := types.NodeHealthSnapshot{Node: "mi-1",
		Devices: []types.DeviceCount{{Resource: "amd.com/gpu", Capacity: 8, Allocatable: 8}},
		Taints:  []types.NodeTaint{{Key: "amd-gpu-unhealthy", Effect: "NoSchedule"}}}
	tpu := types.NodeHealthSnapshot{Node: "tpu-1",
		Devices: []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Allocatable: 4}},
		Labels:  map[string]string{"cloud.google.com/gke-tpu-topology": "4x4", "cloud.google.com/gke-nodepool": "v5e-pool"}}
	spot := types.NodeHealthSnapshot{Node: "spot-1",
		Devices: []types.DeviceCount{{Resource: "nvidia.com/gpu", Capacity: 8, Allocatable: 8}},
		Taints:  []types.NodeTaint{{Key: "karpenter.sh/disrupted", Effect: "NoSchedule"}}}
	plat.set(amd, tpu, spot, neuronNode(clock, "False", ""))
	step := func(action string) *playbook.Step { return &playbook.Step{Name: "s", Action: action} }
	// Before the first read of this leadership term nothing is known about
	// another repairer: every step that takes a node is held. A failed read
	// does not count as one.
	unread := func(when string) {
		t.Helper()
		err := c.requireCloudRecovery(ctx, &types.Incident{ID: "i", Target: types.Target{Node: "spot-1"}}, step("platform.cordon"))
		if err == nil || !strings.Contains(err.Error(), "has not been read since this controller became leader") {
			t.Fatalf("cordon %s: %v", when, err)
		}
	}
	unread("before the first read")
	plat.err = errors.New("apiserver unavailable")
	c.pollNodeHealth(ctx)
	unread("after a failed read")
	plat.err = nil
	c.pollNodeHealth(ctx)

	refused := func(node, action string) string {
		err := c.requireCloudRecovery(ctx, &types.Incident{ID: "i", Target: types.Target{Node: node}}, step(action))
		if err == nil {
			return ""
		}
		return err.Error()
	}
	if got := refused("mi-1", "platform.cordon"); !strings.Contains(got, "another repairer is acting on the node (amd-gpu-operator)") {
		t.Fatalf("cordon under the AMD operator's repair: %q", got)
	}
	// The cordon of an announced interruption takes the same check: on a
	// node Karpenter is already disrupting, KubeNeuron only warns.
	interruption := &types.Incident{ID: "n", Class: types.ClassCloudInterruption, Target: types.Target{Node: "spot-1"}}
	if err := c.requireCloudRecovery(ctx, interruption, step("platform.cordon")); err == nil || !strings.Contains(err.Error(), "(karpenter)") {
		t.Fatalf("interruption cordon on a node Karpenter disrupts: %v", err)
	}
	if err := c.requireCloudRecovery(ctx, interruption, step(playbook.CheckpointWarningAction)); err != nil {
		t.Fatalf("checkpoint warning on a node Karpenter disrupts: %v", err)
	}
	if got := refused("tpu-1", "platform.replace_node"); !strings.Contains(got, "multi-host TPU slice in node pool v5e-pool") {
		t.Fatalf("replacing one host of a slice: %q", got)
	}
	if got := c.healthRecoveryRefusal("tpu-1", "platform.cordon"); got != "" {
		t.Fatalf("cordoning a slice host is refused: %q", got)
	}
	if got := c.healthRecoveryRefusal("trn-1", "platform.replace_node"); got != "" {
		t.Fatalf("a node with no other repairer is refused: %q", got)
	}
	c.health.reset()
	unread("in a new leadership term")
}

// On EKS with the agent, the same double-bit error reaches KubeNeuron twice:
// from the agent, for one GPU, and from the node monitoring agent's
// condition, for the node. The node's report is recorded on the GPU's
// incident; no second incident opens.
func TestNodeHealthJoinsTheAgentsDeviceIncident(t *testing.T) {
	c, st, plat, clock := healthFixture(t)
	ctx := context.Background()
	gpu := types.Target{Node: "gpu-1", GPUUUID: "GPU-abc", GPUIndex: 2}
	now := clock.Now()
	if err := st.CreateIncident(ctx, &types.Incident{ID: "agent-dbe", Class: types.ClassECCDBE, Target: gpu,
		State: types.StateObserving, SignalSeen: 1, OpenedAt: now, UpdatedAt: now, StateChangedAt: now}); err != nil {
		t.Fatal(err)
	}
	plat.set(types.NodeHealthSnapshot{Node: "gpu-1",
		Devices: []types.DeviceCount{{Resource: "nvidia.com/gpu", Capacity: 8, Allocatable: 8}},
		Conditions: []types.NodeHealthCondition{{Type: "AcceleratedHardwareReady", Status: "False", Reason: "NvidiaDoubleBitError",
			LastHeartbeatTime: now.Add(-time.Minute), LastTransitionTime: now.Add(-time.Hour)}}})
	for range 3 {
		c.pollNodeHealth(ctx)
	}
	if _, err := st.GetOpenIncident(ctx, types.Target{Node: "gpu-1"}, types.ClassECCDBE); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second, node-wide ecc-dbe incident opened: %v", err)
	}
	inc, err := st.GetIncident(ctx, "agent-dbe")
	if err != nil {
		t.Fatal(err)
	}
	if inc.SignalSeen != 2 {
		t.Fatalf("signal seen %d, want the node's report counted once", inc.SignalSeen)
	}
	trail, err := st.AuditTrail(ctx, "agent-dbe")
	if err != nil {
		t.Fatal(err)
	}
	var joined int
	for _, e := range trail {
		if e.Action == "node-health-signal" {
			joined++
			if e.Params["rule"] != "eks-node-monitoring" || e.Params["reason"] != "NvidiaDoubleBitError" {
				t.Fatalf("evidence %+v", e.Params)
			}
		}
	}
	if joined != 1 {
		t.Fatalf("node report recorded %d times on the device incident, want once: %+v", joined, trail)
	}
}

func TestJoinedFaultRunsOneCloudLadder(t *testing.T) {
	for _, mode := range []recoverymode.Mode{recoverymode.Approval, recoverymode.Automatic} {
		for _, order := range []string{"node-first", "agent-first"} {
			t.Run(string(mode)+"/"+order, func(t *testing.T) {
				ctx := t.Context()
				p := newSuccessorPlatform()
				c, st, _ := successorFixture(t, mode, p, recoverymode.RemoveFirst)
				node := func() {
					c.observeHealth(ctx, "kubernetes", []nodehealth.NodeHealth{{Node: "n1", Observations: []nodehealth.Observation{{State: nodehealth.StateFault, Class: types.ClassFellOffBus, Profile: "nvidia", Rule: "eks-node-monitoring", Key: "condition:AcceleratedHardwareReady", Reads: 1}}}})
				}
				device := func(uuid string) {
					t.Helper()
					if err := c.IngestSignal(ctx, signal(types.ClassFellOffBus, "n1", uuid)); err != nil {
						t.Fatal(err)
					}
				}
				if order == "node-first" {
					node()
					device("GPU-1")
					device("GPU-2")
				} else {
					device("GPU-1")
					node()
				}
				incs, err := st.ListIncidents(ctx, store.IncidentFilter{})
				if err != nil || len(incs) != 1 {
					t.Fatalf("incidents=%v err=%v", incs, err)
				}
				id := incs[0].ID
				approvals := 0
				waitSettled(t, c, st, id, func(inc *types.Incident) bool {
					if inc.State == types.StateAwaitingApproval {
						if mode != recoverymode.Approval {
							t.Fatalf("unexpected approval: %s", dump(trail(t, st, id)))
						}
						if err := c.DecideApproval(ctx, id, "alice", "test", types.ApprovalApproved, inc.ApprovalEpoch, "", ""); err != nil {
							t.Fatal(err)
						}
						approvals++
					}
					return inc.State == types.StateVerifying
				})
				p.mu.Lock()
				defer p.mu.Unlock()
				if len(p.cordoned["n1"]) != 1 || p.cordoned["n1"][0] != id || len(p.drained) != 1 || len(p.plain) != 1 || (mode == recoverymode.Approval && approvals == 0) {
					t.Fatalf("cordon owners=%v drains=%v replacements=%v approvals=%d", p.cordoned, p.drained, p.plain, approvals)
				}
			})
		}
	}
}
