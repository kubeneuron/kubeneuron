package types

import "strings"

// NormalizePCIAddress reduces every spelling of a PCI device address that
// reaches this control plane to ONE comparable form: lowercase, the PCI
// function suffix dropped, and the domain trimmed and re-padded to four hex
// digits ("0000:3b:00").
//
// It lives here, in the package both the agent and the controller already
// depend on, because the address is now part of a device's identity: it keys
// the agent's detection dedup, it rides on types.Target, and it is the column
// the store matches an unattributed incident on. The spellings genuinely
// differ per source — an NVRM Xid line prints "PCI:0000:3b:00" with no
// function, nvidia-smi prints "00000000:3B:00.0" with an eight-digit domain,
// and an amdgpu line prints "0000:c3:00.0" — so any site that compares two of
// them with ==, or that folds one into a key without asking this function,
// silently decides that one physical GPU is two.
//
// That is the concrete failure this single definition exists to prevent: the
// kernel fault and the vendor tool's later, UUID-bearing report of the SAME
// device stop matching, the incident is never promoted onto the real device,
// and a node that was already cordoned and drained is parked for a human with
// "reset target unattributed". Callers must ask this function rather than
// spelling the rule again at their own call site.
//
// An address this function cannot parse is returned lowercased and trimmed
// rather than rewritten: an unrecognized spelling must stay comparable to
// itself, and inventing structure for it would be a guess about hardware.
func NormalizePCIAddress(addr string) string {
	a := strings.ToLower(strings.TrimSpace(addr))
	if a == "" {
		return ""
	}
	if i := strings.LastIndexByte(a, '.'); i >= 0 {
		a = a[:i] // drop the PCI function (".0")
	}
	parts := strings.Split(a, ":")
	if len(parts) == 2 {
		parts = append([]string{"0000"}, parts...) // no domain printed
	}
	if len(parts) != 3 {
		return a
	}
	domain := strings.TrimLeft(parts[0], "0")
	if len(domain) > 4 {
		return a
	}
	domain = strings.Repeat("0", 4-len(domain)) + domain
	return domain + ":" + parts[1] + ":" + parts[2]
}
