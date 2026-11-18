// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"errors"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/cloud/cloudtest"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

func TestGCPConformance(t *testing.T) {
	cloudtest.Conformance(t, cloudtest.Case{
		Provider: testProvider(newFakeGKE(1)), ProviderID: "gce://" + testInstance(0), Instance: testInstance(0),
		Foreign: []string{
			"aws:///us-east-1a/i-0abc",
			"gce://" + testProject + "/" + testZone,
			"gce://" + testInstance(0) + "/extra",
			"gce://" + testProject + "/us-central1/" + testName(testZone, 0),
		},
	})
}

func TestProviderRegisteredForReplacementOnly(t *testing.T) {
	caps, ok := cloud.DeclaredCapabilities(providerName)
	if !ok || caps.ReinitializeInPlace || !caps.Replace || !caps.AddFirst {
		t.Fatalf("gcp capabilities = %+v registered=%t, want replacement with add-first", caps, ok)
	}
	if got := testProvider(newFakeGKE(1)).Capabilities(); got != caps {
		t.Fatalf("Capabilities() = %+v, want %+v", got, caps)
	}
}

func TestFactoryRequiresClusterSettings(t *testing.T) {
	_, err := cloud.Build(t.Context(), providerName, cloud.Config{Options: map[string]string{OptionProject: testProject, OptionCluster: testCluster}})
	if err == nil || !strings.Contains(err.Error(), "location") {
		t.Fatalf("err = %v, want the missing settings named", err)
	}
}

func TestRecycleIsNeverViable(t *testing.T) {
	f := newFakeGKE(1)
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
	return cloud.NodeRef{
		Identity: cloud.NodeIdentity{Node: testName(testZone, i), NodeUID: "uid", ProviderID: "gce://" + testInstance(i), InstanceID: testInstance(i), BootID: "boot"},
		Ready:    true,
	}
}

func TestRepairPolicyFollowsThePoolAndCluster(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*fakeGKE)
		want  cloud.RepairMode
	}{
		"auto-repair on":  {func(*fakeGKE) {}, cloud.RepairNotReadyOnly},
		"auto-repair off": {func(f *fakeGKE) { f.autoRepair = false }, cloud.RepairOff},
		"autopilot":       {func(f *fakeGKE) { f.autopilot = true }, cloud.RepairOn},
	} {
		f := newFakeGKE(2)
		tc.setup(f)
		policy, err := testProvider(f).RepairPolicy(t.Context(), testRef(1))
		if err != nil || policy.Mode != tc.want {
			t.Fatalf("%s: policy = %+v, %v; want %s", name, policy, err, tc.want)
		}
	}
}

func TestRepairPolicyRefusesANodeOutsideThePool(t *testing.T) {
	wrongNode := testRef(1)
	wrongNode.Identity.Node = testName(testZone, 0)
	otherProject := testRef(1)
	otherProject.Identity.ProviderID = "gce://other-project/" + testZone + "/" + testName(testZone, 1)
	otherProject.Identity.InstanceID = "other-project/" + testZone + "/" + testName(testZone, 1)
	for name, tc := range map[string]struct {
		ref   cloud.NodeRef
		setup func(*fakeGKE)
	}{
		"other node name":       {wrongNode, func(*fakeGKE) {}},
		"not a member":          {testRef(5), func(*fakeGKE) {}},
		"other project":         {otherProject, func(*fakeGKE) {}},
		"other cluster's VM":    {testRef(1), func(f *fakeGKE) { f.labels = map[string]string{labelCluster: "other", labelNodePool: testPool} }},
		"VM without GKE labels": {testRef(1), func(f *fakeGKE) { f.labels = nil }},
		"unknown node pool":     {testRef(1), func(f *fakeGKE) { f.labels = map[string]string{labelCluster: testCluster, labelNodePool: "cpu"} }},
	} {
		f := newFakeGKE(2)
		tc.setup(f)
		if policy, err := testProvider(f).RepairPolicy(t.Context(), tc.ref); err == nil || policy.Mode != cloud.RepairUnknown {
			t.Fatalf("%s: policy = %+v, %v", name, policy, err)
		}
	}
}

func TestOpenRepairKnownOnlyForAQuietReadyNode(t *testing.T) {
	f := newFakeGKE(2)
	p := testProvider(f)
	obs, err := p.OpenRepair(t.Context(), testRef(1))
	if err != nil || !obs.Known || obs.Active {
		t.Fatalf("quiet pool: %+v, %v", obs, err)
	}
	// Operations finished long ago, or on other pools, change nothing.
	f.addOp(opAutoRepair, opDone, poolLink(testPool)+"/node/"+testName(testZone, 1))
	f.addOp("UPGRADE_NODES", "RUNNING", poolLink("system"))
	if obs, err := p.OpenRepair(t.Context(), testRef(1)); err != nil || !obs.Known || obs.Active {
		t.Fatalf("done or foreign operations: %+v, %v", obs, err)
	}

	notReady := testRef(1)
	notReady.Ready = false
	if obs, _ := p.OpenRepair(t.Context(), notReady); obs.Known {
		t.Fatalf("NotReady node: %+v", obs)
	}
}

func TestOpenRepairSeesAnAutoRepairOfThePoolOrCluster(t *testing.T) {
	for name, target := range map[string]string{
		"node":    poolLink(testPool) + "/node/" + testName(testZone, 0),
		"pool":    poolLink(testPool),
		"cluster": strings.TrimSuffix(poolLink(testPool), "/nodePools/"+testPool),
	} {
		f := newFakeGKE(2)
		f.addOp(opAutoRepair, "RUNNING", target)
		obs, err := testProvider(f).OpenRepair(t.Context(), testRef(1))
		if err != nil || !obs.Known || !obs.Active || len(obs.Evidence) != 1 {
			t.Fatalf("%s: %+v, %v; want an active repair", name, obs, err)
		}
	}
}

func TestOpenRepairIsUnknownWhileThePoolChanges(t *testing.T) {
	f := newFakeGKE(2)
	f.addOp("UPGRADE_NODES", "RUNNING", poolLink(testPool))
	obs, err := testProvider(f).OpenRepair(t.Context(), testRef(1))
	if err != nil || obs.Known || !strings.Contains(obs.Reason, "UPGRADE_NODES") {
		t.Fatalf("%+v, %v", obs, err)
	}
}

// Host maintenance is recorded, and no recovery starts.
func TestOpenRepairHoldsForHostMaintenance(t *testing.T) {
	labelled := testRef(1)
	labelled.Labels = map[string]string{maintenanceLabel: "true"}
	f := newFakeGKE(2)
	if obs, err := testProvider(f).OpenRepair(t.Context(), labelled); err != nil || !obs.Active || obs.Evidence[0].Source != "gce-host-maintenance" {
		t.Fatalf("labelled node: %+v, %v", obs, err)
	}
	f.maintenance = "SCHEDULED 2026-11-20T03:00:00Z"
	if obs, err := testProvider(f).OpenRepair(t.Context(), testRef(1)); err != nil || !obs.Active || !strings.Contains(obs.Evidence[0].Detail, "SCHEDULED") {
		t.Fatalf("upcoming maintenance: %+v, %v", obs, err)
	}
}

func TestCheckReplacePermissionNamesWhatIsMissing(t *testing.T) {
	f := newFakeGKE(2)
	p := testProvider(f)
	if err := p.CheckReplacePermission(t.Context(), testInstance(0)); err != nil {
		t.Fatal(err)
	}
	f.permissions = basePermissions
	if err := p.CheckReplacePermission(t.Context(), testInstance(0)); err == nil || !strings.Contains(err.Error(), "container.clusters.update") {
		t.Fatalf("fixed pool without update: %v", err)
	}
	f.autoscaling = true
	if err := p.CheckReplacePermission(t.Context(), testInstance(0)); err != nil {
		t.Fatalf("autoscaled pool needs no update: %v", err)
	}
}

// A multi-zone pool whose zones differ in size is refused before the node is
// cordoned.
func TestCheckReplacePermissionRefusesUnevenZones(t *testing.T) {
	f := newFakeGKE(2, testZone, "us-central1-b")
	f.groups[1].target = 3
	if err := testProvider(f).CheckReplacePermission(t.Context(), testInstance(0)); err == nil || !strings.Contains(err.Error(), "differ in size") {
		t.Fatalf("err = %v", err)
	}
}

// Without credentials the provider is still built, and every call names why.
func TestFactoryWithoutCredentialsRefusesWithTheCause(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", t.TempDir()+"/missing.json")
	p, err := cloud.Build(t.Context(), providerName, cloud.Config{Options: map[string]string{OptionProject: testProject, OptionLocation: testLocation, OptionCluster: testCluster}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Replace(t.Context(), testInstance(0)); err == nil || !strings.Contains(err.Error(), "Application Default Credentials") {
		t.Fatalf("Replace = %v, want the missing credentials named", err)
	}
}
