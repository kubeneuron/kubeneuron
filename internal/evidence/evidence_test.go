// SPDX-License-Identifier: Apache-2.0

package evidence

import (
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var t0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func pod(app string, running, ready bool) Pod {
	return Pod{Labels: map[string]string{"app": app}, Running: running, Ready: ready}
}

func healthyNode() *Node {
	return &Node{
		Capacity:    map[string]int64{"nvidia.com/gpu": 8, "cpu": 96, "vpc.amazonaws.com/efa": 32},
		Allocatable: map[string]int64{"nvidia.com/gpu": 8, "cpu": 94, "vpc.amazonaws.com/efa": 32},
		Pods: []Pod{
			pod("nvidia-driver-daemonset", true, true),
			pod("nvidia-device-plugin-daemonset", true, true),
			pod("nvidia-dcgm-exporter", true, true),
			{Labels: map[string]string{"name": "aws-efa-k8s-device-plugin"}, Running: true, Ready: true},
		},
	}
}

func TestSoftwareStates(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Node)
		want   types.EvidenceState
	}{
		"healthy": {func(*Node) {}, types.EvidenceOK},
		// The device is there but pods cannot be given all of it: software.
		"allocatable below capacity": {func(n *Node) { n.Allocatable["nvidia.com/gpu"] = 6 }, types.EvidenceDegraded},
		"device plugin not ready":    {func(n *Node) { n.Pods[1] = pod("nvidia-device-plugin-daemonset", true, false) }, types.EvidenceDegraded},
		"driver pending":             {func(n *Node) { n.Pods[0] = pod("nvidia-driver-daemonset", false, false) }, types.EvidenceDegraded},
		"no accelerator resource":    {func(n *Node) { delete(n.Capacity, "nvidia.com/gpu") }, types.EvidenceDegraded},
		// Missing signals are unknown, never ok.
		"no device plugin pod": {func(n *Node) { n.Pods = n.Pods[:1] }, types.EvidenceUnknown},
		"pods unreadable":      {func(n *Node) { n.Pods, n.PodsErr = nil, errors.New("forbidden") }, types.EvidenceUnknown},
	} {
		n := healthyNode()
		tc.mutate(n)
		if got := Software(n); got.State != tc.want || (got.State != types.EvidenceOK && got.Reason == "") {
			t.Errorf("%s: %+v, want %s with a reason", name, got, tc.want)
		}
	}
	if got := Software(nil); got.State != types.EvidenceUnknown {
		t.Errorf("unread node: %+v", got)
	}
	// A degraded reason is not hidden by a later unknown.
	n := healthyNode()
	n.Allocatable["nvidia.com/gpu"] = 7
	n.PodsErr = errors.New("timeout")
	if got := Software(n); got.State != types.EvidenceDegraded {
		t.Errorf("degraded and unread pods: %+v", got)
	}
}

func TestSoftwareRecordsComponents(t *testing.T) {
	got := Software(healthyNode())
	want := map[string]string{"driver": types.ComponentRunning, "device-plugin": types.ComponentRunning, "dcgm-exporter": types.ComponentRunning}
	if len(got.Components) != len(want) {
		t.Fatalf("components %+v", got.Components)
	}
	for _, c := range got.Components {
		if want[c.Name] != c.State || c.Name == "" {
			t.Errorf("component %+v", c)
		}
	}
	if len(got.Resources) != 1 || got.Resources[0] != (types.ResourceObservation{Name: "nvidia.com/gpu", Capacity: 8, Allocatable: 8}) {
		t.Errorf("resources %+v", got.Resources)
	}
}

func TestNetworkStates(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Node)
		want   types.EvidenceState
	}{
		"present":          {func(*Node) {}, types.EvidencePresent},
		"none allocatable": {func(n *Node) { n.Allocatable["vpc.amazonaws.com/efa"] = 0 }, types.EvidenceMissing},
		"plugin without resource": {func(n *Node) {
			delete(n.Capacity, "vpc.amazonaws.com/efa")
		}, types.EvidenceMissing},
		// A node with neither may simply have no such network: not missing,
		// not present.
		"neither": {func(n *Node) { delete(n.Capacity, "vpc.amazonaws.com/efa"); n.Pods = n.Pods[:3] }, types.EvidenceUnknown},
		"pods unreadable": {func(n *Node) {
			delete(n.Capacity, "vpc.amazonaws.com/efa")
			n.Pods, n.PodsErr = nil, errors.New("forbidden")
		}, types.EvidenceUnknown},
		"rdma": {func(n *Node) {
			delete(n.Capacity, "vpc.amazonaws.com/efa")
			n.Capacity["rdma/ib"], n.Allocatable["rdma/ib"] = 1, 1
		}, types.EvidencePresent},
	} {
		n := healthyNode()
		tc.mutate(n)
		if got := Network(n); got.State != tc.want {
			t.Errorf("%s: %+v, want %s", name, got, tc.want)
		}
	}
	if got := Network(nil); got.State != types.EvidenceUnknown {
		t.Errorf("unread node: %+v", got)
	}
}

func TestLocateRulePerClass(t *testing.T) {
	ok := types.SoftwareEvidence{State: types.EvidenceOK}
	degraded := types.SoftwareEvidence{State: types.EvidenceDegraded, Reason: "device-plugin pod gpu-operator/p is not-ready"}
	unknown := types.SoftwareEvidence{State: types.EvidenceUnknown}
	for _, tc := range []struct {
		class    types.ProblemClass
		software types.SoftwareEvidence
		want     types.FaultLocation
	}{
		{types.ClassECCDBE, unknown, types.LocationThisNode},
		{types.ClassFellOffBus, degraded, types.LocationThisNode},
		{types.ClassRowRemapFailure, ok, types.LocationThisNode},
		{types.ClassNVLink, ok, types.LocationThisNode},
		{types.ClassDriverHang, ok, types.LocationThisNode},
		{types.ClassGPULost, ok, types.LocationThisNode},
		// Device healthy but the pod cannot see it: not a hardware fault.
		{types.ClassGPULost, degraded, types.LocationUnknown},
		{types.ClassGPULost, unknown, types.LocationUnknown},
		{types.ClassXIDApp, ok, types.LocationUnknown},
		{types.ClassAgentDown, ok, types.LocationUnknown},
		{types.ClassExporterDown, ok, types.LocationUnknown},
		{types.ProblemClass("storage-timeout"), ok, types.LocationUnknown},
	} {
		got := Locate("i1", tc.class, "n1", t0, nil, tc.software)
		if got.Location != tc.want || got.Rule == "" || got.Group != "" {
			t.Errorf("%s with software %s: %+v, want %s", tc.class, tc.software.State, got, tc.want)
		}
	}
}

func TestLocateGroupsTheSameClassOnSeveralNodes(t *testing.T) {
	peers := []Peer{
		{ID: "i0", Node: "n0", Class: types.ClassXIDApp, OpenedAt: t0.Add(-time.Minute)},
		{ID: "i2", Node: "n2", Class: types.ClassFellOffBus, OpenedAt: t0.Add(-3 * time.Minute)},
		{ID: "i3", Node: "n3", Class: types.ClassFellOffBus, OpenedAt: t0.Add(2 * time.Minute)},
		{ID: "old", Node: "n4", Class: types.ClassFellOffBus, OpenedAt: t0.Add(-Window - time.Second)},
		// The same node again is not a second node.
		{ID: "i1b", Node: "n1", Class: types.ClassFellOffBus, OpenedAt: t0},
	}
	got := Locate("i1", types.ClassFellOffBus, "n1", t0, peers, types.SoftwareEvidence{State: types.EvidenceOK})
	if got.Location != types.LocationShared || got.Group != "i2" {
		t.Fatalf("%+v", got)
	}
	if len(got.Members) != 3 || got.Members[0] != "i2" || got.Members[1] != "i1" || got.Members[2] != "i3" {
		t.Fatalf("members %v", got.Members)
	}
	if len(got.Evidence) != 1 || got.Evidence[0] != "nodes: n1, n2, n3" || got.Rule != "shared: 3 nodes, same class, within 10 min" {
		t.Fatalf("evidence %v rule %q", got.Evidence, got.Rule)
	}
	// Once set the group stays, even after its first incident closed: a
	// later member joins the group a member already names.
	later := Locate("i5", types.ClassFellOffBus, "n5", t0.Add(4*time.Minute),
		[]Peer{{ID: "i3", Node: "n3", Class: types.ClassFellOffBus, OpenedAt: t0.Add(2 * time.Minute), Group: "i2"}}, types.SoftwareEvidence{})
	if later.Group != "i2" || len(later.Members) != 2 {
		t.Fatalf("later member: %+v", later)
	}
	// Every member names the same group.
	again := Locate("i3", types.ClassFellOffBus, "n3", t0.Add(2*time.Minute), append(peers, Peer{ID: "i1", Node: "n1", Class: types.ClassFellOffBus, OpenedAt: t0}), types.SoftwareEvidence{})
	if again.Group != "i2" {
		t.Fatalf("group from another member: %+v", again)
	}
	// A shared silent agent is still shared: one cause, several nodes.
	agents := Locate("a1", types.ClassAgentDown, "n1", t0, []Peer{{ID: "a2", Node: "n2", Class: types.ClassAgentDown, OpenedAt: t0}}, types.SoftwareEvidence{})
	if agents.Location != types.LocationShared {
		t.Fatalf("agents: %+v", agents)
	}
}

func TestBuildUnreadNodeIsUnknown(t *testing.T) {
	inc := &types.Incident{ID: "i1", Class: types.ClassGPULost, Target: types.Target{Node: "n1"}, OpenedAt: t0}
	ev := Build(inc, nil, nil, t0)
	if ev.Software.State != types.EvidenceUnknown || ev.Network.State != types.EvidenceUnknown || ev.Location.Location != types.LocationUnknown || ev.ObservedAt.IsZero() {
		t.Fatalf("%+v", ev)
	}
}
