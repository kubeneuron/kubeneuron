// SPDX-License-Identifier: Apache-2.0

package nodehealth

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// RuleDRADeviceTaint reads the taints Dynamic Resource Allocation drivers
// set on the devices they publish in resource.k8s.io/v1 ResourceSlices
// (DRADeviceTaints, beta in Kubernetes 1.36). The keys and values are those
// of the drivers' documentation, read 2026-11-18:
//
//   - NVIDIA, driver gpu.nvidia.com (kubernetes-sigs/dra-driver-nvidia-gpu
//     v0.4.0 and later, with its NVMLDeviceHealthCheck gate):
//     site/content/docs/guides/gpu-health-checking.md.
//   - AWS Neuron, driver neuron.aws.com (Neuron DRA driver 1.3.0 and later):
//     https://awsdocs-neuron.readthedocs-hosted.com/en/latest/deploy/eks/neuron-device-taints-guide.html
const RuleDRADeviceTaint = "dra-device-taint"

// draConfirmAge is how old a taint's timeAdded must be to count on the
// first read; a younger one needs confirmReads reads.
const draConfirmAge = 2 * time.Minute

// draDriverProfiles names the profile of each driver whose taints are read.
// A slice of any other driver is not read.
var draDriverProfiles = map[string]string{
	"gpu.nvidia.com": "nvidia",
	"neuron.aws.com": "aws-neuron",
	"gpu.amd.com":    "amd-instinct",
}

// Reasons of a device taint that is read and not acted on.
const (
	DRAReasonNonFatalXID       = "non-fatal-xid"
	DRAReasonPeerTainted       = "peer-tainted"
	DRAReasonInterconnect      = "interconnect"
	DRAReasonEffectNone        = "effect-none"
	DRAReasonUndocumentedTaint = "undocumented-taint"
)

// Device states of the DRA read that are not a signal: the device is
// published and says nothing about its health.
const (
	// DRANoHealthTaint: the device carries no taint. That is not health:
	// the driver may not publish health taints, or the cluster may drop them.
	DRANoHealthTaint = "no-health-taint"
	// DRAUnmonitored: the driver says it does not watch the device's health.
	DRAUnmonitored = "unmonitored"
)

// neuronFaults are the values of neuron.aws.com/device-unhealthy that blame
// the device itself; neuronInterconnect those that name a link or fabric
// beyond it. Any other value is unknown.
var (
	neuronFaults = map[string]bool{
		"hbm-uncorrectable-error":          true,
		"sram-uncorrectable-error":         true,
		"device-unreachable":               true,
		"link-host-error":                  true,
		"firmware-service-processor-error": true,
	}
	neuronInterconnect = map[string]bool{
		"link-intraserver-error": true,
		"link-interserver-error": true,
		"link-efa-error":         true,
		"link-switch-error":      true,
		"fabric-error":           true,
	}
	gpuUUID = regexp.MustCompile(`^(GPU|MIG)-[0-9a-fA-F-]{36}$`)
)

// DRADeviceKey is the signal key of one device: the same device on
// consecutive reads has the same key, and an unknown read of the device
// holds its pending fault.
func DRADeviceKey(d types.DRADevice) string {
	return "dra:" + d.Driver + "/" + d.Pool + "/" + d.Device
}

// DRADeviceUUID is the device's uuid in the agent's form, or empty when the
// driver publishes none in that form.
func DRADeviceUUID(d types.DRADevice) string {
	if gpuUUID.MatchString(d.UUID) {
		return d.UUID
	}
	return ""
}

// DRAProfile is the profile of a driver, or empty for a driver not read.
func DRAProfile(driver string) string { return draDriverProfiles[driver] }

// EvaluateDRA reads the device taints of one node. A device without a taint,
// or one its driver does not monitor, gives no observation: it is neither
// healthy nor unknown, holds nothing and clears no other source's fault.
// devices is the state of every device read, by device key, for counting:
// fault, unknown, ignored, unmonitored or no-health-taint.
func EvaluateDRA(snap types.NodeHealthSnapshot) (h NodeHealth, devices map[string]string) {
	h = NodeHealth{Node: snap.Node, State: StateHealthy}
	devices = map[string]string{}
	profiles := map[string]bool{}
	for _, d := range snap.DRADevices {
		profile := DRAProfile(d.Driver)
		if profile == "" {
			continue
		}
		var obs []Observation
		unmonitored := false
		for _, t := range d.Taints {
			o, ok := evaluateDRATaint(snap.ReadAt, d, t)
			switch {
			case o.State == "" && ok:
				unmonitored = true
			case ok:
				o.Profile = profile
				obs = append(obs, o)
			}
		}
		if len(obs) == 0 {
			devices[DRADeviceKey(d)] = DRANoHealthTaint
			if unmonitored {
				devices[DRADeviceKey(d)] = DRAUnmonitored
			}
			continue
		}
		state := StateIgnored
		for _, o := range obs {
			if o.State == StateFault || o.State == StateUnknown && state != StateFault {
				state = o.State
			}
		}
		devices[DRADeviceKey(d)] = string(state)
		profiles[profile] = true
		h.Observations = append(h.Observations, obs...)
	}
	for _, p := range Profiles {
		if profiles[p.Name] {
			h.Profiles = append(h.Profiles, p.Name)
		}
	}
	for _, o := range h.Observations {
		switch {
		case o.State == StateFault:
			h.State = StateFault
		case o.State == StateUnknown && h.State != StateFault:
			h.State = StateUnknown
		}
	}
	return h, devices
}

// evaluateDRATaint reads one taint of a device. ok false is a taint that is
// not about health at all and is skipped; an observation with no state and
// ok true says the driver does not monitor the device. The taint's value is
// kept only when the table lists it, or as an XID number.
func evaluateDRATaint(readAt time.Time, d types.DRADevice, t types.DRATaint) (Observation, bool) {
	o := Observation{Rule: RuleDRADeviceTaint, Key: DRADeviceKey(d), Device: d.Device, DeviceUUID: DRADeviceUUID(d),
		Since: t.TimeAdded, Reads: confirmReads}
	if !t.TimeAdded.IsZero() && readAt.Sub(t.TimeAdded) >= draConfirmAge {
		o.Reads = 1
	}
	acting := t.Effect == "NoSchedule" || t.Effect == "NoExecute"
	ignored := func(reason string) (Observation, bool) {
		o.State, o.Reason = StateIgnored, reason
		o.Why = "the device taint is read and not acted on: " + reason
		return o, true
	}
	switch {
	case d.Driver == "gpu.nvidia.com" && t.Key == "gpu.nvidia.com/unmonitored":
		return Observation{}, true
	case d.Driver == "gpu.nvidia.com" && t.Key == "gpu.nvidia.com/xid":
		code, err := strconv.Atoi(t.Value)
		if err != nil || code <= 0 || code > 999 {
			o.State, o.Why = StateUnknown, "the XID taint carries no XID number"
			return o, true
		}
		o.Reason = "xid-" + strconv.Itoa(code)
		if !acting {
			return ignored(DRAReasonNonFatalXID)
		}
		o.Key += "=xid/" + strconv.Itoa(code)
		if info, known := detect.ClassifyXID(code); known && info.Class != "" {
			o.State, o.Class = StateFault, info.Class
			return o, true
		}
		o.State, o.Class = StateFault, types.ClassAcceleratorFault
		return o, true
	case d.Driver == "gpu.nvidia.com" && t.Key == "gpu.nvidia.com/gpu-lost":
		if !acting {
			return ignored(DRAReasonEffectNone)
		}
		o.State, o.Class, o.Reason = StateFault, types.ClassAcceleratorFault, "gpu-lost"
		o.Key += "=gpu-lost"
		return o, true
	case d.Driver == "neuron.aws.com" && t.Key == "neuron.aws.com/peer-tainted":
		return ignored(DRAReasonPeerTainted)
	case d.Driver == "neuron.aws.com" && t.Key == "neuron.aws.com/device-unhealthy":
		switch {
		case neuronInterconnect[t.Value]:
			o.Reason = t.Value
			return ignored(DRAReasonInterconnect)
		case !neuronFaults[t.Value]:
			o.State, o.Why = StateUnknown, fmt.Sprintf("the value of neuron.aws.com/device-unhealthy is not in profile table %s", Version)
			return o, true
		case !acting:
			o.Reason = t.Value
			return ignored(DRAReasonEffectNone)
		}
		o.State, o.Class, o.Reason = StateFault, types.ClassAcceleratorFault, t.Value
		o.Key += "=" + t.Value
		return o, true
	}
	return ignored(DRAReasonUndocumentedTaint)
}
