// SPDX-License-Identifier: LicenseRef-KubeNeuron

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/accelerator/nvidia"
	"github.com/kubeneuron/kubeneuron/internal/agent/dcgm"
	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/accelerator"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// AdapterConfig enables one registered accelerator adapter on this node.
type AdapterConfig struct {
	// Enabled builds the vendor's registered adapter when the agent starts.
	Enabled bool
	// Options is passed to the adapter factory as accelerator.Config.Options.
	// Keys are defined by each adapter.
	Options map[string]string
}

// adapterTimeout bounds one inventory, capability, health, or detection pass
// of one adapter, so a hung vendor tool cannot stall the agent loop.
const adapterTimeout = 15 * time.Second

// adapterRuntime is one enabled vendor. adapter is nil when the build failed;
// that vendor then fails closed (a not-ready report, no detections) while the
// other vendors keep running.
type adapterRuntime struct {
	vendor   accelerator.Vendor
	adapter  accelerator.Adapter
	buildErr error
	// preflight is true when this vendor reports through the NVIDIA preflight
	// path (reportNVIDIA) instead of the generic observation-only report.
	preflight bool
	// nativeDetections is true when the agent already runs native detection
	// sources that read this vendor's events (NVIDIA: kmsg and gpuhealth read
	// the same driver events as the NVIDIA adapter's stream). The adapter's
	// detections are then not consumed a second time.
	nativeDetections bool
}

func (rt *adapterRuntime) source() string { return "adapter-" + string(rt.vendor) }

// adapterSettings resolves whether vendor is enabled and with which options.
// NVIDIA keeps its dedicated settings: NVIDIAObservation.Enabled enables it and
// its partition topology is passed as the adapter option, with any explicit
// Adapters[nvidia].Options merged on top.
func adapterSettings(cfg Config, vendor accelerator.Vendor) (bool, map[string]string) {
	configured := cfg.Adapters[vendor]
	enabled := configured.Enabled
	options := map[string]string{}
	if vendor == accelerator.VendorNVIDIA {
		enabled = enabled || cfg.NVIDIAObservation.Enabled
		options[nvidia.OptionPartitionTopology] = string(cfg.NVIDIAObservation.PartitionTopology)
	}
	for key, value := range configured.Options {
		options[key] = value
	}
	return enabled, options
}

// buildAdapters builds every registered adapter whose vendor is enabled. A
// build failure is logged and recorded for that vendor only; it never fails
// agent start-up and never disables another vendor.
func (a *Agent) buildAdapters(cfg Config, driver nvml.GPUDriver) {
	for vendor, configured := range cfg.Adapters {
		if configured.Enabled && !accelerator.Registered(vendor) {
			err := fmt.Errorf("no adapter is registered for vendor %q (registered: %v)", vendor, accelerator.Vendors())
			a.log.Error("accelerator adapter unavailable; this vendor fails closed", "vendor", vendor, "err", err)
			a.adapters = append(a.adapters, &adapterRuntime{vendor: vendor, buildErr: err})
		}
	}
	for _, vendor := range accelerator.Vendors() {
		enabled, options := adapterSettings(cfg, vendor)
		if !enabled {
			continue
		}
		buildCfg := accelerator.Config{
			NodeName: cfg.NodeName,
			Options:  options,
			Now:      func() time.Time { return a.now() },
		}
		rt := &adapterRuntime{vendor: vendor}
		if vendor == accelerator.VendorNVIDIA {
			// A configuration declaration cannot make the simulator or another
			// vendor's GPUDriver into NVIDIA evidence. The command only builds
			// *nvml.SMI after finding nvidia-smi, and this concrete check keeps
			// the same boundary for all programmatic callers.
			if _, realSMI := driver.(*nvml.SMI); !realSMI {
				a.log.Warn("NVIDIA observation disabled: no real nvidia-smi runtime evidence")
				continue
			}
			buildCfg.Host = driver
			buildCfg.DriverVersion = cfg.NVIDIAObservation.DriverVersion
			buildCfg.RuntimeVersion = cfg.NVIDIAObservation.RuntimeVersion
			rt.nativeDetections = true
		}
		a.adapters = append(a.adapters, rt)

		adapter, err := accelerator.Build(context.Background(), vendor, buildCfg)
		if err != nil {
			rt.buildErr = err
			a.log.Error("accelerator adapter failed to build; this vendor fails closed", "vendor", vendor, "err", err)
			continue
		}
		rt.adapter = adapter

		// Preflight is NVIDIA-specific and outside the adapter contract. An
		// NVIDIA adapter that lacks it is observation-only, never fatal.
		if vendor == accelerator.VendorNVIDIA {
			if preflight, ok := adapter.(nvidiaPreflighter); ok {
				rt.preflight = true
				a.nvidia = &nvidiaObservation{
					preflight:                preflight,
					runtimeProber:            dcgm.NewWithEndpoint(cfg.NVIDIAObservation.DCGMPath, cfg.NVIDIAObservation.DCGMEndpoint),
					dcgmPath:                 cfg.NVIDIAObservation.DCGMPath,
					driverVersion:            cfg.NVIDIAObservation.DriverVersion,
					runtimeVersion:           cfg.NVIDIAObservation.RuntimeVersion,
					profileDigest:            cfg.NVIDIAObservation.ProfileDigest,
					useControllerProfile:     cfg.NVIDIAObservation.UseControllerProfile,
					useObservedDriverVersion: true,
				}
			} else {
				a.log.Warn("registered NVIDIA adapter provides no preflight; NVIDIA is observation-only")
			}
		}
		if !types.AcceleratorVendor(vendor).Valid() {
			a.log.Warn("accelerator adapter vendor is not in the report protocol; it sends detections but no accelerator report",
				"vendor", vendor)
		}
		a.log.Info("accelerator adapter enabled", "vendor", vendor, "preflight", rt.preflight)
	}
	sort.SliceStable(a.adapters, func(i, j int) bool { return a.adapters[i].vendor < a.adapters[j].vendor })
}

// reportAdapters posts one accelerator report per (node, vendor) for every
// enabled vendor that does not report through the NVIDIA preflight path. Like
// reportNVIDIA it is independent of registration and readiness: a failure is
// logged and retried on the next tick.
func (a *Agent) reportAdapters(ctx context.Context) {
	for _, rt := range a.adapters {
		if rt.preflight || !types.AcceleratorVendor(rt.vendor).Valid() {
			continue
		}
		report, err := a.adapterReport(ctx, rt)
		if err != nil {
			a.log.Warn("accelerator adapter report failed; will retry", "vendor", rt.vendor, "err", err)
			continue
		}
		if err := a.postExpectStatus(ctx, types.AgentAcceleratorReportPath, report, http.StatusNoContent); err != nil {
			a.log.Warn("accelerator adapter report post failed; will retry", "vendor", rt.vendor, "err", err)
		}
	}
}

// adapterReport builds the observation-only report for one vendor from the
// adapter contract: inventory, capabilities, and per-device health. It is
// never ready: readiness "ready" requires the attested runtime evidence that
// only a preflight provides. A physical-device reset declaration is withheld
// because only a preflight can verify the partition topology it requires.
func (a *Agent) adapterReport(ctx context.Context, rt *adapterRuntime) (types.AgentAcceleratorReport, error) {
	if err := ctx.Err(); err != nil {
		return types.AgentAcceleratorReport{}, err
	}
	report := types.AgentAcceleratorReport{
		Node:           a.cfg.NodeName,
		Vendor:         types.AcceleratorVendor(rt.vendor),
		ObservedAt:     a.now().UTC(),
		Devices:        []types.AgentAcceleratorDevice{},
		TopologySafety: types.AcceleratorTopologyUnknown,
		Capabilities:   []types.AgentAcceleratorCapability{},
		Readiness:      types.AcceleratorReadinessDegraded,
	}
	if rt.adapter == nil {
		report.Readiness = types.AcceleratorReadinessNotReady
		report.ReadinessReasons = []string{fmt.Sprintf("%s accelerator adapter is unavailable: %v", rt.vendor, rt.buildErr)}
		return report, validateAdapterReport(report)
	}
	report.ReadinessReasons = []string{fmt.Sprintf("observation only: the %s adapter provides no runtime preflight", rt.vendor)}
	notReady := func(reason string) {
		report.Readiness = types.AcceleratorReadinessNotReady
		report.ReadinessReasons = append(report.ReadinessReasons, reason)
	}

	probeCtx, cancel := context.WithTimeout(ctx, adapterTimeout)
	defer cancel()

	inventory, err := rt.adapter.Inventory(probeCtx)
	switch {
	case err != nil:
		notReady(fmt.Sprintf("%s inventory failed: %v", rt.vendor, err))
		inventory = accelerator.Inventory{}
	case inventory.NodeName != a.cfg.NodeName || inventory.Vendor != rt.vendor:
		notReady(fmt.Sprintf("%s inventory is for node %q vendor %q, not this node", rt.vendor, inventory.NodeName, inventory.Vendor))
		inventory = accelerator.Inventory{}
	default:
		if verr := inventory.Validate(); verr != nil {
			notReady(fmt.Sprintf("%s inventory is invalid: %v", rt.vendor, verr))
			inventory = accelerator.Inventory{}
		}
	}
	if !inventory.ObservedAt.IsZero() {
		report.ObservedAt = inventory.ObservedAt.UTC()
	}
	report.DriverVersion = inventory.DriverVersion
	report.RuntimeVersion = inventory.RuntimeVersion
	devices, err := reportDevices(inventory.Devices)
	if err != nil {
		return types.AgentAcceleratorReport{}, err
	}
	report.Devices = devices

	capabilities, err := rt.adapter.Capabilities(probeCtx)
	if err != nil {
		report.ReadinessReasons = append(report.ReadinessReasons, fmt.Sprintf("%s capabilities unavailable: %v", rt.vendor, err))
		capabilities = nil
	} else if verr := capabilities.Validate(); verr != nil {
		report.ReadinessReasons = append(report.ReadinessReasons, fmt.Sprintf("%s capabilities are invalid: %v", rt.vendor, verr))
		capabilities = nil
	}
	declared, withheld := withholdPhysicalReset(capabilities)
	if withheld {
		report.ReadinessReasons = append(report.ReadinessReasons,
			"physical-device reset withheld: partition topology is not verified by a preflight")
	}
	if report.Capabilities, err = reportCapabilities(declared); err != nil {
		return types.AgentAcceleratorReport{}, err
	}

	for _, device := range inventory.Devices {
		target, err := inventory.TargetFor(device.ID)
		if err != nil {
			notReady(fmt.Sprintf("%s device %s has no valid target: %v", rt.vendor, device.ID, err))
			continue
		}
		health, err := rt.adapter.CheckHealth(probeCtx, target)
		switch {
		case err != nil:
			notReady(fmt.Sprintf("%s health check of %s failed: %v", rt.vendor, device.ID, err))
		case health.Status == accelerator.HealthUnhealthy:
			notReady(fmt.Sprintf("%s device %s is unhealthy", rt.vendor, device.ID))
		case health.Status != accelerator.HealthHealthy:
			report.ReadinessReasons = append(report.ReadinessReasons,
				fmt.Sprintf("%s device %s health is %s", rt.vendor, device.ID, health.Status))
		}
	}
	return report, validateAdapterReport(report)
}

func validateAdapterReport(report types.AgentAcceleratorReport) error {
	if err := report.Validate(); err != nil {
		return fmt.Errorf("invalid %s accelerator report: %w", report.Vendor, err)
	}
	return nil
}

// withholdPhysicalReset drops the physical-device scope from a reset-device
// declaration (and the declaration itself when nothing else remains).
func withholdPhysicalReset(capabilities accelerator.CapabilitySet) (accelerator.CapabilitySet, bool) {
	out := make(accelerator.CapabilitySet, 0, len(capabilities))
	withheld := false
	for _, capability := range capabilities {
		if capability.Action != accelerator.ActionResetDevice {
			out = append(out, capability)
			continue
		}
		scopes := make([]accelerator.TargetScope, 0, len(capability.Scopes))
		for _, scope := range capability.Scopes {
			if scope == accelerator.ScopePhysicalDevice {
				withheld = true
				continue
			}
			scopes = append(scopes, scope)
		}
		if len(scopes) > 0 {
			out = append(out, accelerator.ActionCapability{Action: capability.Action, Scopes: scopes})
		}
	}
	return out, withheld
}

// adapterEvent is one adapter detection converted to the agent event wire
// type, with the metric/log source label of the adapter that produced it.
type adapterEvent struct {
	event  types.AgentEvent
	source string
}

// watchAdapterDetections starts the optional detection stream of every built
// adapter that is not already covered by native sources, and merges them into
// one channel for the Run loop. It returns nil when no stream started, which
// simply never fires in Run's select.
func (a *Agent) watchAdapterDetections(ctx context.Context) <-chan adapterEvent {
	var wg sync.WaitGroup
	started := 0
	merged := make(chan adapterEvent, 64)
	for _, rt := range a.adapters {
		if rt.adapter == nil || rt.nativeDetections {
			continue
		}
		watcher, ok := rt.adapter.(accelerator.DetectionWatcher)
		if !ok {
			continue
		}
		stream, err := watcher.WatchDetections(ctx)
		if err != nil || stream == nil {
			a.log.Error("accelerator adapter detection stream unavailable; polling continues", "vendor", rt.vendor, "err", err)
			continue
		}
		started++
		wg.Add(1)
		go func(rt *adapterRuntime, stream <-chan accelerator.Detection) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case detection, open := <-stream:
					if !open {
						a.log.Error("accelerator adapter detection stream stopped; polling continues", "vendor", rt.vendor)
						return
					}
					event, ok := a.adapterDetectionEvent(rt.vendor, detection)
					if !ok {
						continue
					}
					select {
					case merged <- adapterEvent{event: event, source: rt.source()}:
					case <-ctx.Done():
						return
					}
				}
			}
		}(rt, stream)
	}
	if started == 0 {
		return nil
	}
	go func() {
		wg.Wait()
		close(merged)
	}()
	return merged
}

// pollAdapterDetections runs one bounded Detect pass of every built adapter
// that is not already covered by native sources and hands each detection to
// the shared, deduplicating detection handler.
func (a *Agent) pollAdapterDetections(ctx context.Context) {
	for _, rt := range a.adapters {
		if rt.adapter == nil || rt.nativeDetections || ctx.Err() != nil {
			continue
		}
		detectCtx, cancel := context.WithTimeout(ctx, adapterTimeout)
		detections, err := rt.adapter.Detect(detectCtx)
		cancel()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				a.log.Warn("accelerator adapter detection pass failed; will retry", "vendor", rt.vendor, "err", err)
			}
			continue
		}
		for _, detection := range detections {
			if event, ok := a.adapterDetectionEvent(rt.vendor, detection); ok {
				a.handleDetection(ctx, event, rt.source())
			}
		}
	}
}

// adapterDetectionEvent converts an adapter detection to the vendor-neutral
// fault envelope. A detection for another node or vendor, or with an invalid
// target, is dropped: accepting it would create a target this adapter does not
// own. The raw vendor code is taken from Evidence["code"] when the adapter
// supplies one (see accelerator.DetectionClass), else the normalized class.
func (a *Agent) adapterDetectionEvent(vendor accelerator.Vendor, detection accelerator.Detection) (types.AgentEvent, bool) {
	target := detection.Target
	if target.NodeName != a.cfg.NodeName || target.Vendor != vendor {
		a.log.Warn("accelerator adapter detection dropped: target is not this node and vendor",
			"vendor", vendor, "target_node", target.NodeName, "target_vendor", target.Vendor)
		return types.AgentEvent{}, false
	}
	if err := target.Validate(); err != nil {
		a.log.Warn("accelerator adapter detection dropped: invalid target", "vendor", vendor, "err", err)
		return types.AgentEvent{}, false
	}
	code := strings.TrimSpace(detection.Evidence["code"])
	if code == "" {
		code = string(detection.Class)
	}
	if code == "" {
		a.log.Warn("accelerator adapter detection dropped: no fault code or class", "vendor", vendor)
		return types.AgentEvent{}, false
	}
	attributes := make(map[string]string, len(detection.Evidence)+4)
	for key, value := range detection.Evidence {
		if key != "code" {
			attributes[key] = value
		}
	}
	if detection.Class != "" {
		attributes["class"] = string(detection.Class)
	}
	if detection.Severity != "" {
		attributes["severity"] = string(detection.Severity)
	}
	if detection.ID != "" {
		attributes["detection_id"] = detection.ID
	}
	if target.DeviceID != "" {
		attributes["device_id"] = target.DeviceID
	}
	observedAt := detection.ObservedAt
	if observedAt.IsZero() {
		observedAt = a.now()
	}
	return types.AgentEvent{
		Node:     a.cfg.NodeName,
		GPUIndex: -1,
		GPUUUID:  target.DeviceID,
		Fault: &types.FaultSignal{
			Vendor:     string(vendor),
			Source:     "adapter",
			Code:       code,
			Attributes: attributes,
		},
		Timestamp: observedAt.UTC(),
	}, true
}
