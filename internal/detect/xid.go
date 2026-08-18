package detect

import (
	"strconv"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// XIDInfo classifies one NVIDIA XID error code: what it means, how severe it
// is, and which problem class (and therefore which playbook, via
// policies.yaml) it maps to.
//
// References: NVIDIA XID catalog (docs.nvidia.com/deploy/xid-errors) and
// field experience. See docs/xid-catalog.md for the rationale behind each
// default policy.
type XIDInfo struct {
	Code        int
	Name        string
	Class       types.ProblemClass
	Severity    types.Severity
	Description string
	// Threshold, if > 0, means the XID is only actionable after this many
	// occurrences within ThresholdWindow (rolling). Zero means act on first
	// occurrence.
	Threshold       int
	ThresholdWindow string // Go duration string, e.g. "1h"
}

// xidTable is the classification table for XID codes KubeNeuron reacts to.
// Codes not listed here are recorded (counted in metrics and the event log)
// but open no incident.
var xidTable = map[int]XIDInfo{
	13: {
		Code: 13, Name: "graphics-engine-exception",
		Class: types.ClassXIDApp, Severity: types.SeverityInfo,
		Description: "Graphics engine exception. Usually a workload bug; suspect the GPU only if it recurs across different workloads.",
		Threshold:   3, ThresholdWindow: "1h",
	},
	31: {
		Code: 31, Name: "gpu-memory-page-fault",
		Class: types.ClassXIDApp, Severity: types.SeverityInfo,
		Description: "GPU memory page fault. Almost always an application bug; hardware suspect only when recurrent across workloads.",
		Threshold:   3, ThresholdWindow: "1h",
	},
	43: {
		Code: 43, Name: "reset-channel-verification-error",
		Class: types.ClassXIDApp, Severity: types.SeverityInfo,
		Description: "Reset channel verification error. Usually secondary to an application termination or another GPU fault; correlate with the triggering event rather than acting on it alone.",
		Threshold:   10, ThresholdWindow: "24h",
	},
	46: {
		Code: 46, Name: "gpu-stopped-processing",
		Class: types.ClassXIDApp, Severity: types.SeverityInfo,
		Description: "GPU stopped processing. Typically follows an application crash or a preceding fault; hardware is suspect only when it recurs across different workloads.",
		Threshold:   10, ThresholdWindow: "24h",
	},
	48: {
		Code: 48, Name: "double-bit-ecc-error",
		Class: types.ClassECCDBE, Severity: types.SeverityCritical,
		Description: "Double-bit ECC error: uncorrectable memory fault. Drain and reset immediately; verify row remap afterwards; recurrence points to RMA.",
	},
	63: {
		Code: 63, Name: "row-remap-recorded",
		Class: types.ClassRowRemapOK, Severity: types.SeverityWarning,
		Description: "ECC page retirement / row remap recorded successfully — this is the recovery mechanism working, not a failure. The remap applies on the next GPU reset, so schedule one when the device is idle.",
	},
	64: {
		Code: 64, Name: "row-remap-failed",
		Class: types.ClassRowRemapFailure, Severity: types.SeverityCritical,
		Description: "ECC page retirement / row remap could not be recorded. Drain, reset, and re-check row-remap state and diagnostics; escalate to hardware replacement when it recurs.",
	},
	74: {
		Code: 74, Name: "nvlink-error",
		Class: types.ClassNVLink, Severity: types.SeverityCritical,
		Description: "NVLink error. Drain and reset; if it recurs within 24h, escalate to reboot and flag cabling/topology in the ticket.",
	},
	79: {
		Code: 79, Name: "gpu-fell-off-bus",
		Class: types.ClassFellOffBus, Severity: types.SeverityCritical,
		Description: "GPU has fallen off the bus. A GPU reset cannot recover this — drain and reboot the node; recurrence points to RMA.",
	},
	92: {
		Code: 92, Name: "high-single-bit-ecc-rate",
		// Deliberately not ClassECCDBE: single-bit errors are corrected and
		// warrant observation, while DBE is a critical drain-and-reset. A
		// shared class would force one policy for both.
		Class: types.ClassECCSBERate, Severity: types.SeverityWarning,
		Description: "High single-bit ECC error rate. Observe; correlate with volatile SBE counter trends before acting.",
		Threshold:   3, ThresholdWindow: "24h",
	},
	94: {
		Code: 94, Name: "contained-ecc-error",
		Class: types.ClassECCContained, Severity: types.SeverityWarning,
		Description: "Contained ECC error (A100+). Only the affected workload is corrupted: restart it, count the event; 3+ per week means drain-and-reset.",
	},
	95: {
		Code: 95, Name: "uncontained-ecc-error",
		Class: types.ClassECCDBE, Severity: types.SeverityCritical,
		Description: "Uncontained ECC error. GPU state is corrupt: drain and reset immediately; recurrence points to RMA.",
	},
	119: {
		Code: 119, Name: "gsp-rpc-timeout",
		Class: types.ClassGSPError, Severity: types.SeverityCritical,
		Description: "GSP RPC timeout. GPU reset first; recurrence escalates to reboot, persistent failures to driver reinstall.",
	},
	120: {
		Code: 120, Name: "gsp-error",
		Class: types.ClassGSPError, Severity: types.SeverityCritical,
		Description: "GSP error. Same ladder as XID 119: reset, then reboot, then driver reinstall.",
	},
}

// ClassifyXID returns the classification for an XID code, and whether the
// code is one KubeNeuron acts on.
func ClassifyXID(code int) (XIDInfo, bool) {
	info, ok := xidTable[code]
	return info, ok
}

// XIDTable returns the full classification table (for docs, UI, and tests).
func XIDTable() []XIDInfo {
	out := make([]XIDInfo, 0, len(xidTable))
	for _, v := range xidTable {
		out = append(out, v)
	}
	return out
}

// SignalFromAgentEvent converts an agent event into a Signal, or returns
// ok=false when it is not actionable (it should still be counted). A
// neutral-envelope event (types.FaultSignal) classifies through the fault
// catalog; otherwise the genuine XID classifies through the XID catalog.
func SignalFromAgentEvent(ev types.AgentEvent) (types.Signal, bool) {
	if ev.Fault != nil {
		return SignalFromFault(ev)
	}
	info, ok := ClassifyXID(ev.XID)
	if !ok {
		return types.Signal{}, false
	}
	return signalFromXID(ev, info), true
}

// signalFromXID builds the Signal for a classified XID.
//
// It is a shared function rather than a body repeated twice because the two
// copies had already diverged. The vendor key below was added to the
// package-level SignalFromAgentEvent, which nothing in the controller calls:
// ingest goes through Catalog.SignalFromAgentEvent, whose identical-looking
// copy did not have it. So every XID-opened incident — the oldest and most
// common detection path — still carried no vendor at all, while the comment
// explaining the key claimed otherwise. The Catalog is nil-safe, so this was
// true with and without a signal-mapping override, i.e. always.
func signalFromXID(ev types.AgentEvent, info XIDInfo) types.Signal {
	return types.Signal{
		Target: types.Target{
			Node:     ev.Node,
			GPUUUID:  ev.GPUUUID,
			GPUIndex: ev.GPUIndex,
		},
		Class:    info.Class,
		Severity: info.Severity,
		Source:   types.SourceAgentEvent,
		Evidence: map[string]string{
			// An XID is an NVIDIA-native encoding — there is no such thing as
			// an AMD XID — so the vendor is not an inference here, it is what
			// the encoding means. Declaring it keeps incidents opened by the
			// oldest and most common detection path from being the ones with
			// no vendor on them.
			"vendor":   string(types.AcceleratorVendorNVIDIA),
			"xid":      strconv.Itoa(ev.XID),
			"xid_name": info.Name,
			"raw":      ev.Raw,
		},
		ObservedAt: ev.Timestamp,
	}
}
