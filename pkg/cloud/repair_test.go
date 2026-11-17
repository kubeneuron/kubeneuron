// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"errors"
	"testing"
	"time"
)

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

func TestIdentityMismatchRequiresCompleteEvidence(t *testing.T) {
	p := NodeIdentity{Node: "n", NodeUID: "u", ProviderID: "p", InstanceID: "i", BootID: "b"}
	current := p
	current.BootID = "new"
	if !errors.Is(p.Match(current), ErrIdentityChanged) {
		t.Fatal("confirmed reboot not classified")
	}
	current.BootID = ""
	if errors.Is(p.Match(current), ErrIdentityChanged) {
		t.Fatal("missing evidence classified as changed target")
	}
	if errors.Is((NodeIdentity{}).Match(p), ErrIdentityChanged) {
		t.Fatal("missing pin classified as changed target")
	}
}

func TestRecycleResultRequiresSameTargetNewBoot(t *testing.T) {
	pin := NodeIdentity{Node: "n", NodeUID: "u", ProviderID: "p", InstanceID: "i", BootID: "old"}
	after := pin
	after.BootID = "new"
	good := RecycleResult{Before: pin, After: after, CompletedAt: time.Now()}
	if err := good.Validate(pin); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*RecycleResult){
		func(r *RecycleResult) { r.After.NodeUID = "other" },
		func(r *RecycleResult) { r.After.InstanceID = "other" },
		func(r *RecycleResult) { r.After.ProviderID = "other" },
		func(r *RecycleResult) { r.After.BootID = "old" },
		func(r *RecycleResult) { r.After.BootID = "" },
		func(r *RecycleResult) { r.CompletedAt = time.Time{} },
	} {
		bad := good
		change(&bad)
		if err := bad.Validate(pin); err == nil {
			t.Fatalf("accepted invalid evidence: %+v", bad)
		}
	}
}
