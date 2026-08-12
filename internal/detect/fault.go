package detect

import "github.com/kubeneuron/kubeneuron/pkg/types"

// FaultKey identifies one vendor-native fault code in the neutral-envelope
// catalog. It is the vendor-neutral analogue of an XID number: a (Vendor, Code)
// pair a non-XID source emits through types.FaultSignal.
type FaultKey struct {
	Vendor string
	Code   string
}

// FaultInfo classifies one neutral fault code: which problem class (and
// therefore which playbook) it maps to, and how severe it is. It sits beside
// the XID catalog so a source that has no genuine XID still lands in the same
// ProblemClass vocabulary.
type FaultInfo struct {
	Vendor   string
	Code     string
	Name     string
	Class    types.ProblemClass
	Severity types.Severity
}

// faultTable maps a vendor-native fault code to a problem class. The NVIDIA
// entries are the migration targets for the gpuhealth nvidia-smi fallback,
// which used to synthesize XIDs 48/64: each MUST classify to exactly the class
// its former synthesized XID produced (XID 48 -> ClassECCDBE, XID 64 ->
// ClassRowRemapFailure), so removing the synthesized XID changes the encoding,
// never the outcome.
//
// Beyond those two, the table must keep EVERY class the XID catalog can reach
// reachable through a neutral (vendor, code) row as well — otherwise a
// non-NVIDIA source can only ever describe the handful of conditions NVIDIA
// happened to need first, and the catalog is neutral in form only.
// TestEveryXIDReachableClassIsFaultReachable enforces that, including the
// severity parity below: a class must not become less severe merely because it
// arrived in the neutral encoding.
//
// Rows whose code no shipping source emits yet are deliberate: they are the
// vocabulary a second source is expected to speak, and they are what makes the
// audit test meaningful rather than tautological.
var faultTable = map[FaultKey]FaultInfo{
	{Vendor: "nvidia", Code: "ecc-dbe"}: {
		Vendor: "nvidia", Code: "ecc-dbe", Name: "double-bit-ecc-error",
		Class: types.ClassECCDBE, Severity: types.SeverityCritical,
	},
	{Vendor: "nvidia", Code: "row-remap-failure"}: {
		Vendor: "nvidia", Code: "row-remap-failure", Name: "row-remap-failed",
		Class: types.ClassRowRemapFailure, Severity: types.SeverityCritical,
	},
	{Vendor: "nvidia", Code: "row-remap-recorded"}: {
		Vendor: "nvidia", Code: "row-remap-recorded", Name: "row-remap-recorded",
		Class: types.ClassRowRemapOK, Severity: types.SeverityWarning,
	},
	{Vendor: "nvidia", Code: "ecc-correctable-rate"}: {
		Vendor: "nvidia", Code: "ecc-correctable-rate", Name: "high-single-bit-ecc-rate",
		Class: types.ClassECCSBERate, Severity: types.SeverityWarning,
	},
	{Vendor: "nvidia", Code: "ecc-contained"}: {
		Vendor: "nvidia", Code: "ecc-contained", Name: "contained-ecc-error",
		Class: types.ClassECCContained, Severity: types.SeverityWarning,
	},
	{Vendor: "nvidia", Code: "nvlink-error"}: {
		Vendor: "nvidia", Code: "nvlink-error", Name: "nvlink-error",
		Class: types.ClassNVLink, Severity: types.SeverityCritical,
	},
	{Vendor: "nvidia", Code: "fell-off-bus"}: {
		Vendor: "nvidia", Code: "fell-off-bus", Name: "gpu-fell-off-bus",
		Class: types.ClassFellOffBus, Severity: types.SeverityCritical,
	},
	{Vendor: "nvidia", Code: "gsp-error"}: {
		Vendor: "nvidia", Code: "gsp-error", Name: "gsp-error",
		Class: types.ClassGSPError, Severity: types.SeverityCritical,
	},
	{Vendor: "nvidia", Code: "app-fault"}: {
		Vendor: "nvidia", Code: "app-fault", Name: "application-level-gpu-fault",
		Class: types.ClassXIDApp, Severity: types.SeverityInfo,
	},

	// AMD. These are the codes internal/agent/amdhealth and the kmsg amdgpu
	// families emit. Their CLASSES are shared with NVIDIA on purpose: a neutral
	// catalog that grew an AMD-only class per condition would fork the playbook
	// vocabulary and make "vendor-neutral" a spelling exercise.
	{Vendor: "amd", Code: "ecc-uncorrectable"}: {
		Vendor: "amd", Code: "ecc-uncorrectable", Name: "uncorrectable-ecc-error",
		Class: types.ClassECCDBE, Severity: types.SeverityCritical,
	},
	{Vendor: "amd", Code: "ecc-correctable-rate"}: {
		Vendor: "amd", Code: "ecc-correctable-rate", Name: "high-correctable-ecc-rate",
		Class: types.ClassECCSBERate, Severity: types.SeverityWarning,
	},
	{Vendor: "amd", Code: "page-retirement"}: {
		// AMD's bad-page reservation is the same recovery mechanism NVIDIA calls
		// a row remap, and like XID 63 it is the mechanism WORKING: it warrants a
		// reset when the device is idle, not a drain-now.
		Vendor: "amd", Code: "page-retirement", Name: "page-retirement-recorded",
		Class: types.ClassRowRemapOK, Severity: types.SeverityWarning,
	},
	{Vendor: "amd", Code: "page-retirement-exhausted"}: {
		// The peer of XID 64. A retirement below the budget is the mechanism
		// working (ClassRowRemapOK above); past the budget the device has no
		// spares left to retire into, which is the same standing as an NVIDIA
		// row-remap failure and gets the same class and severity.
		Vendor: "amd", Code: "page-retirement-exhausted", Name: "page-retirement-budget-exhausted",
		Class: types.ClassRowRemapFailure, Severity: types.SeverityCritical,
	},
	{Vendor: "amd", Code: "xgmi-link-error"}: {
		// XGMI is AMD's inter-GPU fabric, the structural peer of NVLink. The
		// class name is NVIDIA's spelling for historical reasons; minting a
		// ClassXGMI would give the identical condition two policies to keep in
		// sync, which is the failure this catalog exists to prevent.
		Vendor: "amd", Code: "xgmi-link-error", Name: "xgmi-link-error",
		Class: types.ClassNVLink, Severity: types.SeverityCritical,
	},
	{Vendor: "amd", Code: "thermal-throttle"}: {
		Vendor: "amd", Code: "thermal-throttle", Name: "thermal-throttle",
		Class: types.ClassThermal, Severity: types.SeverityWarning,
	},
	{Vendor: "amd", Code: "gpu-lost"}: {
		// A device that vanished from the vendor tool's own inventory, which is
		// what ClassGPULost means. It is NOT ClassFellOffBus: that class encodes
		// the specific NVIDIA bus-detach diagnosis (XID 79), and claiming it from
		// a disappearance we did not diagnose would overstate the evidence.
		Vendor: "amd", Code: "gpu-lost", Name: "gpu-lost",
		Class: types.ClassGPULost, Severity: types.SeverityCritical,
	},
	{Vendor: "amd", Code: "ring-timeout"}: {
		// An amdgpu ring timeout is a kernel-confirmed engine hang, not the
		// monitoring gap ClassDriverHang usually arrives from (exporter down),
		// hence critical rather than the alert path's warning default.
		Vendor: "amd", Code: "ring-timeout", Name: "ring-timeout",
		Class: types.ClassDriverHang, Severity: types.SeverityCritical,
	},
	{Vendor: "amd", Code: "pcie-error"}: {
		Vendor: "amd", Code: "pcie-error", Name: "pcie-error",
		Class: types.ClassPCIe, Severity: types.SeverityWarning,
	},
}

// ClassifyFault returns the classification for a vendor-native fault code, and
// whether it is one KubeNeuron acts on. Codes not listed are recorded but open
// no incident, mirroring ClassifyXID.
func ClassifyFault(vendor, code string) (FaultInfo, bool) {
	info, ok := faultTable[FaultKey{Vendor: vendor, Code: code}]
	return info, ok
}

// FaultTable returns the full neutral-fault classification table (for docs, UI,
// and tests).
func FaultTable() []FaultInfo {
	out := make([]FaultInfo, 0, len(faultTable))
	for _, v := range faultTable {
		out = append(out, v)
	}
	return out
}

// SignalFromFault converts a neutral-envelope AgentEvent into a Signal using
// the built-in fault table, or returns ok=false when the fault is not
// actionable (it should still be counted). Catalog-aware callers use
// Catalog.SignalFromAgentEvent, which resolves GPUSignalMapping fault
// overrides before this table.
func SignalFromFault(ev types.AgentEvent) (types.Signal, bool) {
	return (*Catalog)(nil).signalFromFault(ev)
}

// signalFromFault is SignalFromFault with this catalog's fault overrides
// applied first; a nil receiver classifies through the built-in table alone.
func (c *Catalog) signalFromFault(ev types.AgentEvent) (types.Signal, bool) {
	if ev.Fault == nil {
		return types.Signal{}, false
	}
	info, ok := c.ClassifyFault(ev.Fault.Vendor, ev.Fault.Code)
	if !ok {
		return types.Signal{}, false
	}
	evidence := map[string]string{
		"vendor":     ev.Fault.Vendor,
		"source":     ev.Fault.Source,
		"code":       ev.Fault.Code,
		"fault_name": info.Name,
		"raw":        ev.Raw,
	}
	for k, v := range ev.Fault.Attributes {
		evidence["attr_"+k] = v
	}
	return types.Signal{
		Target: types.Target{
			Node:     ev.Node,
			GPUUUID:  ev.GPUUUID,
			GPUIndex: ev.GPUIndex,
		},
		Class:      info.Class,
		Severity:   info.Severity,
		Source:     types.SourceAgentEvent,
		Evidence:   evidence,
		ObservedAt: ev.Timestamp,
	}, true
}

// FaultClass resolves an AgentEvent to its ProblemClass whether it carries a
// genuine XID or a neutral fault descriptor. It is the shared fault identity
// used for cross-source deduplication: a kmsg XID 48 and an nvidia-smi neutral
// "ecc-dbe" on the same GPU both resolve to ClassECCDBE and so collapse to one
// incident. ok is false for an XID or fault the built-in catalog does not
// classify; the caller then falls back to a source-native identity so distinct
// unclassified faults are not over-collapsed.
func FaultClass(ev types.AgentEvent) (types.ProblemClass, bool) {
	if ev.Fault != nil {
		if info, ok := ClassifyFault(ev.Fault.Vendor, ev.Fault.Code); ok {
			return info.Class, true
		}
		return "", false
	}
	if info, ok := ClassifyXID(ev.XID); ok {
		return info.Class, true
	}
	return "", false
}
