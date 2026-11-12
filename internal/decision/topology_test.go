// SPDX-License-Identifier: LicenseRef-KubeNeuron

package decision

import (
	"reflect"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/topology"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func refusedTopology() *topology.Classification {
	c := topology.Classify(topology.Snapshot{
		Node: "gpu-a", NodeUID: "uid-a", EvaluatedAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
		Resources: topology.Resources{Source: topology.ResourceSourceNode, Capacity: map[string]int64{"nvidia.com/gpu": 4}},
		Labels:    map[string]string{topology.LabelGPUReplicas: "4"},
		Report:    &topology.Report{ObservedAt: time.Date(2026, 9, 5, 11, 59, 0, 0, time.UTC), TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned, Readiness: types.AcceleratorReadinessReady, PhysicalDevices: 1},
		DRA:       topology.DRAObservation{State: topology.DRADisabled},
	})
	return &c
}

func eligibleTopology() *topology.Classification {
	c := topology.Classify(topology.Snapshot{
		Node: "gpu-a", NodeUID: "uid-a", EvaluatedAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
		Resources: topology.Resources{Source: topology.ResourceSourceNode, Capacity: map[string]int64{"nvidia.com/gpu": 1}},
		Report:    &topology.Report{ObservedAt: time.Date(2026, 9, 5, 11, 59, 0, 0, time.UTC), TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned, Readiness: types.AcceleratorReadinessReady, PhysicalDevices: 1},
		DRA:       topology.DRAObservation{State: topology.DRADisabled},
	})
	return &c
}

// TestEvaluateTopologyOnlyNarrowsDeviceScopedReset pins the contract of the
// Topology field: absent, the evaluation is byte-for-byte what it was; a
// refused classification turns an otherwise Eligible device-scoped reset into
// Blocked/TopologyUnqualified; an eligible classification never rescues a
// decision any other check blocked; and node-scoped or observe-class requests
// are not consulted at all.
func TestEvaluateTopologyOnlyNarrowsDeviceScopedReset(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	refused, eligible := refusedTopology(), eligibleTopology()
	if refused.Eligible() || !eligible.Eligible() {
		t.Fatalf("fixtures: refused=%s eligible=%s", refused.Summary, eligible.Summary)
	}

	baseline := Evaluate(testSnapshot(now))
	if baseline.State != StateEligible {
		t.Fatalf("baseline = %#v", baseline)
	}
	withEligible := testSnapshot(now)
	withEligible.Topology = eligible
	if got := Evaluate(withEligible); !reflect.DeepEqual(got, baseline) {
		t.Fatalf("an eligible classification must leave the decision unchanged:\n got %#v\nwant %#v", got, baseline)
	}

	withRefused := testSnapshot(now)
	withRefused.Topology = refused
	got := Evaluate(withRefused)
	if got.State != StateBlocked || !reflect.DeepEqual(got.ReasonCodes, []ReasonCode{ReasonTopologyUnqualified}) {
		t.Fatalf("refused topology = %#v, want Blocked/TopologyUnqualified", got)
	}
	if !reflect.DeepEqual(got.RequiredActions, []string{"review_device_topology"}) {
		t.Fatalf("required actions = %v", got.RequiredActions)
	}
	if got.Permitted() {
		t.Fatal("a topology-blocked decision must not be permitted")
	}

	// Already blocked for another reason: the classification adds nothing and
	// an eligible one certainly does not rescue it.
	paused := testSnapshot(now)
	paused.GlobalPaused = true
	paused.Topology = eligible
	if got := Evaluate(paused); got.State != StateBlocked || got.ReasonCodes[0] != ReasonEmergencyStopActive {
		t.Fatalf("paused with eligible topology = %#v", got)
	}
	noProfile := testSnapshot(now)
	noProfile.Profile = nil
	noProfile.Topology = eligible
	if got := Evaluate(noProfile); got.State != StateObservedOnly {
		t.Fatalf("no profile with eligible topology = %#v, want ObservedOnly", got)
	}

	// Node-scoped and observe-class requests never consult the classification.
	for name, request := range map[string]Request{
		"observe":             {Class: ActionObserve},
		"diagnostic":          {Class: ActionDiagnostic},
		"node reboot":         {Class: ActionRemediate, AcceleratorAction: types.AcceleratorActionRebootNode, Scope: types.AcceleratorScopeNode},
		"partition reset":     {Class: ActionRemediate, AcceleratorAction: types.AcceleratorActionResetDevice, Scope: types.AcceleratorScopePartition},
		"reset without scope": {Class: ActionRemediate, AcceleratorAction: types.AcceleratorActionResetDevice},
	} {
		plain := testSnapshot(now)
		plain.Request = request
		judged := testSnapshot(now)
		judged.Request = request
		judged.Topology = refused
		a, b := Evaluate(plain), Evaluate(judged)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: refused topology changed a non-device-scoped decision:\n got %#v\nwant %#v", name, b, a)
		}
		for _, code := range b.ReasonCodes {
			if code == ReasonTopologyUnqualified {
				t.Errorf("%s: TopologyUnqualified must only ever appear for a physical-device reset", name)
			}
		}
	}
}

// TestTopologyRefusalIsAStableContract pins the reason code and its order
// position: both are consumed by the API, CLI and audit exports.
func TestTopologyRefusalIsAStableContract(t *testing.T) {
	if ReasonTopologyUnqualified != "TopologyUnqualified" {
		t.Fatalf("reason code = %q", ReasonTopologyUnqualified)
	}
	got := canonicalReasons([]ReasonCode{ReasonTopologyUnqualified, ReasonCapabilityMissing, ReasonTopologyUnqualified})
	if !reflect.DeepEqual(got, []ReasonCode{ReasonCapabilityMissing, ReasonTopologyUnqualified}) {
		t.Fatalf("canonical order = %v", got)
	}
	if summary(StateBlocked, got) != "Blocked: CapabilityMissing, TopologyUnqualified" {
		t.Fatalf("summary = %q", summary(StateBlocked, got))
	}
}
