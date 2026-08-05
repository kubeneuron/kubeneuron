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
// never the outcome. A future AMD/Intel source adds its own rows here.
var faultTable = map[FaultKey]FaultInfo{
	{Vendor: "nvidia", Code: "ecc-dbe"}: {
		Vendor: "nvidia", Code: "ecc-dbe", Name: "double-bit-ecc-error",
		Class: types.ClassECCDBE, Severity: types.SeverityCritical,
	},
	{Vendor: "nvidia", Code: "row-remap-failure"}: {
		Vendor: "nvidia", Code: "row-remap-failure", Name: "row-remap-failed",
		Class: types.ClassRowRemapFailure, Severity: types.SeverityCritical,
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
