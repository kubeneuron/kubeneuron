// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

// journalOSImage is one row of the table that chooses a node's journal
// directory from its Node object. Checked says whether the row was read on a
// live node; the others follow the linked documentation and were not
// checked. Reviewed 2026-11-19. docs/reference-capabilities.md shows the
// same rows.
type journalOSImage struct {
	name    string
	matches func(osImage, providerID string) bool
	dir     string
	checked bool
	source  string
}

func osImagePrefix(prefixes ...string) func(string, string) bool {
	return func(osImage, _ string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(osImage, p) {
				return true
			}
		}
		return false
	}
}

var journalOSImages = []journalOSImage{
	{
		// Checked on a live node: AL2023.12.20261112, EKS v1.34.12.
		name: "Amazon Linux 2023", matches: osImagePrefix("Amazon Linux 2023"),
		dir: kubeneuronv1alpha1.JournalDirVar, checked: true,
		source: "https://docs.aws.amazon.com/linux/al2023/ug/journald.html",
	},
	{
		name: "Bottlerocket", matches: osImagePrefix("Bottlerocket OS"),
		dir:    kubeneuronv1alpha1.JournalDirVar,
		source: "https://bottlerocket.dev/en/os/1.50.x/concepts/restricted-filesystem/",
	},
	{
		name: "Container-Optimized OS", matches: osImagePrefix("Container-Optimized OS"),
		dir:    kubeneuronv1alpha1.JournalDirVar,
		source: "https://docs.cloud.google.com/kubernetes-engine/docs/concepts/node-images",
	},
	{
		name: "Ubuntu", matches: osImagePrefix("Ubuntu"),
		dir:    kubeneuronv1alpha1.JournalDirVar,
		source: "https://learn.microsoft.com/en-us/azure/aks/kubelet-logs",
	},
	{
		name: "Azure Linux", matches: osImagePrefix("CBL-Mariner", "Microsoft Azure Linux", "Azure Linux"),
		dir:    kubeneuronv1alpha1.JournalDirVar,
		source: "https://learn.microsoft.com/en-us/azure/aks/kubelet-logs",
	},
	{
		// The kind node image is Debian-based and keeps a volatile journal.
		// It is told apart by its provider ID, not by the OS image: a Debian
		// host outside kind may keep a persistent journal.
		name: "kind", matches: func(_, providerID string) bool { return strings.HasPrefix(providerID, "kind://") },
		dir: kubeneuronv1alpha1.JournalDirRun, checked: true,
		source: "https://kind.sigs.k8s.io/docs/design/node-image/",
	},
}

// journalDirForImage is the table's directory for a node, or "" when no row
// matches.
func journalDirForImage(osImage, providerID string) string {
	for _, row := range journalOSImages {
		if row.matches(osImage, providerID) {
			return row.dir
		}
	}
	return ""
}
