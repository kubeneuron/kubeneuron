package kmsg

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The amdgpu families this watcher understands. They ride the SAME /dev/kmsg
// stream, cursor, boot scoping, and acknowledgement watermark as the NVRM Xid
// path — nothing about delivery is vendor-specific, so nothing about delivery
// changes here. Only the line grammar and the emitted identity differ: an
// amdgpu line has no XID, so it is carried as a neutral types.FaultSignal.
//
// UNVALIDATED AGAINST REAL HARDWARE. These patterns were written from the
// amdgpu driver's message vocabulary, not from logs captured on an AMD node.
// They are therefore deliberately narrow: a line that does not match exactly
// produces no fault, and the families that could be read two ways refuse and
// say so (see refusal reasons below) rather than guessing a diagnosis.
const (
	faultVendorAMD = "amd"
	// faultSourceKmsg is the provenance stamped on a kernel-log fault. It is
	// the kernel's own report, which is stronger evidence than a polled
	// counter, and an operator reading the incident should be able to see that.
	faultSourceKmsg = "kmsg"

	// Codes. Each must have a row in internal/detect/fault.go's faultTable, or
	// the detection is counted and then silently opens nothing;
	// TestEveryAMDGPUCodeIsClassified enforces that.
	codeAMDRingTimeout  = "ring-timeout"
	codeAMDECCUncorr    = "ecc-uncorrectable"
	codeAMDPageRetire   = "page-retirement"
	codeAMDPCIeError    = "pcie-error"
	amdRefusalNoDevice  = "amdgpu fault line carries no PCI device address; refusing to attribute it to a GPU"
	amdRefusalRetireBad = "amdgpu bad-page line reports a FAILED reservation; refusing to record it as a successful retirement"
	amdRefusalSoftRecov = "amdgpu ring timeout was soft-recovered by the driver; no reset happened, so this is not a hang to remediate"
	amdRefusalRASClear  = "amdgpu RAS line reports NO uncorrectable errors; refusing to read an all-clear as a fault"
)

var (
	// amdgpuDeviceRe matches the driver-and-device prefix every device-scoped
	// amdgpu message carries: "amdgpu 0000:c3:00.0: amdgpu: ...".
	amdgpuDeviceRe = regexp.MustCompile(`amdgpu\s+([0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F])`)

	// A GPU engine (ring) stopped making progress: the kernel-confirmed hang
	// that precedes an amdgpu-initiated GPU reset.
	amdgpuRingTimeoutRe = regexp.MustCompile(`(?i)\bring\s+(\S+)\s+timeout\b`)
	// ...unless the driver recovered the ring in place. amdgpu prints
	// "ring sdma0 timeout, but soft recovered" when it killed the offending
	// job and the engine kept running: no reset, no lost device, nothing for
	// a playbook to fix. Cordoning a healthy node over a soft recovery is a
	// self-inflicted outage, and these are common enough under a buggy kernel
	// to do it repeatedly.
	amdgpuSoftRecoveredRe = regexp.MustCompile(`(?i)\bsoft\s+recover(?:ed|y)\b`)

	// The RAS counter report the driver prints per error block. The COUNT is
	// load-bearing: amdgpu prints this line with a zero count as a routine
	// all-clear, and reading it as a fault would open an incident on a healthy
	// GPU every time the block is polled.
	amdgpuRASCountRe = regexp.MustCompile(`(?i)\b(\d+)\s+uncorrectable\s+hardware\s+errors?\s+detected(?:\s+in\s+(\S+)\s+block)?`)
	// The countless spelling, evaluated only when no count was printed.
	amdgpuRASEventRe = regexp.MustCompile(`(?i)\buncorrectable\s+(?:hardware\s+)?error(?:s)?\s+detected\b`)
	// The countless spelling has an all-clear twin: "no uncorrectable
	// hardware errors detected". The substring the fault pattern looks for
	// sits inside it verbatim, so without this guard every clean RAS poll
	// opens an ECC incident on a healthy GPU — the loudest possible false
	// positive, on the class that ends in node replacement.
	amdgpuRASNegatedRe = regexp.MustCompile(`(?i)\bno\s+(?:new\s+)?(?:uncorrectable|correctable)\b`)

	// Bad-page reservation: AMD's page retirement, the peer of an NVIDIA row
	// remap. "reserved"/"retired" is the mechanism succeeding.
	amdgpuBadPageRe = regexp.MustCompile(`(?i)\b(?:bad|retired)\s+page\b`)
	amdgpuFailureRe = regexp.MustCompile(`(?i)\b(fail|failed|failure|unable|error creating|cannot)\b`)

	// amdgpu's PCI error handler callbacks.
	amdgpuPCIErrorRe = regexp.MustCompile(`(?i)\bPCI\s+error:\s*(.+)$`)
)

// parseAMDGPU extracts a neutral fault from one amdgpu kernel-log message.
//
// It returns ok=false for every line it does not positively recognize. The
// third return value is a refusal reason: a non-empty string means the line DID
// look like an amdgpu fault report but was deliberately not turned into a
// fault, which the watcher logs so the decision is visible to an operator
// rather than being an unexplained silence.
func parseAMDGPU(msg string) (Event, bool, string) {
	if !strings.Contains(msg, "amdgpu") {
		return Event{}, false, ""
	}
	pci := ""
	if m := amdgpuDeviceRe.FindStringSubmatch(msg); m != nil {
		pci = strings.ToLower(m[1])
	}

	code, attrs, refusal := amdgpuFault(msg)
	if refusal != "" {
		return Event{}, false, refusal
	}
	if code == "" {
		return Event{}, false, ""
	}
	if pci == "" {
		// Unlike an NVRM Xid line — which names its own device and whose code
		// alone is a complete diagnosis — an amdgpu message without the device
		// prefix cannot be tied to any accelerator on a multi-GPU node. A
		// node-wide fault attributed to nothing would drive remediation at a
		// target nobody chose.
		return Event{}, false, amdRefusalNoDevice
	}
	return Event{
		PCIAddr: pci,
		Fault: &types.FaultSignal{
			Vendor:     faultVendorAMD,
			Source:     faultSourceKmsg,
			Code:       code,
			Attributes: attrs,
		},
		Raw: strings.TrimSpace(msg),
	}, true, ""
}

// amdgpuFault classifies the message body. Families are evaluated most specific
// first; the first match wins, so one kernel line never produces two faults.
func amdgpuFault(msg string) (code string, attrs map[string]string, refusal string) {
	if m := amdgpuRingTimeoutRe.FindStringSubmatch(msg); m != nil {
		if amdgpuSoftRecoveredRe.MatchString(msg) {
			return "", nil, amdRefusalSoftRecov
		}
		return codeAMDRingTimeout, map[string]string{"ring": strings.Trim(m[1], ",")}, ""
	}
	if m := amdgpuRASCountRe.FindStringSubmatch(msg); m != nil {
		count, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil || count == 0 {
			// "0 uncorrectable hardware errors detected in umc block" is the
			// driver saying the GPU is FINE. Returning here (rather than
			// falling through to the countless spelling below) is what keeps a
			// health report from becoming an incident.
			return "", nil, ""
		}
		attrs := map[string]string{"uncorrectable_errors": m[1]}
		if m[2] != "" {
			attrs["ras_block"] = strings.Trim(m[2], ",")
		}
		return codeAMDECCUncorr, attrs, ""
	}
	if amdgpuRASEventRe.MatchString(msg) {
		if amdgpuRASNegatedRe.MatchString(msg) {
			return "", nil, amdRefusalRASClear
		}
		return codeAMDECCUncorr, map[string]string{"uncorrectable_errors": "unspecified"}, ""
	}
	if amdgpuBadPageRe.MatchString(msg) {
		if amdgpuFailureRe.MatchString(msg) {
			// A failed reservation is the opposite outcome from a recorded one
			// (NVIDIA's XID 63 vs 64): one is the recovery mechanism working,
			// the other is a device out of spare memory. The exact spelling of
			// the failure line is not known well enough to classify it, and
			// silently filing it as "retirement recorded" would report a
			// dying GPU as self-healed.
			return "", nil, amdRefusalRetireBad
		}
		return codeAMDPageRetire, nil, ""
	}
	if m := amdgpuPCIErrorRe.FindStringSubmatch(msg); m != nil {
		return codeAMDPCIeError, map[string]string{"detail": strings.TrimSpace(m[1])}, ""
	}
	// Deliberately unmatched: "GPU reset begin!/succeeded", which is the
	// driver RECOVERING from a fault this watcher already reported (the ring
	// timeout). Matching both would open two incidents for one failure, and
	// the reset line alone carries no diagnosis of what went wrong.
	return "", nil, ""
}
