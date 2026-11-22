// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"testing"
	"time"

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
			check := func(_ types.Node, permissionChecked bool) error {
				checked, permission = true, permissionChecked
				return nil
			}
			replacement := func(types.Node) (cloud.RecoveryEligibility, *cloud.RecoveryHold, error) {
				return tc.verdict, tc.hold, tc.err
			}
			c.inventory.recordPools(nodes, nil, poolOrderEngine(t, tc.order), time.Now().UTC(), check, replacement)
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
	c.inventory.recordPools(nodes, nil, poolOrderEngine(t, recoverymode.RemoveFirst), time.Now().UTC(), func(types.Node, bool) error { return nil }, replacement)
	if calls != 4 {
		t.Fatalf("checked %d pools, want the budget of 4", calls)
	}
}

func TestRemoveFirstPoolWithoutProvider(t *testing.T) {
	var c Controller
	nodes := []types.Node{{Name: "n1", Labels: map[string]string{"cloud.google.com/gke-nodepool": "p"}, Devices: tpuDevices}}
	c.inventory.recordPools(nodes, nil, poolOrderEngine(t, recoverymode.RemoveFirst), time.Now().UTC(), nil, nil)
	row := c.PoolAvailability().Pools[0]
	if row.ReplacementStatus != "unknown" || row.ReplacementReason != "no cloud provider configured" || row.Status != "" {
		t.Fatal(row)
	}
}
