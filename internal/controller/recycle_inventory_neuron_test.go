// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// neuronObservationReport is what the agent sends for an AWS Neuron node:
// its devices, observed, with no runtime attested.
func neuronObservationReport(node string, id cloud.NodeIdentity, at time.Time) *types.AgentAcceleratorReport {
	return &types.AgentAcceleratorReport{Node: node, NodeUID: id.NodeUID, BootID: id.BootID, Vendor: types.AcceleratorVendorAWS,
		ObservedAt: at, TopologySafety: types.AcceleratorTopologyUnknown, Capabilities: []types.AgentAcceleratorCapability{},
		Readiness: types.AcceleratorReadinessObserved, ReadinessReasons: []string{"device presence observed; runtime not attested"},
		Devices: []types.AgentAcceleratorDevice{{ID: "neuron0", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyNeuron}}}
}

// An incident opened by a fault of the AWS Neuron sysfs read names the
// vendor aws. After a verified recycle it resolves on the agent's report
// from the new boot, which observes the devices and attests no runtime, and
// the resolution says so. A replacement holds as it does for every vendor.
func TestNeuronIncidentResolvesAfterAVerifiedRecycle(t *testing.T) {
	for _, name := range []string{"recycle", "recycle-old-boot", "recycle-no-device", "replace"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p := &autoCloudPlatform{}
			first, st := stackTestController(t, p)
			rc := *first.runtime.Load()
			rc.Health = HealthPolicy{AWSNeuronSysfs: true}
			first.runtime.Store(&rc)
			sig, ok := first.agentEventSignal(ctx, types.AgentEvent{Node: "node-a", GPUIndex: -1, Timestamp: time.Now(),
				Fault: &types.FaultSignal{Vendor: "aws", Source: detect.NeuronSysfsSource, Code: "hbm-ecc-uncorrectable",
					Attributes: map[string]string{"device": "neuron0"}}})
			if !ok || sig.Vendor() != types.AcceleratorVendorAWS || sig.Target.IsGPU() {
				t.Fatalf("signal %+v ok %v, want the node's, vendor aws", sig, ok)
			}

			ref, err := p.CloudNode(ctx, "node-a")
			if err != nil {
				t.Fatal(err)
			}
			old := ref.Identity
			old.BootID = "previous"
			now := time.Now().UTC()
			inc := &types.Incident{ID: "inc-neuron", Target: sig.Target, Class: sig.Class, Vendor: sig.Vendor(), State: types.StateExecuting,
				CloudIdentity: &old, OpenedAt: now.Add(-time.Hour), StateChangedAt: now.Add(-2 * time.Minute)}
			inc.UpdatedAt = inc.StateChangedAt
			if err := st.CreateIncident(ctx, inc); err != nil {
				t.Fatal(err)
			}
			if err := first.repinAfterRecycle(ctx, inc, cloud.RecycleResult{Before: old, After: ref.Identity, CompletedAt: now.Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			inc.State = types.StateVerifying
			if err := st.UpdateIncident(ctx, inc); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendAudit(ctx, &types.AuditEntry{IncidentID: inc.ID, Time: now, Result: auditStepOutcomeResult("platform.recycle_node", false, true)}); err != nil {
				t.Fatal(err)
			}
			if name == "replace" {
				if err := st.AppendAudit(ctx, &types.AuditEntry{IncidentID: inc.ID, Time: now, Result: auditStepStartResult("platform.replace_node")}); err != nil {
					t.Fatal(err)
				}
			}
			report := neuronObservationReport("node-a", ref.Identity, now)
			switch name {
			case "recycle-old-boot":
				report.BootID = old.BootID
			case "recycle-no-device":
				report.Devices = []types.AgentAcceleratorDevice{}
			}
			if err := st.UpsertAcceleratorReport(ctx, report); err != nil {
				t.Fatal(err)
			}
			if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", AgentLastSeen: now}); err != nil {
				t.Fatal(err)
			}

			c := New(st, st, modeEngine(t, recoverymode.Approval, nil), safety.NewGate(safety.Limits{}), nil, p, nil, &recordingNotifier{}, first.log)
			c.SetTimings(time.Millisecond, 0)
			reloaded, err := st.GetIncident(ctx, inc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.advanceVerifying(ctx, reloaded); err != nil {
				t.Fatal(err)
			}
			got, err := st.GetIncident(ctx, inc.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := types.StateVerifying
			if name == "recycle" {
				want = types.StateResolved
			}
			if got.State != want {
				t.Fatalf("got %s, want %s", got.State, want)
			}
			if want != types.StateResolved {
				return
			}
			trail, err := st.AuditTrail(ctx, inc.ID)
			if err != nil || len(trail) == 0 || !strings.Contains(trail[len(trail)-1].Result, "device presence observed; runtime not attested") {
				t.Fatalf("resolution does not name its depth: %v, %v", trail, err)
			}
			if types.VerifiedInfrastructureTime(trail) == nil {
				t.Fatal("verified recycle not recorded as infrastructure evidence")
			}
		})
	}
}
