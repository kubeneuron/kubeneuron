// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func poolOrderEngine(t *testing.T, order recoverymode.Strategy) *playbook.Engine {
	t.Helper()
	e, err := playbook.NewEngine(map[string]*playbook.Playbook{"replace": {Steps: []playbook.Step{{Action: "platform.replace_node"}}}},
		[]playbook.Policy{{Class: types.ClassFellOffBus, Playbook: "replace", ReplacementStrategy: order}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// A pool reads "replacement not possible" before its first incident, whatever
// the replacement order, when what an incident would meet needs the operator.
func TestPoolSaysWhetherAnyReplacementIsPossible(t *testing.T) {
	const autoRepair = "EKS node auto repair is enabled for node group gpu, so KubeNeuron does not replace its nodes."
	allowed := cloud.RecoveryEligibility{Mode: cloud.RepairOff, ReplaceAllowed: true}
	for _, tc := range []struct {
		name        string
		order       recoverymode.Strategy
		verdict     cloud.RecoveryEligibility
		hold        *cloud.RecoveryHold
		err         error
		replacement string
		code        string
		addFirst    string // the add-first status; "" for RemoveFirst
		checked     bool   // the add-first check ran
		permission  bool   // ... told the permissions were already checked
	}{
		{"remove-first, auto repair on", recoverymode.RemoveFirst, cloud.RecoveryEligibility{Mode: cloud.RepairOn, Reason: autoRepair + " KubeNeuron checks again every minute."},
			&cloud.RecoveryHold{Code: cloud.HoldNativeRepair, Reason: "long", PoolReason: autoRepair, PoolAction: "turn it off"}, nil, "unavailable", cloud.HoldNativeRepair, "", false, false},
		{"remove-first, possible", recoverymode.RemoveFirst, allowed, nil, nil, "available", "", "", false, false},
		{"add-first, permission missing", recoverymode.AddFirst, cloud.RecoveryEligibility{}, nil, &cloud.RecoveryHold{Code: cloud.HoldPermissionMissing, Reason: "missing ec2:TerminateInstances", Action: "Allow it."}, "unavailable", cloud.HoldPermissionMissing, "unavailable", false, false},
		{"add-first, read failed", recoverymode.AddFirst, cloud.RecoveryEligibility{}, nil, &cloud.RecoveryHold{Code: cloud.HoldReadFailed, Reason: "could not read"}, "unknown", cloud.HoldReadFailed, "available", true, false},
		{"add-first, unclassified error", recoverymode.AddFirst, cloud.RecoveryEligibility{}, nil, errors.New("boom"), "unknown", "read-failed", "available", true, false},
		{"add-first, possible", recoverymode.AddFirst, allowed, nil, nil, "available", "", "available", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c Controller
			nodes := []types.Node{{Name: "n1", UID: "uid-1", Labels: map[string]string{"eks.amazonaws.com/nodegroup": "gpu"}, Devices: tpuDevices}}
			checked, permission := false, false
			check := func(_ types.Node, permissionChecked bool, _ recoverymode.SuccessorZone, _ *[]string) error {
				checked, permission = true, permissionChecked
				return nil
			}
			replacement := func(types.Node) (cloud.RecoveryEligibility, *cloud.RecoveryHold, error) {
				return tc.verdict, tc.hold, tc.err
			}
			c.inventory.recordPools(nodes, nil, &RuntimeConfig{Engine: poolOrderEngine(t, tc.order)}, time.Now().UTC(), check, replacement)
			row := c.PoolAvailability().Pools[0]
			if row.ReplacementStatus != tc.replacement || row.ReplacementReasonCode != tc.code || row.CheckedAt == nil {
				t.Fatalf("replacement %q/%q, want %q/%q: %+v", row.ReplacementStatus, row.ReplacementReasonCode, tc.replacement, tc.code, row)
			}
			if checked != tc.checked || permission != tc.permission {
				t.Fatalf("add-first check ran = %v (permissions already checked = %v), want %v (%v)", checked, permission, tc.checked, tc.permission)
			}
			if tc.addFirst == "available" && tc.order == recoverymode.AddFirst && row.Status == "" {
				t.Fatalf("no add-first status: %+v", row)
			}
			if tc.addFirst == "unavailable" && (row.Status != "unavailable" || row.ReasonCode != tc.code) {
				t.Fatalf("add-first status %q/%q: %+v", row.Status, row.ReasonCode, row)
			}
			if tc.order == recoverymode.RemoveFirst && row.Status != "" {
				t.Fatalf("remove-first pool has an add-first status: %+v", row)
			}
			if tc.code == cloud.HoldNativeRepair && row.ReplacementReason != autoRepair {
				t.Fatalf("reason %q", row.ReplacementReason)
			}
		})
	}
}

// RemoveFirst pools are checked within the same budget as the others.
func TestRemoveFirstPoolsShareTheCheckBudget(t *testing.T) {
	var c Controller
	var nodes []types.Node
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		nodes = append(nodes, types.Node{Name: name, Labels: map[string]string{"cloud.google.com/gke-nodepool": name}, Devices: tpuDevices})
	}
	calls := 0
	replacement := func(types.Node) (cloud.RecoveryEligibility, *cloud.RecoveryHold, error) {
		calls++
		return cloud.RecoveryEligibility{Mode: cloud.RepairOff, ReplaceAllowed: true}, nil, nil
	}
	c.inventory.recordPools(nodes, nil, &RuntimeConfig{Engine: poolOrderEngine(t, recoverymode.RemoveFirst)}, time.Now().UTC(), func(types.Node, bool, recoverymode.SuccessorZone, *[]string) error { return nil }, replacement)
	if calls != 4 {
		t.Fatalf("checked %d pools, want the budget of 4", calls)
	}
}

func TestRemoveFirstPoolWithoutProvider(t *testing.T) {
	var c Controller
	nodes := []types.Node{{Name: "n1", Labels: map[string]string{"cloud.google.com/gke-nodepool": "p"}, Devices: tpuDevices}}
	c.inventory.recordPools(nodes, nil, &RuntimeConfig{Engine: poolOrderEngine(t, recoverymode.RemoveFirst)}, time.Now().UTC(), nil, nil)
	row := c.PoolAvailability().Pools[0]
	if row.ReplacementStatus != "unknown" || row.ReplacementReason != "no cloud provider configured" || row.Status != "" {
		t.Fatal(row)
	}
}

func TestGKEPoolReportsMultiZoneRefusalForAddFirstPolicy(t *testing.T) {
	var c Controller
	nodes := []types.Node{{Name: "n1", Labels: map[string]string{"cloud.google.com/gke-nodepool": "gpu"}, Devices: tpuDevices}}
	refusal := &cloud.PoolRefusal{Code: cloud.PoolMultiZone, Reason: "multi-zone pool", Action: "select RemoveFirst"}
	check := func(types.Node, bool, recoverymode.SuccessorZone, *[]string) error { return refusal }
	replacement := func(types.Node) (cloud.RecoveryEligibility, *cloud.RecoveryHold, error) {
		return cloud.RecoveryEligibility{Mode: cloud.RepairOff, ReplaceAllowed: true}, nil, nil
	}
	c.inventory.recordPools(nodes, nil, &RuntimeConfig{Engine: poolOrderEngine(t, recoverymode.AddFirst)}, time.Now(), check, replacement)
	row := c.PoolAvailability().Pools[0]
	if row.ReplacementStatus != "available" || row.Status != "unavailable" || row.ReasonCode != refusal.Code || row.Reason != refusal.Reason || row.Action != refusal.Action {
		t.Fatalf("replacement assessment: %+v", row)
	}
}

func TestPoolRechecksWhenSuccessorZoneChanges(t *testing.T) {
	var c Controller
	nodes := []types.Node{{Name: "n1", UID: "uid", Labels: map[string]string{"kubernetes.azure.com/agentpool": "gpu"}, Devices: tpuDevices}}
	now := time.Now()
	for i, zone := range []recoverymode.SuccessorZone{"", recoverymode.SameZone, recoverymode.AnyPoolZone, recoverymode.SameZone} {
		engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"replace": {Steps: []playbook.Step{{Action: "platform.replace_node"}}}}, []playbook.Policy{{Class: types.ClassFellOffBus, Playbook: "replace", SuccessorZone: zone}})
		if err != nil {
			t.Fatal(err)
		}
		called := false
		c.inventory.recordPools(nodes, nil, &RuntimeConfig{Engine: engine}, now, func(_ types.Node, _ bool, got recoverymode.SuccessorZone, _ *[]string) error {
			called = true
			want := zone
			if want == "" {
				want = recoverymode.SameZone
			}
			if got != want {
				t.Fatalf("checked %s, want %s", got, want)
			}
			if got == recoverymode.SameZone {
				return &cloud.PoolRefusal{Code: cloud.PoolMultiZone, Reason: "two zones"}
			}
			return nil
		}, nil)
		row := c.PoolAvailability().Pools[0]
		if called != (i != 1) || (row.Status == "available") != (zone == recoverymode.AnyPoolZone) {
			t.Fatalf("called=%v row=%+v", called, row)
		}
	}
}

func TestPoolsReportMissingProfileBeforeAnIncident(t *testing.T) {
	var c Controller
	settings := autoSettings()
	rc := &RuntimeConfig{Engine: modeEngine(t, recoverymode.Automatic, settings)}
	nodes := []types.Node{{Name: "n1", UID: "uid-1", Labels: map[string]string{"eks.amazonaws.com/nodegroup": "gpu", "pool": "train"}, Devices: tpuDevices}}
	now := time.Now()
	for _, present := range []bool{false, true} {
		if present {
			rc.AcceleratorProfiles = []config.AcceleratorRuntimeProfile{{Name: autoProfile}}
		}
		c.inventory.recordPools(nodes, nil, rc, now, nil, nil)
		row := c.PoolAvailability().Pools[0]
		if present && len(row.MissingQualifiedProfiles) != 0 || !present && !slices.Equal(row.MissingQualifiedProfiles, []string{autoProfile}) {
			t.Fatalf("profile present=%v: %+v", present, row)
		}
		if row.ReplacementReasonCode != "no-cloud-provider" {
			t.Fatalf("lost independent cloud diagnosis: %+v", row)
		}
	}
}

func TestPoolQualificationUsesNodeLabelsAndVendor(t *testing.T) {
	match := testNVIDIAResetProfile()
	match.Name, match.NodeSelector = autoProfile, map[string]string{"pool": "train"}
	other := match
	other.Name = "another"
	outside := match
	outside.NodeSelector = map[string]string{"pool": "elsewhere"}
	for _, tc := range []struct {
		name     string
		profiles []config.AcceleratorRuntimeProfile
		want     string
	}{
		{"none", []config.AcceleratorRuntimeProfile{outside}, "no AcceleratorRuntimeProfile selects this pool's nodes"},
		{"overlap", []config.AcceleratorRuntimeProfile{match, other}, "more than one AcceleratorRuntimeProfile selects this pool's nodes (" + autoProfile + " and another)"},
		{"unlisted", []config.AcceleratorRuntimeProfile{outside, other}, "node profile another is not listed in spec.automatic.qualifiedProfiles"},
		{"qualified", []config.AcceleratorRuntimeProfile{match}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c Controller
			rc := &RuntimeConfig{Engine: modeEngine(t, recoverymode.Automatic, autoSettings()), AcceleratorProfiles: tc.profiles}
			nodes := []types.Node{{Name: "n1", UID: "uid-1", Labels: map[string]string{"pool": "train", "eks.amazonaws.com/nodegroup": "gpu"}, Devices: []types.DeviceCount{{Resource: "nvidia.com/gpu", Capacity: 1}}}}
			c.inventory.recordPools(nodes, nil, rc, time.Now(), nil, nil)
			got := c.PoolAvailability().Pools[0].AutomaticQualificationReasons
			var want []string
			if tc.want != "" {
				want = []string{tc.want}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
			if tc.name == "none" {
				second := nodes[0]
				second.Name, second.UID = "n2", "uid-2"
				second.Labels = map[string]string{"pool": "elsewhere", "eks.amazonaws.com/nodegroup": "gpu"}
				c.inventory.recordPools(append(nodes, second), nil, rc, time.Now(), nil, nil)
				mixed := c.PoolAvailability().Pools[0].AutomaticQualificationReasons
				if !slices.Equal(mixed, []string{"for some of this pool's nodes: no AcceleratorRuntimeProfile selects the node"}) {
					t.Fatalf("mixed pool: %v", mixed)
				}
			}
		})
	}
}

func TestPoolQualificationInvalidProfileSuggestsCorrection(t *testing.T) {
	rc := &RuntimeConfig{AcceleratorProfiles: []config.AcceleratorRuntimeProfile{{Name: autoProfile}}}
	got := poolQualificationReason(rc, autoSettings(), map[string]string{"pool": "train"}, types.AcceleratorVendorNVIDIA, nil)
	if !strings.HasPrefix(got, "correct the invalid AcceleratorRuntimeProfile: ") || !strings.Contains(got, autoProfile) {
		t.Fatal(got)
	}
}
