package types

import "testing"

// TestNormalizePCIAddressReducesEverySourcesSpellingToOneIdentity is the unit
// pin under a rule the rest of the control plane now depends on for device
// identity. Three sources print the same slot three ways, and each pair of them
// meets somewhere: the kernel's XID line opens the incident, nvidia-smi's
// report is what promotes it onto a real GPU UUID, and the sysfs form is what a
// reset is finally issued against. Any pair that fails to reduce to the same
// string means the promotion never matches — the incident stays unattributed,
// the reset is refused as permanently infeasible, and a node that has already
// been cordoned and drained of tenant work is parked for a human.
func TestNormalizePCIAddressReducesEverySourcesSpellingToOneIdentity(t *testing.T) {
	for _, group := range [][]string{
		// One slot, as the NVRM Xid line, nvidia-smi, sysfs and an amdgpu
		// line respectively spell it.
		{"0000:3b:00", "00000000:3B:00.0", "0000:3b:00.0", "3b:00"},
		{"0001:af:00", "00000001:AF:00.0", "0001:af:00.1", "  0001:AF:00  "},
	} {
		want := NormalizePCIAddress(group[0])
		if want == "" {
			t.Fatalf("NormalizePCIAddress(%q) = %q: the test's own input is not an address this function "+
				"recognizes, so it would prove nothing", group[0], want)
		}
		for _, spelling := range group {
			if got := NormalizePCIAddress(spelling); got != want {
				t.Errorf("NormalizePCIAddress(%q) = %q, want %q: two spellings of ONE physical slot compare "+
					"unequal, so the kernel fault and the vendor tool's report of the same GPU are treated as "+
					"two devices and the incident is never promoted onto the device that failed",
					spelling, got, want)
			}
		}
	}

	// An address this function cannot parse must stay comparable to itself
	// rather than be rewritten into a guess about hardware.
	if got := NormalizePCIAddress("  NOT-AN-ADDRESS  "); got != "not-an-address" {
		t.Errorf("NormalizePCIAddress on an unrecognized spelling = %q, want it lowercased and trimmed and "+
			"otherwise untouched", got)
	}
	if got := NormalizePCIAddress(""); got != "" {
		t.Errorf("NormalizePCIAddress(\"\") = %q, want \"\": an absent address must stay absent, because "+
			"empty is what distinguishes a signal that names no device from one that does", got)
	}
}
