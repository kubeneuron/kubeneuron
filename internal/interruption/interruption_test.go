// SPDX-License-Identifier: Apache-2.0

package interruption

import (
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

func TestFromNode(t *testing.T) {
	at := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		node     Node
		kind     cloud.InterruptionKind
		source   string
		deadline time.Time
	}{
		"gke host maintenance": {Node{Taints: []Taint{{Key: "cloud.google.com/impending-node-termination"}}},
			cloud.InterruptionHostMaintenance, SourceGKEMaintenanceTaint, time.Time{}},
		"gke scheduled maintenance": {Node{Labels: map[string]string{"cloud.google.com/scheduled-maintenance-time": "1795143600"}},
			cloud.InterruptionScheduledMaintenance, SourceGKEMaintenanceLabel, at},
		"aks scheduled event": {Node{Conditions: []Condition{{Type: "VMEventScheduled", Status: "True", LastTransition: at}}},
			cloud.InterruptionScheduledMaintenance, SourceAKSScheduledEvent, time.Time{}},
		"nth spot": {Node{Taints: []Taint{{Key: "aws-node-termination-handler/spot-itn", Value: "spot-itn-abc"}}},
			cloud.InterruptionSpotReclaim, SourceNTHSpot, time.Time{}},
		"nth rebalance": {Node{Taints: []Taint{{Key: "aws-node-termination-handler/rebalance-recommendation", Value: "rebalance-1"}}},
			cloud.InterruptionRebalance, SourceNTHRebalance, time.Time{}},
		"nth scheduled": {Node{Taints: []Taint{{Key: "aws-node-termination-handler/scheduled-maintenance", Value: "instance-event-1"}}},
			cloud.InterruptionScheduledMaintenance, SourceNTHScheduled, time.Time{}},
	} {
		tc.node.Name, tc.node.ProviderID = "n1", "aws:///zone/i-1"
		got := FromNode(tc.node)
		if len(got) != 1 {
			t.Fatalf("%s: %+v", name, got)
		}
		n := got[0]
		if n.Node != "n1" || n.Kind != tc.kind || n.Source != tc.source || !n.Deadline.Equal(tc.deadline) || n.Validate() != nil {
			t.Errorf("%s: %+v (validate %v)", name, n, n.Validate())
		}
	}
}

func TestFromNodeIgnoresOtherSignals(t *testing.T) {
	n := Node{Name: "n1", ProviderID: "p",
		Taints: []Taint{{Key: "node.kubernetes.io/unschedulable"}, {Key: "aws-node-termination-handler/asg-lifecycle-termination"}},
		Labels: map[string]string{"cloud.google.com/scheduled-maintenance-time": "soon"},
		Conditions: []Condition{
			{Type: "VMEventScheduled", Status: "False"},
			{Type: "Ready", Status: "True"},
		}}
	if got := FromNode(n); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

// The key tells repeats from new notices: the same taint value is the same
// notice, a new event ID or a new maintenance time is another.
func TestFromNodeKeys(t *testing.T) {
	key := func(n Node) string { n.Name, n.ProviderID = "n1", "p"; return FromNode(n)[0].Key }
	spot := func(v string) Node {
		return Node{Taints: []Taint{{Key: "aws-node-termination-handler/spot-itn", Value: v}}}
	}
	first, again := key(spot("e1")), key(spot("e1"))
	if first != again || first == key(spot("e2")) {
		t.Fatal("taint value does not key the notice")
	}
	gke := func(v string) Node {
		return Node{Labels: map[string]string{"cloud.google.com/scheduled-maintenance-time": v}}
	}
	if key(gke("1795143600")) == key(gke("1795147200")) {
		t.Fatal("a rescheduled maintenance is the same notice")
	}
}
