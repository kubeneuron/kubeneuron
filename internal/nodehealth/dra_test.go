// SPDX-License-Identifier: Apache-2.0

package nodehealth

import (
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var draNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

const testGPUUUID = "GPU-0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d"

func draSnap(driver string, taints ...types.DRATaint) types.NodeHealthSnapshot {
	return types.NodeHealthSnapshot{Node: "n1", ReadAt: draNow, DRADevices: []types.DRADevice{
		{Driver: driver, Pool: "n1", Device: "gpu-0", UUID: testGPUUUID, Taints: taints}}}
}

func taint(key, value, effect string, ago time.Duration) types.DRATaint {
	return types.DRATaint{Key: key, Value: value, Effect: effect, TimeAdded: draNow.Add(-ago)}
}

func TestEvaluateDRA(t *testing.T) {
	cases := []struct {
		name   string
		snap   types.NodeHealthSnapshot
		state  State
		class  types.ProblemClass
		reason string
		device string
		reads  int
	}{
		{name: "fatal xid", snap: draSnap("gpu.nvidia.com", taint("gpu.nvidia.com/xid", "79", "NoSchedule", 10*time.Second)),
			state: StateFault, class: types.ClassFellOffBus, reason: "xid-79", device: "fault", reads: 2},
		{name: "fatal xid, NoExecute, old taint", snap: draSnap("gpu.nvidia.com", taint("gpu.nvidia.com/xid", "79", "NoExecute", 5*time.Minute)),
			state: StateFault, class: types.ClassFellOffBus, device: "fault", reads: 1},
		{name: "non-fatal xid", snap: draSnap("gpu.nvidia.com", taint("gpu.nvidia.com/xid", "13", "None", time.Minute)),
			state: StateIgnored, reason: DRAReasonNonFatalXID, device: "ignored"},
		{name: "xid without a number", snap: draSnap("gpu.nvidia.com", taint("gpu.nvidia.com/xid", "free text", "NoSchedule", time.Minute)),
			state: StateUnknown, device: "unknown"},
		{name: "gpu lost", snap: draSnap("gpu.nvidia.com", taint("gpu.nvidia.com/gpu-lost", "", "NoSchedule", time.Minute)),
			state: StateFault, class: types.ClassAcceleratorFault, reason: "gpu-lost", device: "fault"},
		{name: "unmonitored", snap: draSnap("gpu.nvidia.com", taint("gpu.nvidia.com/unmonitored", "", "None", time.Minute)),
			device: DRAUnmonitored},
		{name: "no taint", snap: draSnap("gpu.nvidia.com"), device: DRANoHealthTaint},
		{name: "nvidia key in another driver's slice", snap: draSnap("neuron.aws.com", taint("gpu.nvidia.com/xid", "79", "NoSchedule", time.Minute)),
			state: StateIgnored, reason: DRAReasonUndocumentedTaint, device: "ignored"},
		{name: "undocumented key", snap: draSnap("gpu.nvidia.com", taint("example.com/broken", "x", "NoExecute", time.Minute)),
			state: StateIgnored, reason: DRAReasonUndocumentedTaint, device: "ignored"},
		{name: "neuron hbm", snap: draSnap("neuron.aws.com", taint("neuron.aws.com/device-unhealthy", "hbm-uncorrectable-error", "NoExecute", time.Minute)),
			state: StateFault, class: types.ClassAcceleratorFault, reason: "hbm-uncorrectable-error", device: "fault"},
		{name: "neuron policy set effect None", snap: draSnap("neuron.aws.com", taint("neuron.aws.com/device-unhealthy", "hbm-uncorrectable-error", "None", time.Minute)),
			state: StateIgnored, reason: DRAReasonEffectNone, device: "ignored"},
		{name: "neuron interconnect", snap: draSnap("neuron.aws.com", taint("neuron.aws.com/device-unhealthy", "link-efa-error", "NoSchedule", time.Minute)),
			state: StateIgnored, reason: DRAReasonInterconnect, device: "ignored"},
		{name: "neuron undocumented value", snap: draSnap("neuron.aws.com", taint("neuron.aws.com/device-unhealthy", "new-fault", "NoExecute", time.Minute)),
			state: StateUnknown, device: "unknown"},
		{name: "neuron firmware api", snap: draSnap("neuron.aws.com", taint("neuron.aws.com/device-unhealthy", "firmware-service-api-error", "NoSchedule", time.Minute)),
			state: StateUnknown, device: "unknown"},
		{name: "neuron peer", snap: draSnap("neuron.aws.com", taint("neuron.aws.com/peer-tainted", "", "None", time.Minute)),
			state: StateIgnored, reason: DRAReasonPeerTainted, device: "ignored"},
		{name: "driver not read", snap: draSnap("accel.example.com", taint("accel.example.com/bad", "", "NoExecute", time.Minute))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, devices := EvaluateDRA(tc.snap)
			key := DRADeviceKey(tc.snap.DRADevices[0])
			if devices[key] != tc.device {
				t.Errorf("device state %q, want %q", devices[key], tc.device)
			}
			if tc.state == "" {
				if len(h.Observations) != 0 || len(h.Profiles) != 0 {
					t.Fatalf("observations %+v profiles %v: a device without a health signal gives none", h.Observations, h.Profiles)
				}
				return
			}
			if len(h.Observations) != 1 {
				t.Fatalf("observations %+v, want one", h.Observations)
			}
			o := h.Observations[0]
			if o.State != tc.state || o.Class != tc.class || o.Rule != RuleDRADeviceTaint {
				t.Errorf("observation %+v, want state %s class %q", o, tc.state, tc.class)
			}
			if tc.reason != "" && o.Reason != tc.reason {
				t.Errorf("reason %q, want %q", o.Reason, tc.reason)
			}
			if tc.reads != 0 && o.Reads != tc.reads {
				t.Errorf("reads %d, want %d", o.Reads, tc.reads)
			}
			if o.DeviceUUID != testGPUUUID || o.Device != "gpu-0" {
				t.Errorf("device %q uuid %q", o.Device, o.DeviceUUID)
			}
		})
	}
}

// The fault key names the device and the taint; the device key is its
// prefix, so an unknown read of the device holds the fault.
func TestDRAFaultKey(t *testing.T) {
	h, _ := EvaluateDRA(draSnap("gpu.nvidia.com", taint("gpu.nvidia.com/xid", "79", "NoSchedule", time.Minute)))
	if got := h.Observations[0].Key; got != "dra:gpu.nvidia.com/n1/gpu-0=xid/79" {
		t.Fatalf("key %q", got)
	}
	if d := (types.DRADevice{UUID: "not a uuid"}); DRADeviceUUID(d) != "" {
		t.Fatal("a uuid not in the agent's form was kept")
	}
}
