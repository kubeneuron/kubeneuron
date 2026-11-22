// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"context"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const recycleDriverProbeEvidence = "accelerator inventory and driver probe observed after verified recycle; DCGM not present, device health not checked"
const recycleInventoryEvidence = "fresh accelerator inventory and driver health observed after verified recycle"
const recycleObservedEvidence = "fresh accelerator inventory observed after verified recycle: device presence observed; runtime not attested"

// recycleInventoryMatches checks the agent's observation against the pin
// persisted after a verified reboot. The API supplies the report's node UID.
func recycleInventoryMatches(report *types.AgentAcceleratorReport, pin cloud.NodeIdentity, gpu string, completed, now time.Time) bool {
	if report == nil || pin.Validate() != nil || completed.IsZero() ||
		report.Node != pin.Node || report.NodeUID != pin.NodeUID || report.BootID != pin.BootID ||
		!report.ObservedAt.After(completed) || report.ObservedAt.After(now) ||
		now.Sub(report.ObservedAt) > verifyEvidenceMaxAge {
		return false
	}
	return inventoryMatches(report, gpu, completed, now)
}

// inventoryMatches checks that a report lists the device, or with gpu empty
// any physical device, at a readiness that vouches for it: ready, NVIDIA
// degraded with a driver probe since, or observed for a vendor this build
// cannot attest.
func inventoryMatches(report *types.AgentAcceleratorReport, gpu string, completed, now time.Time) bool {
	if report.Readiness == types.AcceleratorReadinessObserved {
		// An observation-only report of a vendor this build cannot attest:
		// the new boot's devices are present, which is all it says. It never
		// stands in for a device-scoped incident's device.
		if gpu != "" || vendorRuntimeAttested(report.Vendor) {
			return false
		}
		for _, device := range report.Devices {
			if device.ID != "" && device.Kind == types.AcceleratorDevicePhysical {
				return true
			}
		}
		return false
	}
	for _, device := range report.Devices {
		if device.ID != "" && (gpu == "" || gpu == device.ID) &&
			device.Kind == types.AcceleratorDevicePhysical && device.Family == types.AcceleratorFamilyGPU {
			if report.Readiness == types.AcceleratorReadinessReady {
				return true
			}
			at := device.DriverHealthyAt
			if report.Vendor == types.AcceleratorVendorNVIDIA && report.Readiness == types.AcceleratorReadinessDegraded &&
				at != nil && at.After(completed) && !at.After(now) && now.Sub(*at) <= verifyEvidenceMaxAge {
				return true
			}
		}
	}
	return false
}

func (c *Controller) verifyRecycledInventory(ctx context.Context, inc *types.Incident) (bool, string) {
	pin := inc.CloudIdentity
	if pin == nil || pin.Validate() != nil || pin.Node != inc.Target.Node {
		return false, "cloud recovery awaits a verified target identity"
	}
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil {
		return false, "cloud recovery history is unavailable"
	}
	var completed time.Time
	for _, entry := range trail {
		if entry.DryRun {
			continue
		}
		const replace = "platform.replace_node"
		switch entry.Result {
		case auditStepStartResult(replace), auditExecutingResult(replace), auditStepOutcomeResult(replace, false, true), auditStepOutcomeResult(replace, false, false):
			return false, "replacement awaits correlated accelerator inventory"
		case auditStepStartResult("platform.recycle_node"):
			completed = time.Time{}
		}
		if entry.Action == "cloud-pin-updated" && entry.Actor == "system" && entry.Params["instance_id"] == pin.InstanceID &&
			entry.Params["boot_id"] == pin.BootID && entry.Params["previous_boot_id"] != "" && entry.Params["previous_boot_id"] != pin.BootID {
			completed = entry.Time
		}
	}
	if completed.IsZero() {
		return false, "cloud recovery awaits a verified recycle"
	}
	observer, ok := c.platform.(platform.CloudNodeObserver)
	if !ok {
		return false, "current cloud identity is unavailable"
	}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ref, err := observer.CloudNode(readCtx, pin.Node)
	if err != nil || pin.Match(ref.Identity) != nil {
		return false, "current cloud identity could not be confirmed"
	}
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return false, "accelerator report store is unavailable"
	}
	vendor := inc.Vendor
	if !vendor.Valid() {
		vendor = types.AcceleratorVendorNVIDIA
	}
	report, err := reports.GetAcceleratorReport(ctx, pin.Node, vendor)
	if err != nil || !recycleInventoryMatches(report, *pin, inc.Target.GPUUUID, completed, time.Now()) {
		return false, "awaiting fresh accelerator inventory and driver health from the verified boot"
	}
	if report.Readiness == types.AcceleratorReadinessDegraded {
		return true, recycleDriverProbeEvidence
	}
	if report.Readiness == types.AcceleratorReadinessObserved {
		return true, recycleObservedEvidence
	}
	return true, recycleInventoryEvidence
}
