// SPDX-License-Identifier: LicenseRef-KubeNeuron

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/internal/operations"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/internal/topology"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// topoInventoryPlatform lists node-a and NOTHING optional: it stands in for a
// platform that cannot observe node resources at all, which the adapter must
// record as unobserved — never as "a platform with no partitions".
type topoInventoryPlatform struct {
	basePlatform
	labels map[string]string
}

func (p *topoInventoryPlatform) ListNodes(context.Context) ([]types.Node, error) {
	labels := map[string]string{"accelerator": "nvidia-h100"}
	for key, value := range p.labels {
		labels[key] = value
	}
	return []types.Node{{Name: "node-a", UID: "node-uid-a", Labels: labels}}, nil
}

// topoObservingPlatform adds platform.NodeResourceObserver with a scripted
// answer.
type topoObservingPlatform struct {
	topoInventoryPlatform
	resources func(node string) (topology.Resources, bool, error)
}

func (p *topoObservingPlatform) NodeResources(_ context.Context, node string) (topology.Resources, bool, error) {
	return p.resources(node)
}

// topoDRAPlatform adds platform.DRAObserver on top.
type topoDRAPlatform struct {
	topoObservingPlatform
	observe func(node string) topology.DRAObservation
}

func (p *topoDRAPlatform) ObserveDRA(_ context.Context, node string) topology.DRAObservation {
	return p.observe(node)
}

var (
	_ platform.NodeResourceObserver = (*topoObservingPlatform)(nil)
	_ platform.DRAObserver          = (*topoDRAPlatform)(nil)
	_ platform.NodeResourceObserver = (*stackPlatform)(nil)
)

func topoNodeResources(capacity map[string]int64) func(string) (topology.Resources, bool, error) {
	return func(node string) (topology.Resources, bool, error) {
		if node != "node-a" {
			return topology.Resources{}, false, nil
		}
		return topology.Resources{Source: topology.ResourceSourceNode, Capacity: capacity, Allocatable: capacity}, true, nil
	}
}

func topoWholeGPU() map[string]int64 { return map[string]int64{"nvidia.com/gpu": 1} }

// wholeDeviceTopologyPlatform is the fixture every pre-existing reset-gate
// test needs now that the adapter fails closed without an observer: node-a
// advertises one whole GPU and nothing else.
func wholeDeviceTopologyPlatform() *topoObservingPlatform {
	return &topoObservingPlatform{resources: topoNodeResources(topoWholeGPU())}
}

func observingPlatform(labels map[string]string, capacity map[string]int64) *topoObservingPlatform {
	return &topoObservingPlatform{topoInventoryPlatform: topoInventoryPlatform{labels: labels}, resources: topoNodeResources(capacity)}
}

func draObservingPlatform(observe func(string) topology.DRAObservation) *topoDRAPlatform {
	return &topoDRAPlatform{topoObservingPlatform: *wholeDeviceTopologyPlatform(), observe: observe}
}

// NodeResources lets the quiesce fixture's platform observe node-a's
// advertisement, so the narrowing step sees a whole device there too.
func (p *stackPlatform) NodeResources(_ context.Context, node string) (topology.Resources, bool, error) {
	return topoNodeResources(topoWholeGPU())(node)
}

// topologyGateController builds a controller whose legacy reset evidence
// PASSES for node-a/GPU-a: a selecting profile, a fresh ready report bound to
// the node's UID, the device in the inventory. Everything a test then sees is
// the classifier's narrowing step and nothing else.
func topologyGateController(t *testing.T, p platform.Platform, dra bool, mutateReport func(*types.AgentAcceleratorReport)) *Controller {
	t.Helper()
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid-a", Labels: map[string]string{"accelerator": "nvidia-h100"}}); err != nil {
		t.Fatal(err)
	}
	c := New(st, nil, nil, nil, nil, p, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if dra {
		if err := c.InstallRuntimeConfig(RuntimeConfig{Topology: TopologyPolicy{DRADiscovery: true}}); err != nil {
			t.Fatal(err)
		}
	}
	profile := testNVIDIAResetProfile()
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{profile}); err != nil {
		t.Fatal(err)
	}
	report := readyNVIDIAResetReport(time.Now().UTC(), profile.ProfileDigest)
	if mutateReport != nil {
		mutateReport(&report)
	}
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}
	return c
}

func assertTopologyRefusal(t *testing.T, err error, code topology.ReasonCode) {
	t.Helper()
	if !errors.Is(err, errTopologyRefused) {
		t.Fatalf("err = %v, want the topology refusal", err)
	}
	if !strings.Contains(err.Error(), string(code)) || !strings.Contains(err.Error(), "inputs_digest=sha256:") {
		t.Fatalf("refusal %q must name %s and carry the inputs digest", err, code)
	}
	for _, raw := range []string{"token", "secret"} {
		if strings.Contains(err.Error(), raw) {
			t.Fatalf("refusal %q leaks raw evidence", err)
		}
	}
}

// TestResetGateRefusesUnqualifiedTopologyAfterLegacyEvidencePasses is the
// live gate, end to end: the legacy evidence test passes for every case, and
// the classifier alone turns MIG, time-slicing, DRA, an unobservable platform
// and an invalid observation into a refusal that names its reason. The one
// admitted shape is a whole device the platform explicitly observed.
func TestResetGateRefusesUnqualifiedTopologyAfterLegacyEvidencePasses(t *testing.T) {
	target := types.Target{Node: "node-a", GPUUUID: "GPU-a"}
	observed := func(slices int, version string) func(string) topology.DRAObservation {
		return func(string) topology.DRAObservation {
			o := topology.DRAObservation{State: topology.DRAObserved, APIVersion: version, ResourceSlices: slices, Devices: slices * 8, PodsInspected: true}
			if slices > 0 {
				o.Drivers = []string{"gpu.nvidia.com"}
			}
			return o
		}
	}
	claimed := func(direct, template int) func(string) topology.DRAObservation {
		return func(string) topology.DRAObservation {
			return topology.DRAObservation{State: topology.DRAObserved, APIVersion: topology.DRAAPIVersion, PodsInspected: true, ClaimPods: 1, DirectClaims: direct, TemplateClaims: template}
		}
	}
	// Partition evidence from the agent report itself (a partitioned safety or
	// a partition device) is not in this table: the report validation in
	// pkg/types refuses to store such a report beside a physical reset
	// capability, so it never reaches the classifier through this gate. The
	// classifier's own table covers it; here the partition evidence is the
	// node's nvidia.com/mig-* advertisement.
	cases := []struct {
		name     string
		platform platform.Platform
		dra      bool
		want     topology.ReasonCode // empty: admitted
	}{
		{name: "whole device observed on the node is admitted", platform: wholeDeviceTopologyPlatform()},
		{name: "mig instances advertised", platform: observingPlatform(nil, map[string]int64{"nvidia.com/gpu": 1, "nvidia.com/mig-1g.5gb": 7}), want: topology.ReasonMigPartitioned},
		// The GPU Operator defaults mig.strategy=single even with
		// mig.config=all-disabled: the label alone is an advertisement mode,
		// not a partition, so a verified unpartitioned whole GPU stays admitted.
		{name: "mig.strategy single label beside a verified report is admitted", platform: observingPlatform(map[string]string{topology.LabelMIGStrategy: "single"}, topoWholeGPU())},
		{name: "mig.strategy mixed label beside a verified report is admitted", platform: observingPlatform(map[string]string{topology.LabelMIGStrategy: "mixed"}, topoWholeGPU())},
		{name: "mig.strategy single with mig instances advertised", platform: observingPlatform(map[string]string{topology.LabelMIGStrategy: "single"}, map[string]int64{"nvidia.com/gpu": 1, "nvidia.com/mig-1g.5gb": 7}), want: topology.ReasonMigPartitioned},
		{name: "mig.strategy mixed with mig instances advertised", platform: observingPlatform(map[string]string{topology.LabelMIGStrategy: "mixed"}, map[string]int64{"nvidia.com/gpu": 1, "nvidia.com/mig-1g.5gb": 7}), want: topology.ReasonMigPartitioned},
		{name: "unrecognised mig.strategy label", platform: observingPlatform(map[string]string{topology.LabelMIGStrategy: "auto"}, topoWholeGPU()), want: topology.ReasonMigStrategyLabelInvalid},
		{name: "empty mig.strategy label", platform: observingPlatform(map[string]string{topology.LabelMIGStrategy: ""}, topoWholeGPU()), want: topology.ReasonMigStrategyLabelInvalid},
		{name: "mig instances advertised past the bound", platform: observingPlatform(nil, map[string]int64{"nvidia.com/gpu": 1, "nvidia.com/mig-1g.5gb": topology.MaxAdvertisedDevices + 1}), want: topology.ReasonMigPartitioned},
		{name: "gpu.shared advertised past the bound", platform: observingPlatform(nil, map[string]int64{"nvidia.com/gpu.shared": topology.MaxAdvertisedDevices + 1}), want: topology.ReasonTimeSliced},
		{name: "time-sliced via gpu.replicas", platform: observingPlatform(map[string]string{topology.LabelGPUReplicas: "4"}, map[string]int64{"nvidia.com/gpu": 4}), want: topology.ReasonTimeSliced},
		// Time-slicing without renaming on a node without GFD labels: the
		// plugin advertises physical x replicas under nvidia.com/gpu and
		// nothing else says so. Four advertised against the one physical GPU
		// the report attests is refused as a count mismatch, not admitted
		// as whole-device and not guessed to be time-sliced.
		{name: "whole-GPU advertisement above the one attested physical GPU without GFD labels", platform: observingPlatform(nil, map[string]int64{"nvidia.com/gpu": 4}), want: topology.ReasonWholeDeviceCountMismatch},
		{name: "whole-GPU advertisement above the attested count beside benign GFD labels", platform: observingPlatform(map[string]string{topology.LabelMIGStrategy: "none", topology.LabelMIGCapable: "false"}, map[string]int64{"nvidia.com/gpu": 2}), want: topology.ReasonWholeDeviceCountMismatch},
		{name: "time-sliced via gpu.shared", platform: observingPlatform(nil, map[string]int64{"nvidia.com/gpu.shared": 2}), want: topology.ReasonTimeSliced},
		{name: "time-sliced via sharing-strategy", platform: observingPlatform(map[string]string{topology.LabelSharingStrategy: "time-slicing"}, topoWholeGPU()), want: topology.ReasonTimeSliced},
		{name: "mps sharing is unknown", platform: observingPlatform(map[string]string{topology.LabelSharingStrategy: "mps"}, topoWholeGPU()), want: topology.ReasonTopologyUnknown},
		{name: "malformed gpu.replicas", platform: observingPlatform(map[string]string{topology.LabelGPUReplicas: "many"}, topoWholeGPU()), want: topology.ReasonReplicasLabelInvalid},
		{name: "dra enabled and slices observed for the node", platform: draObservingPlatform(observed(1, topology.DRAAPIVersion)), dra: true, want: topology.ReasonDraManaged},
		{name: "dra enabled and nothing observed for the node is admitted", platform: draObservingPlatform(observed(0, topology.DRAAPIVersion)), dra: true},
		{name: "dra enabled and a direct claim reference on a pod with zero slices", platform: draObservingPlatform(claimed(1, 0)), dra: true, want: topology.ReasonDraManaged},
		{name: "dra enabled and a template claim reference on a pod with zero slices", platform: draObservingPlatform(claimed(0, 1)), dra: true, want: topology.ReasonDraManaged},
		{name: "dra enabled and the observer read slices without inspecting pods", platform: draObservingPlatform(func(string) topology.DRAObservation {
			return topology.DRAObservation{State: topology.DRAObserved, APIVersion: topology.DRAAPIVersion}
		}), dra: true, want: topology.ReasonDraObservationInvalid},
		{name: "dra enabled and the pod list could not be taken", platform: draObservingPlatform(func(string) topology.DRAObservation {
			return topology.DRAObservation{State: topology.DRANotObserved, Reason: "pods-list-failed", APIVersion: topology.DRAAPIVersion}
		}), dra: true, want: topology.ReasonTopologyUnknown},
		{name: "dra enabled but the platform cannot observe", platform: wholeDeviceTopologyPlatform(), dra: true, want: topology.ReasonTopologyUnknown},
		{name: "dra enabled but not observed", platform: draObservingPlatform(func(string) topology.DRAObservation {
			return topology.DRAObservation{State: topology.DRANotObserved, Reason: "forbidden"}
		}), dra: true, want: topology.ReasonTopologyUnknown},
		{name: "dra enabled and the observer answers with no state", platform: draObservingPlatform(func(string) topology.DRAObservation { return topology.DRAObservation{} }), dra: true, want: topology.ReasonTopologyUnknown},
		{name: "dra enabled and observed from a beta version", platform: draObservingPlatform(observed(1, "resource.k8s.io/v1beta1")), dra: true, want: topology.ReasonDraObservationInvalid},
		{name: "dra disabled never consults the observer", platform: draObservingPlatform(func(string) topology.DRAObservation {
			panic("DRA observed while discovery is disabled")
		})},
		{name: "platform without a resource observer", platform: &topoInventoryPlatform{}, want: topology.ReasonTopologyUnknown},
		{name: "node resources unreadable", platform: &topoObservingPlatform{resources: func(string) (topology.Resources, bool, error) {
			return topology.Resources{}, false, fmt.Errorf("apiserver 503")
		}}, want: topology.ReasonTopologyUnknown},
		{name: "node resources not found", platform: &topoObservingPlatform{resources: func(string) (topology.Resources, bool, error) {
			return topology.Resources{}, false, nil
		}}, want: topology.ReasonTopologyUnknown},
		{name: "observer answers without a source", platform: &topoObservingPlatform{resources: func(string) (topology.Resources, bool, error) {
			return topology.Resources{Capacity: topoWholeGPU()}, true, nil
		}}, want: topology.ReasonTopologyUnknown},
		{name: "observer answers from a source outside the closed set", platform: &topoObservingPlatform{resources: func(string) (topology.Resources, bool, error) {
			return topology.Resources{Source: "cache", Capacity: topoWholeGPU()}, true, nil
		}}, want: topology.ReasonTopologyUnknown},
		{name: "observer explicitly answers not-applicable and the attestation carries it", platform: &topoObservingPlatform{resources: func(string) (topology.Resources, bool, error) {
			return topology.Resources{Source: topology.ResourceSourceNotApplicable}, true, nil
		}}},
		{name: "observer answers no NVIDIA advertisement at all", platform: observingPlatform(nil, map[string]int64{"amd.com/gpu": 1}), want: topology.ReasonTopologyUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := topologyGateController(t, tc.platform, tc.dra, nil)
			err := c.allowNVIDIAReset(context.Background(), &types.Incident{ID: "inc-topology"}, target)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("whole-device reset refused: %v", err)
				}
				return
			}
			assertTopologyRefusal(t, err, tc.want)
			// The quiesce step pins evidence through the same function, so a
			// refusal here is also what keeps monitoring from being stood down
			// for a reset the topology forbids.
			if _, _, _, err := c.acceleratorEvidenceForReset(context.Background(), target); !errors.Is(err, errTopologyRefused) {
				t.Fatalf("acceleratorEvidenceForReset = %v, want the same topology refusal", err)
			}
		})
	}
}

// TestResetGateCountMismatchFollowsTheAttestedInventory: the count test
// compares the advertisement with the report that passed the legacy gate.
// Two attested physical GPUs under nvidia.com/gpu: 2 are admitted; the same
// advertisement against one attested GPU, or one advertised against two
// attested, is refused with the count mismatch and never reaches the
// actuator through the quiesce path either.
func TestResetGateCountMismatchFollowsTheAttestedInventory(t *testing.T) {
	target := types.Target{Node: "node-a", GPUUUID: "GPU-a"}
	twoPhysical := func(report *types.AgentAcceleratorReport) {
		report.Devices = append(report.Devices, types.AgentAcceleratorDevice{
			ID: "GPU-b", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU,
		})
	}
	cases := []struct {
		name       string
		advertised int64
		report     func(*types.AgentAcceleratorReport)
		want       topology.ReasonCode // empty: admitted
	}{
		{name: "two advertised, two attested is admitted", advertised: 2, report: twoPhysical},
		{name: "two advertised, one attested is refused", advertised: 2, want: topology.ReasonWholeDeviceCountMismatch},
		{name: "one advertised, two attested is refused", advertised: 1, report: twoPhysical, want: topology.ReasonWholeDeviceCountMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := topologyGateController(t, observingPlatform(nil, map[string]int64{"nvidia.com/gpu": tc.advertised}), false, tc.report)
			err := c.allowNVIDIAReset(context.Background(), &types.Incident{ID: "inc-topology-count"}, target)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("whole-device reset with agreeing counts refused: %v", err)
				}
				return
			}
			assertTopologyRefusal(t, err, tc.want)
			if strings.Contains(err.Error(), string(topology.ReasonTimeSliced)) {
				t.Fatalf("count mismatch %q must not be inferred as time-slicing", err)
			}
			if _, _, _, err := c.acceleratorEvidenceForReset(context.Background(), target); !errors.Is(err, errTopologyRefused) {
				t.Fatalf("acceleratorEvidenceForReset = %v, want the same topology refusal", err)
			}
		})
	}
}

// TestPinnedResetEvidenceDoesNotBypassTheTopologyClassifier: evidence pinned
// by a quiesce is judged by the classifier exactly like live evidence.
func TestPinnedResetEvidenceDoesNotBypassTheTopologyClassifier(t *testing.T) {
	target := types.Target{Node: "node-a", GPUUUID: "GPU-a"}
	c := topologyGateController(t, observingPlatform(nil, map[string]int64{"nvidia.com/gpu": 1, "nvidia.com/mig-1g.5gb": 7}), false, nil)
	report := readyNVIDIAResetReport(time.Now().UTC(), testNVIDIAResetProfile().ProfileDigest)
	c.pinAcceleratorEvidence("inc-pinned", pinnedAcceleratorEvidence{node: "node-a", report: report, pinnedAt: time.Now()})
	err := c.allowNVIDIAReset(context.Background(), &types.Incident{ID: "inc-pinned"}, target)
	assertTopologyRefusal(t, err, topology.ReasonMigPartitioned)

	// And a pin on a whole-device node is still admitted: the classifier
	// narrows, it does not break the quiesce contract.
	admitted := topologyGateController(t, wholeDeviceTopologyPlatform(), false, nil)
	admitted.pinAcceleratorEvidence("inc-pinned", pinnedAcceleratorEvidence{node: "node-a", report: report, pinnedAt: time.Now()})
	if err := admitted.allowNVIDIAReset(context.Background(), &types.Incident{ID: "inc-pinned"}, target); err != nil {
		t.Fatalf("pinned whole-device reset refused: %v", err)
	}
}

// TestLegacyResetDenialsStayDeniedUnderEveryTopology is the narrowing
// property at the gate: every case the legacy evidence test refused is still
// refused under every platform shape the classifier can see, and it is the
// LEGACY error that is returned — the classifier runs after it and can never
// turn a legacy refusal into anything else.
func TestLegacyResetDenialsStayDeniedUnderEveryTopology(t *testing.T) {
	target := types.Target{Node: "node-a", GPUUUID: "GPU-a"}
	// Each mutation is a shape the report store accepts (it validates on
	// write) and the legacy evidence test refuses.
	verifyOnly := []types.AgentAcceleratorCapability{{
		Action: types.AcceleratorActionVerifyHealth,
		Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopeNode},
	}}
	legacy := map[string]func(*types.AgentAcceleratorReport){
		"stale": func(r *types.AgentAcceleratorReport) { r.ObservedAt = time.Now().Add(-10 * time.Minute).UTC() },
		"partitioned": func(r *types.AgentAcceleratorReport) {
			r.TopologySafety = types.AcceleratorTopologyPartitioned
			r.Capabilities = verifyOnly
		},
		"topology unknown": func(r *types.AgentAcceleratorReport) {
			r.TopologySafety = types.AcceleratorTopologyUnknown
			r.Capabilities = verifyOnly
		},
		"degraded": func(r *types.AgentAcceleratorReport) {
			r.Readiness = types.AcceleratorReadinessDegraded
			r.ReadinessReasons = []string{"xid-79"}
		},
		"profile generation":  func(r *types.AgentAcceleratorReport) { r.ProfileGeneration = 2 },
		"profile digest":      func(r *types.AgentAcceleratorReport) { r.ProfileDigest = "sha256:" + strings.Repeat("b", 64) },
		"driver version":      func(r *types.AgentAcceleratorReport) { r.DriverVersion = "550.54.15" },
		"node UID":            func(r *types.AgentAcceleratorReport) { r.NodeUID = "node-uid-recreated" },
		"device absent":       func(r *types.AgentAcceleratorReport) { r.Devices[0].ID = "GPU-other" },
		"no reset capability": func(r *types.AgentAcceleratorReport) { r.Capabilities = nil },
		"no physical device":  func(r *types.AgentAcceleratorReport) { r.Devices = nil },
		"capability scope is node": func(r *types.AgentAcceleratorReport) {
			r.Capabilities[0].Scopes = []types.AcceleratorTargetScope{types.AcceleratorScopeNode}
		},
	}
	platforms := map[string]func() platform.Platform{
		"whole-device": func() platform.Platform { return wholeDeviceTopologyPlatform() },
		"mig":          func() platform.Platform { return observingPlatform(nil, map[string]int64{"nvidia.com/mig-1g.5gb": 7}) },
		"time-sliced": func() platform.Platform {
			return observingPlatform(map[string]string{topology.LabelGPUReplicas: "4"}, map[string]int64{"nvidia.com/gpu": 4})
		},
		"not-applicable": func() platform.Platform {
			return &topoObservingPlatform{resources: func(string) (topology.Resources, bool, error) {
				return topology.Resources{Source: topology.ResourceSourceNotApplicable}, true, nil
			}}
		},
		"unobservable": func() platform.Platform { return &topoInventoryPlatform{} },
	}
	for legacyName, mutate := range legacy {
		for platformName, newPlatform := range platforms {
			t.Run(legacyName+"/"+platformName, func(t *testing.T) {
				c := topologyGateController(t, newPlatform(), false, mutate)
				err := c.allowNVIDIAReset(context.Background(), &types.Incident{ID: "inc-legacy"}, target)
				if err == nil {
					t.Fatal("a legacy-denied reset was admitted")
				}
				if errors.Is(err, errTopologyRefused) {
					t.Fatalf("err = %v: the legacy evidence test must refuse before the classifier is consulted", err)
				}
			})
		}
	}
	// Without a selecting profile the default-deny is unchanged on every platform.
	for platformName, newPlatform := range platforms {
		st, err := storesqlite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		c := New(st, nil, nil, nil, nil, newPlatform(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := c.allowNVIDIAReset(context.Background(), &types.Incident{ID: "inc-noprofile"}, target); !errors.Is(err, config.ErrNoAcceleratorRuntimeProfile) {
			t.Fatalf("%s without profile = %v, want ErrNoAcceleratorRuntimeProfile", platformName, err)
		}
	}
}

// TestTopologyIsCapturedOnlyForDeviceScopedResetSnapshots pins the
// node-scoped contract: a reboot, a node-scoped reset and an observe-class
// request carry no classification at all, so their evaluation is exactly what
// it was; only a physical-device reset carries one.
func TestTopologyIsCapturedOnlyForDeviceScopedResetSnapshots(t *testing.T) {
	c := topologyGateController(t, observingPlatform(nil, map[string]int64{"nvidia.com/gpu": 1, "nvidia.com/mig-1g.5gb": 7}), false, nil)
	ctx := context.Background()
	for name, request := range map[string]decision.Request{
		"observe":           {Class: decision.ActionObserve},
		"diagnostic":        {Class: decision.ActionDiagnostic},
		"node reboot":       {Class: decision.ActionRemediate, AcceleratorAction: types.AcceleratorActionRebootNode, Scope: types.AcceleratorScopeNode},
		"node-scoped reset": {Class: decision.ActionRemediate, AcceleratorAction: types.AcceleratorActionResetDevice, Scope: types.AcceleratorScopeNode},
	} {
		snapshot, err := c.BuildDecisionSnapshot(ctx, "node-a", request)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if snapshot.Topology != nil {
			t.Fatalf("%s: node-scoped or observe-class snapshot carries a topology classification: %s", name, snapshot.Topology.Summary)
		}
	}
	snapshot, err := c.BuildDecisionSnapshot(ctx, "node-a", decision.Request{
		Class: decision.ActionRemediate, AcceleratorAction: types.AcceleratorActionResetDevice,
		Scope: types.AcceleratorScopePhysicalDevice, TargetDeviceID: "GPU-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Topology == nil || snapshot.Topology.Eligible() || !snapshot.Topology.Has(topology.ReasonMigPartitioned) {
		t.Fatalf("device-scoped snapshot topology = %+v, want a MIG refusal", snapshot.Topology)
	}
	if snapshot.Topology.Version != topology.Version || snapshot.Topology.Node != "node-a" || snapshot.Topology.NodeUID != "node-uid-a" {
		t.Fatalf("classification identity = %+v", snapshot.Topology)
	}
}

// TestBuildTopologyClassificationFailsClosedWithoutAnObserver: the read-only
// API answers "unknown, refused" for a platform that cannot observe, and the
// same for an explicit unobserved answer; it does not invent not-applicable.
func TestBuildTopologyClassificationFailsClosedWithoutAnObserver(t *testing.T) {
	ctx := context.Background()
	for name, p := range map[string]platform.Platform{
		"no observer": &topoInventoryPlatform{},
		"read error":  &topoObservingPlatform{resources: func(string) (topology.Resources, bool, error) { return topology.Resources{}, false, errors.New("boom") }},
		"not found":   &topoObservingPlatform{resources: func(string) (topology.Resources, bool, error) { return topology.Resources{}, false, nil }},
		"sourceless": &topoObservingPlatform{resources: func(string) (topology.Resources, bool, error) {
			return topology.Resources{Capacity: topoWholeGPU()}, true, nil
		}},
		"unknown source": &topoObservingPlatform{resources: func(string) (topology.Resources, bool, error) {
			return topology.Resources{Source: "cache", Capacity: topoWholeGPU()}, true, nil
		}},
	} {
		c := topologyGateController(t, p, false, nil)
		classification, err := c.BuildTopologyClassification(ctx, "node-a")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if classification.Topology != topology.TopologyUnknown || classification.Eligible() || classification.Evidence.ResourceSource != topology.ResourceSourceUnobserved {
			t.Fatalf("%s: classification = %s evidence=%+v, want unknown/refused/unobserved", name, classification.Summary, classification.Evidence)
		}
	}
	// A nil platform is the same answer: the store-only controller cannot
	// observe a device-plugin advertisement and must not pretend to.
	c := topologyGateController(t, nil, false, nil)
	classification, err := c.BuildTopologyClassification(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if classification.Topology != topology.TopologyUnknown || classification.Eligible() {
		t.Fatalf("nil platform classification = %s, want unknown/refused", classification.Summary)
	}
	// The whole-device platform answers eligible through the same API.
	c = topologyGateController(t, wholeDeviceTopologyPlatform(), false, nil)
	classification, err = c.BuildTopologyClassification(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if !classification.Eligible() || classification.Evidence.ResourceSource != topology.ResourceSourceNode {
		t.Fatalf("whole-device classification = %s", classification.Summary)
	}
}

// TestBuildTopologyClassificationWithoutReportStoreIsUnavailable pins the
// API contract that was previously only stated in a comment: a workflow store
// that cannot retain accelerator reports (reportlessStore, defined beside the
// runtime-contract tests) is operations.ErrUnavailable (503), never a
// classification with ReportMissing. "This store has no report for the node"
// and "this store cannot hold reports" are different facts, and only the
// first is evidence. Capability is checked before any read, so an unknown
// node answers the same way and the failure cannot be mistaken for a per-node
// condition. The matching case follows: a store that does retain reports and
// simply has none for the node classifies ordinarily as ReportMissing.
func TestBuildTopologyClassificationWithoutReportStoreIsUnavailable(t *testing.T) {
	ctx := context.Background()
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid-a", Labels: map[string]string{"accelerator": "nvidia-h100"}}); err != nil {
		t.Fatal(err)
	}
	report := readyNVIDIAResetReport(time.Now().UTC(), testNVIDIAResetProfile().ProfileDigest)
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// The report is retained in the underlying store; hiding the capability
	// must still refuse to classify rather than read around it.
	c := New(reportlessStore{st}, nil, nil, nil, nil, wholeDeviceTopologyPlatform(), nil, nil, log)
	got, err := c.BuildTopologyClassification(ctx, "node-a")
	if !errors.Is(err, operations.ErrUnavailable) || !strings.Contains(err.Error(), "accelerator report store") {
		t.Fatalf("reportless store error = %v, want ErrUnavailable naming the report store", err)
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reportless store error = %v must not look like an unknown node", err)
	}
	if got.Topology != "" || got.DeviceActions != "" || len(got.Reasons) != 0 {
		t.Fatalf("reportless store returned a classification %+v, want none", got)
	}
	if _, err := c.BuildTopologyClassification(ctx, "ghost"); !errors.Is(err, operations.ErrUnavailable) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reportless unknown node error = %v, want ErrUnavailable and not ErrNotFound", err)
	}

	// The same store with its capability visible classifies the node; this
	// proves the refusal above came from the hidden capability, not the data.
	c = New(st, nil, nil, nil, nil, wholeDeviceTopologyPlatform(), nil, nil, log)
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{testNVIDIAResetProfile()}); err != nil {
		t.Fatal(err)
	}
	if got, err = c.BuildTopologyClassification(ctx, "node-a"); err != nil || !got.Eligible() {
		t.Fatalf("report-capable store classification = %s, %v; want eligible", got.Summary, err)
	}
}

// TestBuildTopologyClassificationMissingReportIsReportMissingNotAnError: a
// store that CAN retain reports and has none for the node is ordinary
// evidence. The classification is unknown and refused with ReportMissing, and
// the API answers 200 with it, not 503.
func TestBuildTopologyClassificationMissingReportIsReportMissingNotAnError(t *testing.T) {
	ctx := context.Background()
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid-a", Labels: map[string]string{"accelerator": "nvidia-h100"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAcceleratorReport(ctx, "node-a", types.AcceleratorVendorNVIDIA); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fixture: report lookup = %v, want ErrNotFound", err)
	}
	c := New(st, nil, nil, nil, nil, wholeDeviceTopologyPlatform(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := c.BuildTopologyClassification(ctx, "node-a")
	if err != nil {
		t.Fatalf("missing report must classify, got error %v", err)
	}
	if got.Topology != topology.TopologyUnknown || got.Eligible() || !got.Has(topology.ReasonReportMissing) {
		t.Fatalf("missing report classification = %s reasons=%v, want unknown/refused/ReportMissing", got.Summary, got.Reasons)
	}
	// An unknown node on the same store stays the 404-shaped error.
	if _, err := c.BuildTopologyClassification(ctx, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown node error = %v, want ErrNotFound", err)
	}
}
