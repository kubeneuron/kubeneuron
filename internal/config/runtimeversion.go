package config

import "strings"

// RuntimeVersionSatisfies reports whether an attested runtime version is
// acceptable for a profile that pins another.
//
// Exact equality was the original rule, and it turned every DCGM client bump
// into a fleet-wide outage: the agent image ships a pinned client, so raising
// it degrades every installation whose profile still names the old one, with no
// code change and no warning. The versions that matter for compatibility are
// the major and minor components — a 4.6 client was measured serving a 4.5
// engine without trouble — so the patch level is not something a profile should
// have to track.
//
// The comparison stays deliberately narrow:
//
//   - the vendor prefix ("dcgm-") must match exactly, so a profile can never be
//     satisfied by a different runtime that happens to share a number;
//   - major and minor must match exactly. This is not a ">=" rule: a profile
//     names the runtime it was reviewed against, and an unreviewed newer minor
//     is not automatically safe to reset hardware with;
//   - anything below minor — patch, build suffix — is ignored.
//
// A profile that pins a bare major ("dcgm-4") is satisfied by any 4.x, which
// lets an operator opt into a looser rule deliberately rather than by accident.
func RuntimeVersionSatisfies(attested, pinned string) bool {
	attested = strings.TrimSpace(attested)
	pinned = strings.TrimSpace(pinned)
	if attested == "" || pinned == "" {
		return false
	}
	if attested == pinned {
		return true
	}
	attestedPrefix, attestedVersion := splitRuntimeVersion(attested)
	pinnedPrefix, pinnedVersion := splitRuntimeVersion(pinned)
	if attestedPrefix != pinnedPrefix {
		return false
	}
	attestedParts := strings.Split(attestedVersion, ".")
	pinnedParts := strings.Split(pinnedVersion, ".")
	if len(pinnedParts) == 0 || pinnedParts[0] == "" {
		return false
	}
	// Compare only as many components as the profile actually pinned, never
	// more than major.minor.
	compare := len(pinnedParts)
	if compare > 2 {
		compare = 2
	}
	if len(attestedParts) < compare {
		return false
	}
	for i := 0; i < compare; i++ {
		if attestedParts[i] != pinnedParts[i] {
			return false
		}
	}
	return true
}

// splitRuntimeVersion separates the vendor prefix from the numeric version:
// "dcgm-4.6.1" becomes ("dcgm", "4.6.1").
func splitRuntimeVersion(version string) (prefix, number string) {
	if index := strings.LastIndex(version, "-"); index >= 0 {
		return version[:index], version[index+1:]
	}
	return "", version
}
