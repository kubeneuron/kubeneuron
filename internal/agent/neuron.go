// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/kubeneuron/kubeneuron/internal/agent/neuronsysfs"
	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// neuronStateFile is the file in the agent's state directory that keeps the
// AWS Neuron counters' baselines.
const neuronStateFile = "neuron-sysfs.json"

// neuronSysfs reads the AWS Neuron devices' health from the driver's sysfs
// tree on every registration tick. Reading needs no privilege and no opt-in:
// whether a fault opens an incident is the controller's switch. A node
// without the driver's tree has no such source and sends nothing.
//
// EXPERIMENTAL, NOT VALIDATED AGAINST A LIVE AWS TRAINIUM OR INFERENTIA NODE.
type neuronSysfs struct {
	root    string
	tracker *neuronsysfs.Tracker
	// listed is the components each device's last read listed; a component
	// is a fault once two reads in a row list it, since one read can catch
	// a state the driver is still settling.
	listed map[int]map[string]bool
	// sent is the components already sent, until a read no longer lists
	// them.
	sent map[int]map[string]bool
	// reason is why the tree could not be read last, empty when it was.
	reason string
}

// newNeuronSysfs reads the baselines kept beside the action journal for this
// boot. A baseline file that cannot be read is said once and starts over.
func (a *Agent) newNeuronSysfs(root, stateDir string) *neuronSysfs {
	if root == "" {
		root = neuronsysfs.DefaultRoot
	}
	tracker, err := neuronsysfs.NewTracker(filepath.Join(stateDir, neuronStateFile), a.currentBootID())
	if err != nil {
		a.log.Warn("AWS Neuron counter baselines unreadable; starting without them", "err", err)
	}
	return &neuronSysfs{root: root, tracker: tracker, listed: map[int]map[string]bool{}, sent: map[int]map[string]bool{}}
}

// neuronDevice is how a device is named to the controller: its index in the
// driver's tree.
func neuronDevice(index int) string { return "neuron" + strconv.Itoa(index) }

// readNeuron reads the tree once, sends its faults, and reports the devices.
func (a *Agent) readNeuron(ctx context.Context) {
	n := a.neuron
	if n == nil {
		return
	}
	devices, present, err := neuronsysfs.Read(n.root)
	if !present {
		return
	}
	if err != nil {
		reason := string(neuronsysfs.ReasonReadError)
		var re *neuronsysfs.ReadError
		if errors.As(err, &re) {
			reason = string(re.Reason)
		}
		if reason != n.reason {
			a.log.Warn("AWS Neuron sysfs unreadable; no faults are read from it", "reason", reason)
		}
		n.setReason(reason)
		a.postNeuronReport(ctx, nil, reason)
		return
	}
	n.setReason("")

	delivered := true
	for _, f := range n.tracker.Observe(devices) {
		if f.Kind != neuronsysfs.KindFault {
			a.log.Info("AWS Neuron device observation, not a fault", "device", neuronDevice(f.Device), "class", f.Class,
				"present_at_start", f.PresentAtStart)
			continue
		}
		ev := a.neuronEvent(f.Device, string(f.Class), nil)
		if !a.handleDetection(ctx, ev, detect.NeuronSysfsSource) {
			delivered = false
		}
	}
	// A fault not delivered nor spooled is found again by the next read.
	if delivered {
		if err := n.tracker.Commit(); err != nil {
			a.log.Warn("AWS Neuron counter baselines not saved", "err", err)
		}
	}
	for _, d := range devices {
		if d.Err != "" || !d.HasComponents {
			continue
		}
		now := map[string]bool{}
		for _, c := range d.Components {
			now[c] = true
		}
		sent := map[string]bool{}
		for c := range now {
			switch {
			case n.sent[d.Index][c]:
				sent[c] = true
			case n.listed[d.Index][c]:
				ev := a.neuronEvent(d.Index, "unhealthy-component", map[string]string{"component": c})
				sent[c] = a.handleDetection(ctx, ev, detect.NeuronSysfsSource)
			}
		}
		n.listed[d.Index], n.sent[d.Index] = now, sent
	}
	a.postNeuronReport(ctx, devices, "")
}

// setReason records whether the tree could be read, for the source report.
func (n *neuronSysfs) setReason(reason string) {
	if n.reason != "" && n.reason != reason {
		metrics.SetSignalSourceUnavailable(detect.NeuronSysfsSource, types.ClassAcceleratorFault, n.reason, 0)
	}
	n.reason = reason
	unavailable := 0.0
	if reason != "" {
		unavailable = 1
	}
	metrics.SetSignalSourceUnavailable(detect.NeuronSysfsSource, types.ClassAcceleratorFault, reason, unavailable)
}

// neuronEvent is one fault of one device, scoped to the node: the device's
// index is not a stable identity, so it is evidence, not a target.
func (a *Agent) neuronEvent(device int, code string, attrs map[string]string) types.AgentEvent {
	attributes := map[string]string{"device": neuronDevice(device)}
	for k, v := range attrs {
		attributes[k] = v
	}
	return types.AgentEvent{
		Node:      a.cfg.NodeName,
		GPUIndex:  -1,
		Fault:     &types.FaultSignal{Vendor: string(types.AcceleratorVendorAWS), Source: detect.NeuronSysfsSource, Code: code, Attributes: attributes},
		Timestamp: a.now().UTC(),
	}
}

// postNeuronReport sends the node's AWS Neuron devices as an observation-only
// accelerator report of vendor aws, so the controller knows the node's
// vendor. It never declares a capability and is never ready.
func (a *Agent) postNeuronReport(ctx context.Context, devices []neuronsysfs.Device, reason string) {
	report := types.AgentAcceleratorReport{
		Node:             a.cfg.NodeName,
		BootID:           a.currentBootID(),
		Vendor:           types.AcceleratorVendorAWS,
		ObservedAt:       a.now().UTC(),
		Devices:          []types.AgentAcceleratorDevice{},
		TopologySafety:   types.AcceleratorTopologyUnknown,
		Capabilities:     []types.AgentAcceleratorCapability{},
		Readiness:        types.AcceleratorReadinessDegraded,
		ReadinessReasons: []string{"observation only: AWS Neuron devices are read from the driver's sysfs"},
	}
	if reason != "" {
		report.Readiness = types.AcceleratorReadinessNotReady
		report.ReadinessReasons = append(report.ReadinessReasons, "AWS Neuron sysfs unreadable: "+reason)
	}
	for _, d := range devices {
		report.Devices = append(report.Devices, types.AgentAcceleratorDevice{
			ID: neuronDevice(d.Index), Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyNeuron})
		if d.Err != "" {
			report.ReadinessReasons = append(report.ReadinessReasons, fmt.Sprintf("%s unreadable: %s", neuronDevice(d.Index), d.Err))
		}
	}
	if err := report.Validate(); err != nil {
		a.log.Warn("AWS Neuron accelerator report invalid; not sent", "err", err)
		return
	}
	if err := a.postExpectStatus(ctx, types.AgentAcceleratorReportPath, report, http.StatusNoContent); err != nil {
		a.log.Warn("AWS Neuron accelerator report post failed; will retry", "err", err)
	}
}
