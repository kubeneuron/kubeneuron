// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/cloud/cloudtest"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

func TestAzureConformance(t *testing.T) {
	cloudtest.Conformance(t, cloudtest.Case{
		Provider: testProvider(newFakeAKS(1)), ProviderID: "azure://" + testInstance(0), Instance: testInstance(0),
		Foreign: []string{
			"aws:///us-east-1a/i-0abc",
			"azure:///subscriptions/s/resourceGroups/r/providers/Microsoft.Compute/virtualMachines/gpu-0",
			"azure://" + testInstance(0) + "/extra",
		},
	})
}

func TestProviderRegisteredForReplacementOnly(t *testing.T) {
	caps, ok := cloud.DeclaredCapabilities(providerName)
	if !ok || caps.ReinitializeInPlace || !caps.Replace {
		t.Fatalf("azure capabilities = %+v registered=%t, want replacement only", caps, ok)
	}
	if got := testProvider(newFakeAKS(1)).Capabilities(); got != caps {
		t.Fatalf("Capabilities() = %+v, want %+v", got, caps)
	}
}

func TestFactoryRequiresClusterSettings(t *testing.T) {
	_, err := cloud.Build(t.Context(), providerName, cloud.Config{Options: map[string]string{OptionSubscription: testSub, OptionCluster: testCluster}})
	if err == nil || !strings.Contains(err.Error(), "resource group") {
		t.Fatalf("err = %v, want the missing settings named", err)
	}
}

func TestRecycleIsNeverViable(t *testing.T) {
	f := newFakeAKS(1)
	p := testProvider(f)
	for _, err := range []error{p.CheckRecycle(t.Context(), testInstance(0)), p.Recycle(t.Context(), testInstance(0))} {
		if !errors.Is(err, cloud.ErrRecycleNotViable) {
			t.Fatalf("err = %v, want ErrRecycleNotViable", err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %q", f.calls)
	}
}

func testRef(i int) cloud.NodeRef {
	m := testMachine(i)
	return cloud.NodeRef{
		Identity:    cloud.NodeIdentity{Node: m.Name, NodeUID: "uid", ProviderID: "azure://" + testInstance(i), InstanceID: testInstance(i), BootID: "boot"},
		Ready:       true,
		EventsKnown: true,
	}
}

func TestRepairPolicyIsNotReadyOnlyForAMachineOfThisCluster(t *testing.T) {
	p := testProvider(newFakeAKS(2))
	policy, err := p.RepairPolicy(t.Context(), testRef(1))
	if err != nil || policy.Mode != cloud.RepairNotReadyOnly {
		t.Fatalf("policy = %+v, %v", policy, err)
	}
	wrongNode := testRef(1)
	wrongNode.Identity.Node = testMachine(0).Name
	stray := testRef(5)
	for name, ref := range map[string]cloud.NodeRef{"other node name": wrongNode, "machine not in pool": stray} {
		if policy, err := p.RepairPolicy(t.Context(), ref); err == nil || policy.Mode != cloud.RepairUnknown {
			t.Fatalf("%s: policy = %+v, %v", name, policy, err)
		}
	}
}

func TestOpenRepair(t *testing.T) {
	recent := cloud.NodeEvent{Source: autoRepairSource, Reason: "NodeRebootStart", Time: time.Now().Add(-20 * time.Minute)}
	old := cloud.NodeEvent{Source: autoRepairSource, Reason: "NodeRebootEnd", Time: time.Now().Add(-2 * time.Hour)}
	for _, tc := range []struct {
		name          string
		change        func(*cloud.NodeRef, *fakeAKS)
		known, active bool
	}{
		{"clear", func(*cloud.NodeRef, *fakeAKS) {}, true, false},
		{"old repair", func(r *cloud.NodeRef, _ *fakeAKS) { r.Events = []cloud.NodeEvent{old} }, true, false},
		{"auto-repair under way", func(r *cloud.NodeRef, _ *fakeAKS) { r.Events = []cloud.NodeEvent{recent} }, true, true},
		{"not ready", func(r *cloud.NodeRef, _ *fakeAKS) { r.Ready = false }, false, false},
		{"events unknown", func(r *cloud.NodeRef, _ *fakeAKS) { r.EventsKnown = false }, false, false},
		{"pool busy", func(_ *cloud.NodeRef, f *fakeAKS) { f.state = "Upgrading" }, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAKS(2)
			ref := testRef(0)
			tc.change(&ref, f)
			obs, err := testProvider(f).OpenRepair(t.Context(), ref)
			if err != nil || obs.Known != tc.known || obs.Active != tc.active {
				t.Fatalf("observation = %+v, %v; want known=%t active=%t", obs, err, tc.known, tc.active)
			}
			if tc.active && (len(obs.Evidence) != 1 || obs.Evidence[0].Detail != "NodeRebootStart") {
				t.Fatalf("evidence = %+v", obs.Evidence)
			}
		})
	}
}

func TestCheckReplacePermission(t *testing.T) {
	const pools = "Microsoft.ContainerService/managedClusters/agentPools/"
	for _, tc := range []struct {
		name  string
		perms []permission
		ok    bool
	}{
		{"exact actions", []permission{{Actions: replaceActions}}, true},
		{"wildcard", []permission{{Actions: []string{"Microsoft.ContainerService/*"}}}, true},
		{"wildcard in the middle", []permission{{Actions: []string{"microsoft.containerservice/managedclusters/*/read", pools + "write", pools + "deleteMachines/action"}}}, true},
		{"excluded by notActions", []permission{{Actions: []string{"*"}, NotActions: []string{pools + "deleteMachines/action"}}}, false},
		{"granted by another assignment", []permission{{Actions: []string{"*"}, NotActions: []string{pools + "write"}}, {Actions: []string{pools + "write"}}}, true},
		{"read only", []permission{{Actions: []string{"*/read"}}}, false},
		{"none", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAKS(1)
			f.permissions = tc.perms
			err := testProvider(f).CheckReplacePermission(t.Context(), testInstance(0))
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%t", err, tc.ok)
			}
		})
	}
}

func TestCapacityCodesReadInnerErrors(t *testing.T) {
	if !capacityError(armFailure("ScaleVMSSAgentPoolFailed", "ZonalAllocationFailed")) {
		t.Fatal("an inner allocation failure is capacity")
	}
	if capacityError(errors.New("QuotaExceeded")) {
		t.Fatal("only an ARM response carries an error code")
	}
}
