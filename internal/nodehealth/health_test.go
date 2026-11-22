// SPDX-License-Identifier: Apache-2.0

package nodehealth

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var readAt = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func snapshot(resource string, conds ...types.NodeHealthCondition) types.NodeHealthSnapshot {
	return types.NodeHealthSnapshot{
		Node: "n1", ReadAt: readAt, Conditions: conds,
		Devices: []types.DeviceCount{{Resource: resource, Capacity: 8, Allocatable: 8}},
	}
}

func cond(kind, status, reason string) types.NodeHealthCondition {
	return types.NodeHealthCondition{Type: kind, Status: status, Reason: reason,
		LastHeartbeatTime: readAt.Add(-time.Minute), LastTransitionTime: readAt.Add(-time.Hour)}
}

func find(t *testing.T, h NodeHealth, key string) Observation {
	t.Helper()
	for _, o := range h.Observations {
		if o.Key == key {
			return o
		}
	}
	t.Fatalf("no observation %q in %+v", key, h.Observations)
	return Observation{}
}

func TestConditionsMapToClasses(t *testing.T) {
	cases := []struct {
		name, resource string
		cond           types.NodeHealthCondition
		profile        string
		class          types.ProblemClass
	}{
		{"neuron npd", "aws.amazon.com/neuron", cond("NeuronHealth", "True", "NeuronHasError_DMA_ERROR"), "aws-neuron", types.ClassAcceleratorFault},
		{"neuron eks", "aws.amazon.com/neuron", cond("AcceleratedHardwareReady", "False", "NeuronHBMUncorrectableError"), "aws-neuron", types.ClassAcceleratorFault},
		{"nvidia eks dbe", "nvidia.com/gpu", cond("AcceleratedHardwareReady", "False", "NvidiaDoubleBitError"), "nvidia", types.ClassECCDBE},
		{"nvidia eks nvlink", "nvidia.com/gpu", cond("AcceleratedHardwareReady", "False", "NvidiaNVLinkError"), "nvidia", types.ClassNVLink},
		{"nvidia eks xid 79", "nvidia.com/gpu", cond("AcceleratedHardwareReady", "False", "NvidiaXID79Error"), "nvidia", types.ClassFellOffBus},
		{"nvidia eks xid 48", "nvidia.com/gpu", cond("AcceleratedHardwareReady", "False", "NvidiaXID48Error"), "nvidia", types.ClassECCDBE},
		{"nvidia aks missing", "nvidia.com/gpu", cond("GPUMissing", "True", ""), "nvidia", types.ClassGPULost},
		{"nvidia aks nvlink", "nvidia.com/gpu", cond("NVLinkDown", "True", ""), "nvidia", types.ClassNVLink},
		{"amd npd", "amd.com/gpu", cond("AMDGPUProblem", "True", "AMDGPUIsDown"), "amd-instinct", types.ClassAcceleratorFault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Evaluate(snapshot(tc.resource, tc.cond))
			o := find(t, h, "condition:"+tc.cond.Type+"="+tc.cond.Status)
			if o.State != StateFault || o.Class != tc.class || o.Profile != tc.profile || h.State != StateFault {
				t.Fatalf("got %+v (node %s), want fault %s of %s", o, h.State, tc.class, tc.profile)
			}
			if !slices.Contains(Classes(), o.Class) {
				t.Fatalf("Classes() leaves out %s", o.Class)
			}
			if o.Reason != tc.cond.Reason || !o.Since.Equal(tc.cond.LastTransitionTime) || o.Reads != confirmReads {
				t.Fatalf("evidence %+v", o)
			}
		})
	}
}

// Signals read and not acted on are reported as ignored, never as a fault,
// and they are not a signal of the profile: a node with only them is unknown.
func TestIgnoredSignalsAreSeenNotActedOn(t *testing.T) {
	for _, name := range []string{"XIDError", "GPUECCError", "GPUClockThrottling", "UnhealthyNvidiaDevicePlugin", "UnhealthyNvidiaDCGMServices"} {
		t.Run(name, func(t *testing.T) {
			h := Evaluate(snapshot("nvidia.com/gpu", cond(name, "True", "")))
			o := find(t, h, "condition:"+name)
			if o.State != StateIgnored || o.Class != "" || o.Why == "" {
				t.Fatalf("got %+v", o)
			}
			if h.State != StateUnknown {
				t.Fatalf("node %s, want unknown: an ignored signal is no signal of the profile", h.State)
			}
			healthy := Evaluate(snapshot("nvidia.com/gpu", cond(name, "False", ""), cond("GPUMissing", "False", "")))
			if healthy.State != StateHealthy {
				t.Fatalf("got %+v", healthy)
			}
			for _, o := range healthy.Observations {
				if o.State == StateIgnored {
					t.Fatalf("a healthy ignored condition is reported: %+v", o)
				}
			}
		})
	}
}

func TestHealthyConditionIsHealthy(t *testing.T) {
	h := Evaluate(snapshot("aws.amazon.com/neuron", cond("NeuronHealth", "False", ""), cond("AcceleratedHardwareReady", "True", "")))
	if h.State != StateHealthy {
		t.Fatalf("state %s, want healthy: %+v", h.State, h.Observations)
	}
}

// Missing, stale or unknown-status signals are unknown, never healthy.
func TestNoFreshSignalIsUnknown(t *testing.T) {
	stale := cond("NeuronHealth", "False", "")
	stale.LastHeartbeatTime = readAt.Add(-11 * time.Minute)
	noHeartbeat := cond("NeuronHealth", "False", "")
	noHeartbeat.LastHeartbeatTime = time.Time{}
	staleFault := cond("NeuronHealth", "True", "NeuronHasError_NC_UNCORRECTABLE_ERROR")
	staleFault.LastHeartbeatTime = readAt.Add(-time.Hour)
	cases := []struct {
		name string
		snap types.NodeHealthSnapshot
		key  string
		why  string
	}{
		{"missing", snapshot("aws.amazon.com/neuron"), "conditions", "none of the conditions NeuronHealth, AcceleratedHardwareReady"},
		{"stale", snapshot("aws.amazon.com/neuron", stale), "condition:NeuronHealth", "stale: heartbeat 11m0s old"},
		{"stale fault", snapshot("aws.amazon.com/neuron", staleFault), "condition:NeuronHealth", "stale"},
		{"no heartbeat", snapshot("aws.amazon.com/neuron", noHeartbeat), "condition:NeuronHealth", "no heartbeat time"},
		{"status unknown", snapshot("aws.amazon.com/neuron", cond("NeuronHealth", "Unknown", "")), "condition:NeuronHealth", `status "Unknown"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Evaluate(tc.snap)
			o := find(t, h, tc.key)
			if o.State != StateUnknown || o.Class != "" || h.State != StateUnknown || !strings.Contains(o.Why, tc.why) {
				t.Fatalf("got %+v (node %s), want unknown with %q", o, h.State, tc.why)
			}
		})
	}
}

// A reason its source does not document is unknown, not a fault: it may be
// software, and a fault this node owns would let a policy replace it.
func TestUndocumentedReasonIsUnknown(t *testing.T) {
	cases := []struct{ name, resource, kind, status, reason string }{
		{"eks nvidia", "nvidia.com/gpu", "AcceleratedHardwareReady", "False", "NvidiaDriverRestarted"},
		{"eks nvidia unlisted xid", "nvidia.com/gpu", "AcceleratedHardwareReady", "False", "NvidiaXID13Error"},
		{"eks neuron", "aws.amazon.com/neuron", "AcceleratedHardwareReady", "False", "NeuronRuntimeRestarted"},
		{"neuron npd", "aws.amazon.com/neuron", "NeuronHealth", "True", "NeuronHasError_HBM_UNCORRECTABLE_ERROR"},
		{"amd npd", "amd.com/gpu", "AMDGPUProblem", "True", "AMDGPUXgmi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Evaluate(snapshot(tc.resource, cond(tc.kind, tc.status, tc.reason)))
			o := find(t, h, "condition:"+tc.kind)
			want := `reason "` + tc.reason + `" of the condition ` + tc.kind + ` is not in profile table ` + Version
			if o.State != StateUnknown || o.Class != "" || h.State != StateUnknown || o.Why != want {
				t.Fatalf("got %+v (node %s), want unknown: %s", o, h.State, want)
			}
		})
	}
}

// The EKS agent's DCGM reasons are documented and given no repair: read,
// not acted on.
func TestEKSDCGMReasonIsIgnored(t *testing.T) {
	for _, reason := range []string{"DCGMDiagnosticFailure", "DCGMError", "DCGMHealthCode42"} {
		h := Evaluate(snapshot("nvidia.com/gpu", cond("AcceleratedHardwareReady", "False", reason)))
		if o := find(t, h, "condition:AcceleratedHardwareReady"); o.State != StateIgnored || o.Class != "" || o.Why == "" {
			t.Fatalf("%s: got %+v", reason, o)
		}
	}
}

func TestAllocatableBelowCapacityIsHeld(t *testing.T) {
	snap := snapshot("google.com/tpu")
	snap.Devices[0].Allocatable = 6
	h := Evaluate(snap)
	o := find(t, h, "allocatable:google.com/tpu")
	if o.State != StateFault || o.Class != types.ClassAcceleratorUnallocatable || o.Hold != unallocatableHold || o.Reason != "6 of 8 allocatable" {
		t.Fatalf("got %+v", o)
	}
	snap.Devices[0].Allocatable = 8
	if h := Evaluate(snap); h.State != StateHealthy || !reflect.DeepEqual(h.Profiles, []string{"google-tpu"}) {
		t.Fatalf("got %+v", h)
	}
}

func TestNodeWithoutAcceleratorsHasNoProfile(t *testing.T) {
	h := Evaluate(types.NodeHealthSnapshot{Node: "cpu", ReadAt: readAt,
		Conditions: []types.NodeHealthCondition{cond("NeuronHealth", "True", "x")}})
	if len(h.Profiles) != 0 || len(h.Observations) != 0 || h.State != "" {
		t.Fatalf("got %+v", h)
	}
}

func TestOtherRepairersFromTaints(t *testing.T) {
	snap := snapshot("amd.com/gpu")
	snap.Taints = []types.NodeTaint{{Key: "amd-gpu-unhealthy", Effect: "NoSchedule"}, {Key: "karpenter.sh/disrupted", Effect: "NoSchedule"}, {Key: "other"}}
	if got := Evaluate(snap).OtherRepairers; !reflect.DeepEqual(got, []string{"amd-gpu-operator", "karpenter"}) {
		t.Fatalf("repairers %v", got)
	}
}

func TestMultiHostTPUSlice(t *testing.T) {
	snap := snapshot("google.com/tpu")
	snap.Devices[0].Capacity, snap.Devices[0].Allocatable = 4, 4
	snap.Labels = map[string]string{labelTPUTopology: "2x2x4", labelGKENodePool: "v5p-pool"}
	if got := Evaluate(snap).Slice; got != "v5p-pool" {
		t.Fatalf("slice %q", got)
	}
	snap.Labels[labelTPUTopology] = "2x2"
	if got := Evaluate(snap).Slice; got != "" {
		t.Fatalf("a single-host topology is a slice: %q", got)
	}
}

// A failed device can take the node's count to zero: the profile still
// applies, its conditions are read, and the count is unknown, not healthy.
func TestZeroCapacityStillReadsTheProfile(t *testing.T) {
	snap := snapshot("aws.amazon.com/neuron", cond("NeuronHealth", "True", "NeuronHasError_DMA_ERROR"))
	snap.Devices[0].Capacity, snap.Devices[0].Allocatable = 0, 0
	h := Evaluate(snap)
	if !reflect.DeepEqual(h.Profiles, []string{"aws-neuron"}) || h.State != StateFault {
		t.Fatalf("got %+v", h)
	}
	if o := find(t, h, "allocatable:aws.amazon.com/neuron"); o.State != StateUnknown || o.Class != "" {
		t.Fatalf("allocatable at zero: %+v", o)
	}
}
