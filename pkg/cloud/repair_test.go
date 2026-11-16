// SPDX-License-Identifier: Apache-2.0

package cloud

import "testing"

func TestIdentityRejectsChangesWhileApprovalWaits(t *testing.T) {
	pinned := NodeIdentity{Node: "worker", NodeUID: "node-old", ProviderID: "aws:///zone/i-old", InstanceID: "i-old", BootID: "boot-old"}
	if err := pinned.Match(pinned); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*NodeIdentity)
	}{
		{"same name new node", func(n *NodeIdentity) { n.NodeUID = "node-new" }},
		{"new provider target", func(n *NodeIdentity) { n.ProviderID = "aws:///zone/i-new" }},
		{"new instance", func(n *NodeIdentity) { n.InstanceID = "i-new" }},
		{"same instance rebooted", func(n *NodeIdentity) { n.BootID = "boot-new" }},
		{"missing boot evidence", func(n *NodeIdentity) { n.BootID = "" }},
		{"renamed target", func(n *NodeIdentity) { n.Node = "other-worker" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := pinned
			tc.change(&current)
			if err := pinned.Match(current); err == nil {
				t.Fatal("changed or incomplete identity authorized")
			}
		})
	}
	if err := (NodeIdentity{}).Match(pinned); err == nil {
		t.Fatal("legacy incident with no pinned identity authorized")
	}
	if err := (NodeIdentity{}).Match(NodeIdentity{}); err == nil {
		t.Fatal("two empty identities treated as matching evidence")
	}
}
