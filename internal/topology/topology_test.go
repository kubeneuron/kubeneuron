package topology

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var now = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func freshVerified() *Report {
	return &Report{
		ObservedAt:      now.Add(-time.Minute),
		TopologySafety:  types.AcceleratorTopologyVerifiedUnpartitioned,
		Readiness:       types.AcceleratorReadinessReady,
		PhysicalDevices: 1,
	}
}

// wholeDeviceSnapshot is the ONE shape that is eligible: a node advertising
// a whole GPU, GFD saying no MIG strategy is in force, a fresh, ready,
// verified-unpartitioned report with a physical device, and DRA off.
func wholeDeviceSnapshot() Snapshot {
	return Snapshot{
		Node:        "gpu-a",
		NodeUID:     "uid-a",
		EvaluatedAt: now,
		Resources: Resources{
			Source:      ResourceSourceNode,
			Capacity:    map[string]int64{ResourceWholeGPU: 1, "cpu": 4},
			Allocatable: map[string]int64{ResourceWholeGPU: 1, "cpu": 4},
		},
		Labels:       map[string]string{LabelMIGStrategy: "none", LabelMIGCapable: "false"},
		Report:       freshVerified(),
		MaxReportAge: 5 * time.Minute,
		DRA:          DRAObservation{State: DRADisabled},
	}
}

// observedDRA is a complete v1 observation: slices for the node, and the
// node's Pods inspected with no claim reference on them.
func observedDRA(slices, devices int) DRAObservation {
	return DRAObservation{State: DRAObserved, APIVersion: DRAAPIVersion, ResourceSlices: slices, Devices: devices, Drivers: []string{"gpu.nvidia.com"}, PodsInspected: true}
}

// emptyDRA is a complete v1 observation with nothing for the node: no slice
// published for it and no Pod on it referencing a claim. The only observed
// shape that changes nothing.
func emptyDRA() DRAObservation {
	return DRAObservation{State: DRAObserved, APIVersion: DRAAPIVersion, PodsInspected: true}
}

// claimedDRA is a complete v1 observation whose Pods reference claims.
func claimedDRA(slices, pods, direct, template int) DRAObservation {
	o := emptyDRA()
	o.ResourceSlices = slices
	o.Devices = slices * 8
	if slices > 0 {
		o.Drivers = []string{"gpu.nvidia.com"}
	}
	o.ClaimPods, o.DirectClaims, o.TemplateClaims = pods, direct, template
	return o
}

func TestClassifyWholeDeviceIsTheOnlyEligibleTopology(t *testing.T) {
	c := Classify(wholeDeviceSnapshot())
	if c.Version != Version || c.Node != "gpu-a" || c.NodeUID != "uid-a" {
		t.Fatalf("identity = %q %q %q", c.Version, c.Node, c.NodeUID)
	}
	if c.Topology != TopologyWholeDevice || c.DeviceActions != DeviceActionsEligible || !c.Eligible() {
		t.Fatalf("whole-device = %+v, want eligible", c)
	}
	if len(c.Reasons) != 0 || c.Attribution != AttributionPerPod {
		t.Fatalf("reasons=%v attribution=%s, want none/per-pod", c.Reasons, c.Attribution)
	}
	if c.DRA.State != DRADisabled || c.Evidence.WholeDevices != 1 || !c.Evidence.ReportFresh || c.Evidence.ReportPhysicalDevices != 1 {
		t.Fatalf("evidence = %+v dra = %+v", c.Evidence, c.DRA)
	}
	if !strings.HasPrefix(c.InputsDigest, "sha256:") || len(c.InputsDigest) != len("sha256:")+64 {
		t.Fatalf("digest = %q", c.InputsDigest)
	}
	if c.Summary != "topology=whole-device device_actions=eligible attribution=per-pod" {
		t.Fatalf("summary = %q", c.Summary)
	}
}

// TestClassifyTable covers every reason code and every topology at least once,
// from fixtures shaped like the real GPU Operator advertisements.
func TestClassifyTable(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*Snapshot)
		topology    Topology
		reasons     []ReasonCode
		attribution Attribution
	}{
		{
			name: "mig mixed strategy advertises instances beside whole GPUs",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = 7
				s.Labels[LabelMIGStrategy] = "mixed"
				s.Report.TopologySafety = types.AcceleratorTopologyPartitioned
				s.Report.PartitionDevices = 7
			},
			topology:    TopologyMixed,
			reasons:     []ReasonCode{ReasonMigPartitioned, ReasonMixedTopology},
			attribution: AttributionDeviceWide,
		},
		{
			name: "mig single strategy advertises instances under the whole GPU name",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity[ResourceWholeGPU] = 7
				s.Labels[LabelMIGStrategy] = "single"
				s.Report.TopologySafety = types.AcceleratorTopologyPartitioned
				s.Report.PartitionDevices = 7
			},
			topology:    TopologyMIG,
			reasons:     []ReasonCode{ReasonMigPartitioned},
			attribution: AttributionDeviceWide,
		},
		{
			// The GPU Operator defaults mig.strategy=single even with
			// mig.config=all-disabled: the strategy is an advertisement mode,
			// not a partition.
			name: "mig.strategy single alone leaves a verified unpartitioned whole GPU eligible",
			mutate: func(s *Snapshot) {
				s.Labels[LabelMIGStrategy] = "single"
			},
			topology:    TopologyWholeDevice,
			reasons:     nil,
			attribution: AttributionPerPod,
		},
		{
			name: "mig.strategy mixed alone leaves a verified unpartitioned whole GPU eligible",
			mutate: func(s *Snapshot) {
				s.Labels[LabelMIGStrategy] = "MIXED "
			},
			topology:    TopologyWholeDevice,
			reasons:     nil,
			attribution: AttributionPerPod,
		},
		{
			name: "mig.strategy single with partition devices in the report is mig",
			mutate: func(s *Snapshot) {
				s.Labels[LabelMIGStrategy] = "single"
				s.Report.PartitionDevices = 7
			},
			topology:    TopologyMIG,
			reasons:     []ReasonCode{ReasonMigPartitioned},
			attribution: AttributionDeviceWide,
		},
		{
			name: "mig.strategy single with a partitioned report safety is mig",
			mutate: func(s *Snapshot) {
				s.Labels[LabelMIGStrategy] = "single"
				s.Report.TopologySafety = types.AcceleratorTopologyPartitioned
			},
			topology:    TopologyMIG,
			reasons:     []ReasonCode{ReasonMigPartitioned},
			attribution: AttributionDeviceWide,
		},
		{
			name: "mig.strategy mixed with partition devices in the report beside whole GPUs is mixed",
			mutate: func(s *Snapshot) {
				s.Labels[LabelMIGStrategy] = "mixed"
				s.Report.PartitionDevices = 2
			},
			topology:    TopologyMixed,
			reasons:     []ReasonCode{ReasonMigPartitioned, ReasonMixedTopology},
			attribution: AttributionDeviceWide,
		},
		{
			name: "mig.strategy mixed with mig instances advertised beside a verified-unpartitioned report is a contradiction",
			mutate: func(s *Snapshot) {
				s.Labels[LabelMIGStrategy] = "mixed"
				s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = 7
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonMigPartitioned, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "mig.strategy single with mig instances advertised and no report is mig",
			mutate: func(s *Snapshot) {
				s.Labels[LabelMIGStrategy] = "single"
				s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = 7
				s.Report = nil
			},
			topology:    TopologyMIG,
			reasons:     []ReasonCode{ReasonMigPartitioned, ReasonReportMissing},
			attribution: AttributionDeviceWide,
		},
		{
			name: "empty mig.strategy refuses",
			mutate: func(s *Snapshot) {
				s.Labels[LabelMIGStrategy] = ""
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonMigStrategyLabelInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "unrecognised mig.strategy refuses",
			mutate: func(s *Snapshot) {
				s.Labels[LabelMIGStrategy] = "single-v2"
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonMigStrategyLabelInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "absent mig.strategy label is not a signal either way",
			mutate: func(s *Snapshot) {
				delete(s.Labels, LabelMIGStrategy)
			},
			topology:    TopologyWholeDevice,
			reasons:     nil,
			attribution: AttributionPerPod,
		},
		{
			name: "mig.capable true alone says the GPU could be partitioned, not that it is",
			mutate: func(s *Snapshot) {
				s.Labels[LabelMIGCapable] = "true"
			},
			topology:    TopologyWholeDevice,
			reasons:     nil,
			attribution: AttributionPerPod,
		},
		{
			name: "mig.capable outside true/false refuses",
			mutate: func(s *Snapshot) {
				s.Labels[LabelMIGCapable] = "yes"
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "mig from capacity alone with no report",
			mutate: func(s *Snapshot) {
				delete(s.Resources.Capacity, ResourceWholeGPU)
				delete(s.Resources.Allocatable, ResourceWholeGPU)
				s.Resources.Capacity["nvidia.com/mig-3g.20gb"] = 2
				s.Report = nil
			},
			topology:    TopologyMIG,
			reasons:     []ReasonCode{ReasonMigPartitioned, ReasonReportMissing},
			attribution: AttributionDeviceWide,
		},
		{
			name: "mig from the report partitions alone beside whole GPUs without a single strategy",
			mutate: func(s *Snapshot) {
				delete(s.Labels, LabelMIGStrategy)
				s.Report.PartitionDevices = 2
			},
			topology:    TopologyMixed,
			reasons:     []ReasonCode{ReasonMigPartitioned, ReasonMixedTopology},
			attribution: AttributionDeviceWide,
		},
		{
			// Time-slicing without renaming and without GFD labels (GFD not
			// deployed, or its labels stripped): nvidia.com/gpu advertises
			// physical x replicas and nothing else says so. The surplus
			// against the one verified physical GPU is a contradiction, not
			// a whole device, and the cause is named as a count mismatch,
			// never inferred as time-slicing.
			name: "whole-GPU advertisement above the verified physical count without GFD labels",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity[ResourceWholeGPU] = 4
				s.Resources.Allocatable[ResourceWholeGPU] = 4
				delete(s.Labels, LabelMIGStrategy)
				delete(s.Labels, LabelMIGCapable)
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonWholeDeviceCountMismatch, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			// The GFD labels present and benign change nothing: they are not
			// what says the counts disagree.
			name: "whole-GPU advertisement above the verified physical count beside benign GFD labels",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity[ResourceWholeGPU] = 2
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonWholeDeviceCountMismatch, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			// Fewer advertised than attested is the same contradiction the
			// other way round: a device the plugin no longer advertises is
			// not one the report vouches for.
			name: "whole-GPU advertisement below the verified physical count",
			mutate: func(s *Snapshot) {
				s.Report.PhysicalDevices = 2
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonWholeDeviceCountMismatch, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			// Agreeing counts above one stay the eligible whole-device shape.
			name: "whole-GPU advertisement equal to the verified physical count stays eligible",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity[ResourceWholeGPU] = 8
				s.Resources.Allocatable[ResourceWholeGPU] = 8
				s.Report.PhysicalDevices = 8
			},
			topology:    TopologyWholeDevice,
			attribution: AttributionPerPod,
		},
		{
			// The count test belongs to the node-source whole-device path
			// only: with no advertisement to compare, the attestation is the
			// whole of the evidence, exactly as before.
			name: "not-applicable source has no advertised count to disagree with",
			mutate: func(s *Snapshot) {
				s.Resources = Resources{Source: ResourceSourceNotApplicable}
				s.Report.PhysicalDevices = 2
			},
			topology:    TopologyWholeDevice,
			attribution: AttributionPerPod,
		},
		{
			// A disagreeing count beside a stale report is the report's
			// refusal, not a count finding: there is no trusted count to
			// compare against.
			name: "whole-GPU advertisement above an untrusted report count is the report refusal",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity[ResourceWholeGPU] = 4
				s.Report.ObservedAt = now.Add(-time.Hour)
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReportStale, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			// Beside a sharing facet the surplus is the sharing's own
			// signature; the facet refuses with its concrete code and keeps
			// the device-wide attribution rather than collapsing to unknown.
			name: "whole-GPU advertisement above the physical count beside the sharing-strategy label",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity[ResourceWholeGPU] = 4
				s.Labels[LabelSharingStrategy] = "time-slicing"
			},
			topology:    TopologyTimeSliced,
			reasons:     []ReasonCode{ReasonTimeSliced},
			attribution: AttributionDeviceWide,
		},
		{
			name: "time-sliced via gpu.replicas without rename",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity[ResourceWholeGPU] = 4
				s.Labels[LabelGPUReplicas] = "4"
			},
			topology:    TopologyTimeSliced,
			reasons:     []ReasonCode{ReasonTimeSliced},
			attribution: AttributionDeviceWide,
		},
		{
			name: "time-sliced via gpu.shared rename",
			mutate: func(s *Snapshot) {
				delete(s.Resources.Capacity, ResourceWholeGPU)
				delete(s.Resources.Allocatable, ResourceWholeGPU)
				s.Resources.Capacity[ResourceSharedGPU] = 2
			},
			topology:    TopologyTimeSliced,
			reasons:     []ReasonCode{ReasonTimeSliced},
			attribution: AttributionDeviceWide,
		},
		{
			name: "time-sliced via sharing-strategy label",
			mutate: func(s *Snapshot) {
				s.Labels[LabelSharingStrategy] = "time-slicing"
			},
			topology:    TopologyTimeSliced,
			reasons:     []ReasonCode{ReasonTimeSliced},
			attribution: AttributionDeviceWide,
		},
		{
			name: "mps sharing is not modelled and refuses as unknown",
			mutate: func(s *Snapshot) {
				s.Labels[LabelSharingStrategy] = "mps"
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "empty sharing-strategy refuses",
			mutate: func(s *Snapshot) {
				s.Labels[LabelSharingStrategy] = ""
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "replicas label of one is not sharing",
			mutate: func(s *Snapshot) {
				s.Labels[LabelGPUReplicas] = "1"
			},
			topology:    TopologyWholeDevice,
			reasons:     nil,
			attribution: AttributionPerPod,
		},
		{
			name: "malformed gpu.replicas refuses",
			mutate: func(s *Snapshot) {
				s.Labels[LabelGPUReplicas] = "four"
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReplicasLabelInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "zero gpu.replicas refuses",
			mutate: func(s *Snapshot) {
				s.Labels[LabelGPUReplicas] = "0"
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReplicasLabelInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "negative gpu.replicas refuses",
			mutate: func(s *Snapshot) {
				s.Labels[LabelGPUReplicas] = "-2"
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReplicasLabelInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "implausible gpu.replicas refuses",
			mutate: func(s *Snapshot) {
				s.Labels[LabelGPUReplicas] = "99999999999999999999"
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReplicasLabelInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "empty gpu.replicas refuses",
			mutate: func(s *Snapshot) {
				s.Labels[LabelGPUReplicas] = ""
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReplicasLabelInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name:        "missing report refuses as unknown even on whole capacity",
			mutate:      func(s *Snapshot) { s.Report = nil },
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReportMissing, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "stale report refuses as unknown",
			mutate: func(s *Snapshot) {
				s.Report.ObservedAt = now.Add(-6 * time.Minute)
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReportStale, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "report from the future is stale",
			mutate: func(s *Snapshot) {
				s.Report.ObservedAt = now.Add(time.Second)
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReportStale, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "report with zero observation time is stale",
			mutate: func(s *Snapshot) {
				s.Report.ObservedAt = time.Time{}
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReportStale, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "default max age applies when none configured",
			mutate: func(s *Snapshot) {
				s.MaxReportAge = 0
				s.Report.ObservedAt = now.Add(-DefaultMaxReportAge - time.Second)
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReportStale, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			// The shape of every virtualized GPU node: the agent attests one
			// whole, unpartitioned device and degrades the report because the
			// hypervisor withholds the PCI reset. The layout is reported; the
			// device action is refused by name.
			name: "degraded report keeps the whole-device layout and refuses device actions as not ready",
			mutate: func(s *Snapshot) {
				s.Report.Readiness = types.AcceleratorReadinessDegraded
			},
			topology:    TopologyWholeDevice,
			reasons:     []ReasonCode{ReasonReportNotReady},
			attribution: AttributionPerPod,
		},
		{
			name: "not-ready report keeps the layout and refuses",
			mutate: func(s *Snapshot) {
				s.Report.Readiness = types.AcceleratorReadinessNotReady
			},
			topology:    TopologyWholeDevice,
			reasons:     []ReasonCode{ReasonReportNotReady},
			attribution: AttributionPerPod,
		},
		{
			name: "report readiness outside the closed set is not ready",
			mutate: func(s *Snapshot) {
				s.Report.Readiness = ""
			},
			topology:    TopologyWholeDevice,
			reasons:     []ReasonCode{ReasonReportNotReady},
			attribution: AttributionPerPod,
		},
		{
			name: "report with no physical devices refuses even beside a whole-GPU advertisement",
			mutate: func(s *Snapshot) {
				s.Report.PhysicalDevices = 0
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReportNoPhysicalDevices, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "every report shortfall is named at once",
			mutate: func(s *Snapshot) {
				s.Report.ObservedAt = now.Add(-time.Hour)
				s.Report.Readiness = types.AcceleratorReadinessDegraded
				s.Report.PhysicalDevices = 0
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReportStale, ReasonReportNotReady, ReasonReportNoPhysicalDevices, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "stale partitioned report still names mig",
			mutate: func(s *Snapshot) {
				s.Report.ObservedAt = now.Add(-time.Hour)
				s.Report.TopologySafety = types.AcceleratorTopologyPartitioned
				s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = 7
				delete(s.Resources.Capacity, ResourceWholeGPU)
				delete(s.Resources.Allocatable, ResourceWholeGPU)
			},
			topology:    TopologyMIG,
			reasons:     []ReasonCode{ReasonMigPartitioned, ReasonReportStale},
			attribution: AttributionDeviceWide,
		},
		{
			name: "report topology unknown refuses",
			mutate: func(s *Snapshot) {
				s.Report.TopologySafety = types.AcceleratorTopologyUnknown
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "report topology unsafe refuses",
			mutate: func(s *Snapshot) {
				s.Report.TopologySafety = types.AcceleratorTopologyUnsafe
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "report topology outside the closed set refuses",
			mutate: func(s *Snapshot) {
				s.Report.TopologySafety = "verified-unpartitioned-v2"
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "capacity mig contradicts a verified-unpartitioned report",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = 7
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonMigPartitioned, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name:        "resources not observed refuses",
			mutate:      func(s *Snapshot) { s.Resources = Resources{} },
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name:        "resource source outside the closed set refuses",
			mutate:      func(s *Snapshot) { s.Resources.Source = "cache" },
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "no NVIDIA advertisement at all refuses",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity = map[string]int64{"cpu": 8, "amd.com/gpu": 1}
				s.Resources.Allocatable = map[string]int64{"cpu": 8}
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "implausible whole-GPU capacity refuses",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity[ResourceWholeGPU] = MaxAdvertisedDevices + 1
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "implausible gpu.shared capacity refuses and still names sharing",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity[ResourceSharedGPU] = MaxAdvertisedDevices + 1
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTimeSliced, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "implausible mig instance capacity refuses and still names mig",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = MaxAdvertisedDevices + 1
				s.Report.TopologySafety = types.AcceleratorTopologyPartitioned
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonMigPartitioned, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "mig instances that sum past the bound refuse even when each is inside it",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = MaxAdvertisedDevices
				s.Resources.Capacity["nvidia.com/mig-2g.10gb"] = 1
				s.Report.TopologySafety = types.AcceleratorTopologyPartitioned
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonMigPartitioned, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "mig instances that sum to exactly the bound are counted",
			mutate: func(s *Snapshot) {
				delete(s.Resources.Capacity, ResourceWholeGPU)
				delete(s.Resources.Allocatable, ResourceWholeGPU)
				s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = MaxAdvertisedDevices - 1
				s.Resources.Capacity["nvidia.com/mig-2g.10gb"] = 1
				s.Report.TopologySafety = types.AcceleratorTopologyPartitioned
			},
			topology:    TopologyMIG,
			reasons:     []ReasonCode{ReasonMigPartitioned},
			attribution: AttributionDeviceWide,
		},
		{
			name: "negative capacity is ignored, not counted",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity[ResourceWholeGPU] = -1
				s.Resources.Allocatable[ResourceWholeGPU] = -1
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "allocatable alone can prove a partition exists",
			mutate: func(s *Snapshot) {
				delete(s.Resources.Capacity, ResourceWholeGPU)
				delete(s.Resources.Allocatable, ResourceWholeGPU)
				s.Resources.Allocatable["nvidia.com/mig-1g.5gb"] = 1
				s.Report.TopologySafety = types.AcceleratorTopologyPartitioned
			},
			topology:    TopologyMIG,
			reasons:     []ReasonCode{ReasonMigPartitioned},
			attribution: AttributionDeviceWide,
		},
		{
			name: "platform without resource advertisement that still carries one is contradictory",
			mutate: func(s *Snapshot) {
				s.Resources.Source = ResourceSourceNotApplicable
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "dra observed with slices for the node and no plugin advertisement",
			mutate: func(s *Snapshot) {
				delete(s.Resources.Capacity, ResourceWholeGPU)
				delete(s.Resources.Allocatable, ResourceWholeGPU)
				s.DRA = observedDRA(1, 8)
			},
			topology:    TopologyDRAManaged,
			reasons:     []ReasonCode{ReasonDraManaged},
			attribution: AttributionUnknown,
		},
		{
			name: "dra observed beside a device plugin is mixed",
			mutate: func(s *Snapshot) {
				s.DRA = observedDRA(2, 8)
			},
			topology:    TopologyMixed,
			reasons:     []ReasonCode{ReasonDraManaged, ReasonMixedTopology},
			attribution: AttributionDeviceWide,
		},
		{
			name: "dra observed with no slices for the node and no claim on its pods changes nothing",
			mutate: func(s *Snapshot) {
				s.DRA = emptyDRA()
			},
			topology:    TopologyWholeDevice,
			reasons:     nil,
			attribution: AttributionPerPod,
		},
		{
			name: "dra observed with slices read but pods never inspected is invalid",
			mutate: func(s *Snapshot) {
				s.DRA = DRAObservation{State: DRAObserved, APIVersion: DRAAPIVersion}
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonDraObservationInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "dra direct claim reference on a pod with zero slices and no plugin advertisement is dra-managed",
			mutate: func(s *Snapshot) {
				delete(s.Resources.Capacity, ResourceWholeGPU)
				delete(s.Resources.Allocatable, ResourceWholeGPU)
				s.DRA = claimedDRA(0, 1, 1, 0)
			},
			topology:    TopologyDRAManaged,
			reasons:     []ReasonCode{ReasonDraManaged},
			attribution: AttributionUnknown,
		},
		{
			name: "dra template claim reference on a pod with zero slices beside a device plugin is mixed",
			mutate: func(s *Snapshot) {
				s.DRA = claimedDRA(0, 1, 0, 1)
			},
			topology:    TopologyMixed,
			reasons:     []ReasonCode{ReasonDraManaged, ReasonMixedTopology},
			attribution: AttributionDeviceWide,
		},
		{
			name: "dra direct claim reference on a pod beside a device plugin is mixed",
			mutate: func(s *Snapshot) {
				s.DRA = claimedDRA(0, 2, 2, 1)
			},
			topology:    TopologyMixed,
			reasons:     []ReasonCode{ReasonDraManaged, ReasonMixedTopology},
			attribution: AttributionDeviceWide,
		},
		{
			name: "dra claims and slices together are one dra signal",
			mutate: func(s *Snapshot) {
				delete(s.Resources.Capacity, ResourceWholeGPU)
				delete(s.Resources.Allocatable, ResourceWholeGPU)
				s.DRA = claimedDRA(1, 1, 1, 0)
			},
			topology:    TopologyDRAManaged,
			reasons:     []ReasonCode{ReasonDraManaged},
			attribution: AttributionUnknown,
		},
		{
			name: "dra claiming pod counted without a claim reference is invalid",
			mutate: func(s *Snapshot) {
				s.DRA = claimedDRA(0, 1, 0, 0)
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonDraObservationInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "dra claim reference without a claiming pod is invalid",
			mutate: func(s *Snapshot) {
				s.DRA = claimedDRA(0, 0, 1, 0)
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonDraObservationInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "dra more claiming pods than claim references is invalid",
			mutate: func(s *Snapshot) {
				s.DRA = claimedDRA(0, 3, 1, 1)
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonDraObservationInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "dra negative claim counts are invalid",
			mutate: func(s *Snapshot) {
				s.DRA = claimedDRA(0, 1, -1, 2)
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonDraObservationInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "dra disabled that still reports inspected pods is invalid",
			mutate: func(s *Snapshot) {
				s.DRA = DRAObservation{State: DRADisabled, PodsInspected: true}
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonDraObservationInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "dra disabled that still carries claim counts is invalid",
			mutate: func(s *Snapshot) {
				s.DRA = DRAObservation{State: DRADisabled, ClaimPods: 1, DirectClaims: 1}
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonDraObservationInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "dra enabled but not observed refuses",
			mutate: func(s *Snapshot) {
				s.DRA = DRAObservation{State: DRANotObserved, Reason: "api-unavailable"}
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name:        "dra state left at its zero value is not-observed and refuses",
			mutate:      func(s *Snapshot) { s.DRA = DRAObservation{} },
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name:        "dra state outside the closed set refuses",
			mutate:      func(s *Snapshot) { s.DRA = DRAObservation{State: "cached"} },
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "dra observation claimed from a beta version is invalid",
			mutate: func(s *Snapshot) {
				s.DRA = DRAObservation{State: DRAObserved, APIVersion: "resource.k8s.io/v1beta1", ResourceSlices: 1, Devices: 8}
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonDraObservationInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "dra observation without a version is invalid",
			mutate: func(s *Snapshot) {
				s.DRA = DRAObservation{State: DRAObserved}
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonDraObservationInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "dra observation with negative counts is invalid",
			mutate: func(s *Snapshot) {
				s.DRA = DRAObservation{State: DRAObserved, APIVersion: DRAAPIVersion, ResourceSlices: -1}
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonDraObservationInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "dra disabled that still carries counts is invalid",
			mutate: func(s *Snapshot) {
				s.DRA = DRAObservation{State: DRADisabled, ResourceSlices: 1}
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonDraObservationInvalid, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "mig and time-sliced together is mixed",
			mutate: func(s *Snapshot) {
				s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = 7
				s.Labels[LabelGPUReplicas] = "2"
				s.Report.TopologySafety = types.AcceleratorTopologyPartitioned
			},
			topology:    TopologyMixed,
			reasons:     []ReasonCode{ReasonMigPartitioned, ReasonTimeSliced, ReasonMixedTopology},
			attribution: AttributionDeviceWide,
		},
		{
			name:        "empty node name refuses",
			mutate:      func(s *Snapshot) { s.Node = " " },
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name:        "zero evaluation time refuses",
			mutate:      func(s *Snapshot) { s.EvaluatedAt = time.Time{} },
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReportStale, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "platform without resource advertisement relies on the fresh attestation",
			mutate: func(s *Snapshot) {
				s.Resources = Resources{Source: ResourceSourceNotApplicable}
				s.Labels = nil
			},
			topology:    TopologyWholeDevice,
			reasons:     nil,
			attribution: AttributionPerPod,
		},
		{
			name: "platform without resource advertisement and no physical devices in the report refuses",
			mutate: func(s *Snapshot) {
				s.Resources = Resources{Source: ResourceSourceNotApplicable}
				s.Report.PhysicalDevices = 0
			},
			topology:    TopologyUnknown,
			reasons:     []ReasonCode{ReasonReportNoPhysicalDevices, ReasonTopologyUnknown},
			attribution: AttributionUnknown,
		},
		{
			name: "platform without resource advertisement still refuses a partitioned report",
			mutate: func(s *Snapshot) {
				s.Resources = Resources{Source: ResourceSourceNotApplicable}
				s.Report.TopologySafety = types.AcceleratorTopologyPartitioned
				s.Report.PartitionDevices = 2
			},
			topology:    TopologyMIG,
			reasons:     []ReasonCode{ReasonMigPartitioned},
			attribution: AttributionDeviceWide,
		},
	}
	seenReasons := map[ReasonCode]bool{}
	seenTopologies := map[Topology]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := wholeDeviceSnapshot()
			tc.mutate(&s)
			c := Classify(s)
			if c.Topology != tc.topology {
				t.Errorf("topology = %s, want %s (%s)", c.Topology, tc.topology, c.Summary)
			}
			want := tc.reasons
			if want == nil {
				want = []ReasonCode{}
			}
			if !reflect.DeepEqual(c.Reasons, want) {
				t.Errorf("reasons = %v, want %v", c.Reasons, want)
			}
			if c.Attribution != tc.attribution {
				t.Errorf("attribution = %s, want %s", c.Attribution, tc.attribution)
			}
			wantActions := DeviceActionsRefused
			if tc.topology == TopologyWholeDevice && len(tc.reasons) == 0 {
				wantActions = DeviceActionsEligible
			}
			if c.DeviceActions != wantActions {
				t.Errorf("device_actions = %s, want %s", c.DeviceActions, wantActions)
			}
			if c.Topology == TopologyUnknown && !c.Has(ReasonTopologyUnknown) {
				t.Errorf("unknown topology must carry %s: %v", ReasonTopologyUnknown, c.Reasons)
			}
			if c.Topology == TopologyMixed && !c.Has(ReasonMixedTopology) {
				t.Errorf("mixed topology must carry %s: %v", ReasonMixedTopology, c.Reasons)
			}
			if c.Has(ReasonDraObservationInvalid) && (c.DRA.State != DRANotObserved || c.DRA.Reason == "") {
				t.Errorf("an invalid DRA observation must be rendered as not-observed with a reason: %+v", c.DRA)
			}
			for _, code := range c.Reasons {
				seenReasons[code] = true
			}
			seenTopologies[c.Topology] = true
		})
	}
	for _, code := range ReasonCodes() {
		if !seenReasons[code] {
			t.Errorf("reason code %s is never produced by the table", code)
		}
	}
	for _, topology := range Topologies() {
		if !seenTopologies[topology] {
			t.Errorf("topology %s is never produced by the table", topology)
		}
	}
}

func TestClosedEnumsArePinned(t *testing.T) {
	wantReasons := []ReasonCode{
		"ReplicasLabelInvalid", "MigStrategyLabelInvalid", "MigPartitioned", "TimeSliced", "DraManaged", "DraObservationInvalid",
		"MixedTopology", "ReportMissing", "ReportStale", "ReportNotReady", "ReportNoPhysicalDevices", "WholeDeviceCountMismatch", "TopologyUnknown",
	}
	if !reflect.DeepEqual(ReasonCodes(), wantReasons) {
		t.Fatalf("reason codes = %v, want %v", ReasonCodes(), wantReasons)
	}
	wantTopologies := []Topology{"whole-device", "mig", "time-sliced", "dra-managed", "mixed", "unknown"}
	if !reflect.DeepEqual(Topologies(), wantTopologies) {
		t.Fatalf("topologies = %v, want %v", Topologies(), wantTopologies)
	}
	if Version != "topology-classification/v1" || DRAAPIVersion != "resource.k8s.io/v1" {
		t.Fatalf("version = %q dra = %q", Version, DRAAPIVersion)
	}
}

func TestReportFacts(t *testing.T) {
	if ReportFacts(nil) != nil {
		t.Fatal("nil report must stay nil (missing), not become an empty fact set")
	}
	report := &types.AgentAcceleratorReport{
		ObservedAt: now, TopologySafety: types.AcceleratorTopologyPartitioned, Readiness: types.AcceleratorReadinessDegraded,
		Devices: []types.AgentAcceleratorDevice{
			{ID: "GPU-a", Kind: types.AcceleratorDevicePhysical},
			{ID: "MIG-1", Kind: types.AcceleratorDevicePartition, ParentID: "GPU-a"},
			{ID: "MIG-2", Kind: types.AcceleratorDevicePartition, ParentID: "GPU-a"},
		},
	}
	got := ReportFacts(report)
	want := &Report{ObservedAt: now, TopologySafety: types.AcceleratorTopologyPartitioned, Readiness: types.AcceleratorReadinessDegraded, PhysicalDevices: 1, PartitionDevices: 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("facts = %+v, want %+v", got, want)
	}
}

// TestInputsDigestIsDeterministicAndInputSensitive pins the digest contract:
// identical inputs share it regardless of evaluation time, map order or
// unrelated labels; any classified input changes it.
func TestInputsDigestIsDeterministicAndInputSensitive(t *testing.T) {
	base := Classify(wholeDeviceSnapshot())
	for i := 0; i < 20; i++ {
		if again := Classify(wholeDeviceSnapshot()); again.InputsDigest != base.InputsDigest {
			t.Fatalf("digest changed between identical evaluations: %s vs %s", again.InputsDigest, base.InputsDigest)
		}
	}
	later := wholeDeviceSnapshot()
	later.EvaluatedAt = now.Add(30 * time.Second)
	if Classify(later).InputsDigest != base.InputsDigest {
		t.Fatal("evaluation time must not enter the inputs digest")
	}
	unrelated := wholeDeviceSnapshot()
	unrelated.Labels["topology.kubernetes.io/zone"] = "a"
	unrelated.Resources.Capacity["memory"] = 1 << 30
	unrelated.Resources.Capacity["amd.com/gpu"] = 2
	unrelated.Resources.Capacity["example.com/fpga"] = 1
	unrelated.Resources.Allocatable["gpu.intel.com/i915"] = 1
	if Classify(unrelated).InputsDigest != base.InputsDigest {
		t.Fatal("unrelated labels, core resources and other vendors' resources must not enter the inputs digest")
	}
	if Classify(unrelated).Summary != base.Summary {
		t.Fatal("other vendors' resources must not change the classification either")
	}
	shared := wholeDeviceSnapshot()
	shared.Resources.Capacity[ResourceSharedGPU] = 2
	if Classify(shared).InputsDigest == base.InputsDigest {
		t.Fatal("the shared-GPU resource is classified and must enter the digest")
	}
	changes := map[string]func(*Snapshot){
		"node":          func(s *Snapshot) { s.Node = "gpu-b" },
		"node uid":      func(s *Snapshot) { s.NodeUID = "uid-b" },
		"capacity":      func(s *Snapshot) { s.Resources.Capacity[ResourceWholeGPU] = 2 },
		"allocatable":   func(s *Snapshot) { s.Resources.Allocatable["nvidia.com/mig-1g.5gb"] = 1 },
		"source":        func(s *Snapshot) { s.Resources.Source = ResourceSourceNotApplicable },
		"replicas":      func(s *Snapshot) { s.Labels[LabelGPUReplicas] = "2" },
		"mig strategy":  func(s *Snapshot) { s.Labels[LabelMIGStrategy] = "mixed" },
		"mig capable":   func(s *Snapshot) { s.Labels[LabelMIGCapable] = "true" },
		"sharing":       func(s *Snapshot) { s.Labels[LabelSharingStrategy] = "time-slicing" },
		"report time":   func(s *Snapshot) { s.Report.ObservedAt = now.Add(-2 * time.Minute) },
		"report safety": func(s *Snapshot) { s.Report.TopologySafety = types.AcceleratorTopologyPartitioned },
		"report ready":  func(s *Snapshot) { s.Report.Readiness = types.AcceleratorReadinessDegraded },
		"report gpus":   func(s *Snapshot) { s.Report.PhysicalDevices = 2 },
		"report missing": func(s *Snapshot) {
			s.Report = nil
		},
		"max age":             func(s *Snapshot) { s.MaxReportAge = time.Minute },
		"dra state":           func(s *Snapshot) { s.DRA.State = DRAObserved },
		"dra version":         func(s *Snapshot) { s.DRA = DRAObservation{State: DRAObserved, APIVersion: DRAAPIVersion} },
		"dra pods inspected":  func(s *Snapshot) { s.DRA = emptyDRA() },
		"dra count":           func(s *Snapshot) { s.DRA = observedDRA(1, 8) },
		"dra direct claim":    func(s *Snapshot) { s.DRA = claimedDRA(0, 1, 1, 0) },
		"dra template claim":  func(s *Snapshot) { s.DRA = claimedDRA(0, 1, 0, 1) },
		"dra two claim pods":  func(s *Snapshot) { s.DRA = claimedDRA(0, 2, 2, 0) },
		"dra claim and slice": func(s *Snapshot) { s.DRA = claimedDRA(1, 1, 1, 0) },
	}
	seen := map[string]string{base.InputsDigest: "base"}
	for name, change := range changes {
		s := wholeDeviceSnapshot()
		change(&s)
		digest := Classify(s).InputsDigest
		if prior, dup := seen[digest]; dup {
			t.Errorf("%s produced the same digest as %s", name, prior)
		}
		seen[digest] = name
	}
	// Driver order does not matter.
	a := wholeDeviceSnapshot()
	a.DRA = DRAObservation{State: DRAObserved, APIVersion: DRAAPIVersion, ResourceSlices: 1, Drivers: []string{"b.example", "a.example"}}
	b := wholeDeviceSnapshot()
	b.DRA = DRAObservation{State: DRAObserved, APIVersion: DRAAPIVersion, ResourceSlices: 1, Drivers: []string{"a.example", "b.example"}}
	if Classify(a).InputsDigest != Classify(b).InputsDigest {
		t.Fatal("driver order must not change the digest")
	}
}

func TestClassificationJSONIsStableAndCarriesNoRawPayload(t *testing.T) {
	s := wholeDeviceSnapshot()
	s.Labels["kubeneuron.io/secret-looking"] = "token-abc"
	s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = 7
	s.Report.TopologySafety = types.AcceleratorTopologyPartitioned
	s.DRA = observedDRA(1, 8)
	blob, err := json.Marshal(Classify(s))
	if err != nil {
		t.Fatal(err)
	}
	text := string(blob)
	for _, want := range []string{
		`"version":"topology-classification/v1"`, `"topology":"mixed"`, `"device_actions":"refused"`,
		`"reasons":["MigPartitioned","DraManaged","MixedTopology"]`, `"attribution":"device-wide"`,
		`"dra":{"state":"observed","api_version":"resource.k8s.io/v1","resource_slices":1,"devices":8,"drivers":["gpu.nvidia.com"],"pods_inspected":true,"claim_pods":0,"direct_claims":0,"template_claims":0}`,
		`"inputs_digest":"sha256:`, `"observed_at":"2026-10-21T12:00:00Z"`, `"report_physical_devices":1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("json lacks %s:\n%s", want, text)
		}
	}
	// A claim-only observation renders its bounded counts and nothing else:
	// the model has no field that could carry a namespace, pod or claim name.
	claimed := wholeDeviceSnapshot()
	claimed.DRA = claimedDRA(0, 2, 1, 1)
	claimedBlob, err := json.Marshal(Classify(claimed))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"topology":"mixed"`, `"reasons":["DraManaged","MixedTopology"]`,
		`"dra":{"state":"observed","api_version":"resource.k8s.io/v1","resource_slices":0,"devices":0,"pods_inspected":true,"claim_pods":2,"direct_claims":1,"template_claims":1}`,
	} {
		if !strings.Contains(string(claimedBlob), want) {
			t.Errorf("claim-only json lacks %s:\n%s", want, claimedBlob)
		}
	}
	var summary map[string]any
	if err := json.Unmarshal(claimedBlob, &summary); err != nil {
		t.Fatal(err)
	}
	for key, value := range summary["dra"].(map[string]any) {
		switch value.(type) {
		case float64, bool:
		case string:
			if key != "state" && key != "reason" && key != "api_version" {
				t.Errorf("dra summary carries a free-form string field %q=%v", key, value)
			}
		case []any:
			if key != "drivers" {
				t.Errorf("dra summary carries a list field %q=%v", key, value)
			}
		default:
			t.Errorf("dra summary carries an unexpected field %q=%v", key, value)
		}
	}
	if strings.Contains(text, "token-abc") || strings.Contains(text, "secret-looking") {
		t.Fatalf("unrelated node labels must not be rendered:\n%s", text)
	}
	var decoded Classification
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Topology != TopologyMixed || len(decoded.Reasons) != 3 {
		t.Fatalf("round trip = %+v", decoded)
	}
	// An eligible classification still renders an explicit empty reasons list.
	eligible, _ := json.Marshal(Classify(wholeDeviceSnapshot()))
	if !strings.Contains(string(eligible), `"reasons":[]`) {
		t.Fatalf("eligible json must carry an explicit empty reasons list:\n%s", eligible)
	}
}

// TestEvidenceLabelsAreBounded pins that a node-written label value reaches
// an API answer only trimmed, cut and printable: the four known GFD keys are
// echoed for explanation, never as an unbounded raw string.
func TestEvidenceLabelsAreBounded(t *testing.T) {
	huge := strings.Repeat("x", 10*MaxLabelEvidence)
	s := wholeDeviceSnapshot()
	s.Labels[LabelMIGStrategy] = huge
	s.Labels[LabelGPUReplicas] = "  4\x00\x1b[31m\n"
	s.Labels[LabelSharingStrategy] = "time-slicing"
	s.Labels[LabelMIGCapable] = "\u00e9true"
	c := Classify(s)
	if c.Eligible() {
		t.Fatalf("hostile labels must refuse: %s", c.Summary)
	}
	if got := c.Evidence.MIGStrategyLabel; len(got) > MaxLabelEvidence+len("...") || !strings.HasSuffix(got, "...") {
		t.Fatalf("mig strategy evidence = %q (%d bytes), want cut to %d plus an ellipsis", got, len(got), MaxLabelEvidence)
	}
	if got := c.Evidence.GPUReplicasLabel; got != "4??[31m" {
		t.Fatalf("replicas evidence = %q, want control characters replaced and whitespace trimmed", got)
	}
	if got := c.Evidence.MIGCapableLabel; got != "?true" {
		t.Fatalf("mig capable evidence = %q, want non-ASCII replaced", got)
	}
	if c.Evidence.SharingStrategyLabel != "time-slicing" {
		t.Fatalf("sharing evidence = %q, want a valid value echoed as is", c.Evidence.SharingStrategyLabel)
	}
	blob, _ := json.Marshal(c)
	if strings.Contains(string(blob), huge) || strings.Contains(string(blob), "\\u001b") {
		t.Fatalf("raw label material leaked into the rendered classification:\n%s", blob)
	}
	if boundedLabel("") != "" || boundedLabel("  none ") != "none" {
		t.Fatalf("boundedLabel = %q %q", boundedLabel(""), boundedLabel("  none "))
	}
}

// TestNodeWrittenResourceCountsAreBoundedInEvidence: MaxAdvertisedDevices
// caps every classified GPU resource count, not only nvidia.com/gpu. An
// oversized nvidia.com/gpu.shared or nvidia.com/mig-* value, or MIG values
// that sum past the bound, refuse as unknown while still naming the sharing
// or partition they prove, and the rendered evidence never carries the
// oversized figure: the count is 0, never clamped to something that looks
// real and never a wrapped sum.
func TestNodeWrittenResourceCountsAreBoundedInEvidence(t *testing.T) {
	partitioned := func() Snapshot {
		s := wholeDeviceSnapshot()
		s.Report.TopologySafety = types.AcceleratorTopologyPartitioned
		return s
	}
	manyHugeMIG := func(count int, value int64) Snapshot {
		s := partitioned()
		for i := 0; i < count; i++ {
			s.Resources.Capacity[fmt.Sprintf("nvidia.com/mig-%dg.%dgb", i+1, (i+1)*5)] = value
		}
		return s
	}
	cases := map[string]struct {
		snapshot Snapshot
		reasons  []ReasonCode
		mig      int64
		shared   int64
	}{
		"oversized shared": {
			snapshot: func() Snapshot {
				s := wholeDeviceSnapshot()
				s.Resources.Capacity[ResourceSharedGPU] = MaxAdvertisedDevices + 1
				return s
			}(),
			reasons: []ReasonCode{ReasonTimeSliced, ReasonTopologyUnknown},
		},
		"saturated shared": {
			snapshot: func() Snapshot {
				s := wholeDeviceSnapshot()
				s.Resources.Allocatable[ResourceSharedGPU] = 1<<63 - 1
				return s
			}(),
			reasons: []ReasonCode{ReasonTimeSliced, ReasonTopologyUnknown},
		},
		"shared at the bound is counted": {
			snapshot: func() Snapshot {
				s := wholeDeviceSnapshot()
				s.Resources.Capacity[ResourceSharedGPU] = MaxAdvertisedDevices
				return s
			}(),
			reasons: []ReasonCode{ReasonTimeSliced},
			shared:  MaxAdvertisedDevices,
		},
		"oversized mig": {
			snapshot: func() Snapshot {
				s := partitioned()
				s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = MaxAdvertisedDevices + 1
				return s
			}(),
			reasons: []ReasonCode{ReasonMigPartitioned, ReasonTopologyUnknown},
		},
		"oversized mig beside an in-bound one": {
			snapshot: func() Snapshot {
				s := partitioned()
				s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = 7
				s.Resources.Allocatable["nvidia.com/mig-2g.10gb"] = 1<<63 - 1
				return s
			}(),
			reasons: []ReasonCode{ReasonMigPartitioned, ReasonTopologyUnknown},
		},
		"many mig names each saturated": {
			snapshot: manyHugeMIG(64, 1<<63-1),
			reasons:  []ReasonCode{ReasonMigPartitioned, ReasonTopologyUnknown},
		},
		"many mig names each just under half the int64 range": {
			snapshot: manyHugeMIG(3, 1<<62),
			reasons:  []ReasonCode{ReasonMigPartitioned, ReasonTopologyUnknown},
		},
		"many mig names each at the bound": {
			snapshot: manyHugeMIG(2000, MaxAdvertisedDevices),
			reasons:  []ReasonCode{ReasonMigPartitioned, ReasonTopologyUnknown},
		},
		"many mig names that sum inside the bound are counted": {
			snapshot: manyHugeMIG(8, 7),
			reasons:  []ReasonCode{ReasonMigPartitioned, ReasonMixedTopology},
			mig:      56,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := Classify(tc.snapshot)
			if c.Eligible() {
				t.Fatalf("eligible: %s", c.Summary)
			}
			if !reflect.DeepEqual(c.Reasons, tc.reasons) {
				t.Errorf("reasons = %v, want %v", c.Reasons, tc.reasons)
			}
			if c.Evidence.MIGInstances != tc.mig {
				t.Errorf("mig_instances = %d, want %d", c.Evidence.MIGInstances, tc.mig)
			}
			if c.Evidence.SharedReplicas != tc.shared {
				t.Errorf("shared_replicas = %d, want %d", c.Evidence.SharedReplicas, tc.shared)
			}
			assertBoundedEvidence(t, c)
		})
	}
}

// assertBoundedEvidence is the invariant every rendered count must satisfy:
// inside [0, MaxAdvertisedDevices], so neither an oversized node-written
// value nor a wrapped sum can reach an API answer or an audit record.
func assertBoundedEvidence(t *testing.T, c Classification) {
	t.Helper()
	for name, value := range map[string]int64{
		"whole_devices": c.Evidence.WholeDevices, "mig_instances": c.Evidence.MIGInstances, "shared_replicas": c.Evidence.SharedReplicas,
	} {
		if value < 0 || value > MaxAdvertisedDevices {
			t.Errorf("%s = %d escapes [0, %d]: %s", name, value, MaxAdvertisedDevices, c.Summary)
		}
	}
}

// TestEligibilityIsOnlyEverWholeDeviceWithFreshVerifiedReport is the
// fail-closed property over a generated matrix of inputs: a classification is
// eligible only when the topology is whole-device, the report is a fresh,
// ready, verified-unpartitioned attestation of at least one physical GPU, no
// sharing or partition signal is present, the MIG strategy label (an
// advertisement mode, not a partition) is absent or one of its known
// spellings, every count is inside MaxAdvertisedDevices, resources were
// observed (or are not applicable to the platform) and DRA was either
// disabled or observed from resource.k8s.io/v1 with the node's Pods
// inspected and nothing for the node: no slice and no claim reference.
func TestEligibilityIsOnlyEverWholeDeviceWithFreshVerifiedReport(t *testing.T) {
	sources := []ResourceSource{ResourceSourceUnobserved, ResourceSourceNode, ResourceSourceNotApplicable, "bogus"}
	wholes := []int64{0, 1, 8, MaxAdvertisedDevices + 1}
	migs := []int64{0, 7, MaxAdvertisedDevices + 1}
	shareds := []int64{0, 2, MaxAdvertisedDevices + 1}
	replicas := []*string{nil, ptr("1"), ptr("2"), ptr("x"), ptr("0")}
	sharings := []*string{nil, ptr("none"), ptr("time-slicing"), ptr("mps")}
	strategies := []*string{nil, ptr("none"), ptr("single"), ptr("mixed"), ptr("")}
	reports := []func() *Report{
		func() *Report { return nil },
		freshVerified,
		func() *Report { r := freshVerified(); r.ObservedAt = now.Add(-time.Hour); return r },
		func() *Report {
			r := freshVerified()
			r.TopologySafety = types.AcceleratorTopologyPartitioned
			return r
		},
		func() *Report { r := freshVerified(); r.TopologySafety = types.AcceleratorTopologyUnknown; return r },
		func() *Report { r := freshVerified(); r.TopologySafety = types.AcceleratorTopologyUnsafe; return r },
		func() *Report { r := freshVerified(); r.PartitionDevices = 1; return r },
		func() *Report { r := freshVerified(); r.PhysicalDevices = 0; return r },
		func() *Report { r := freshVerified(); r.PhysicalDevices = 8; return r },
		func() *Report { r := freshVerified(); r.Readiness = types.AcceleratorReadinessDegraded; return r },
		func() *Report { r := freshVerified(); r.Readiness = ""; return r },
	}
	dras := []DRAObservation{
		{}, {State: DRADisabled}, {State: DRANotObserved, Reason: "api-unavailable"},
		{State: DRAObserved}, {State: DRAObserved, APIVersion: DRAAPIVersion}, emptyDRA(), observedDRA(1, 8),
		{State: DRAObserved, APIVersion: "resource.k8s.io/v1beta1"}, {State: DRADisabled, ResourceSlices: 1},
		{State: DRADisabled, PodsInspected: true}, {State: DRADisabled, ClaimPods: 1, DirectClaims: 1},
		claimedDRA(0, 1, 1, 0), claimedDRA(0, 1, 0, 1), claimedDRA(1, 1, 1, 0),
		claimedDRA(0, 1, 0, 0), claimedDRA(0, 0, 1, 0), claimedDRA(0, 2, 1, 0),
		{State: DRAObserved, APIVersion: DRAAPIVersion, PodsInspected: true, ResourceSlices: -1},
	}
	total, eligible := 0, 0
	for _, source := range sources {
		for _, whole := range wholes {
			for _, mig := range migs {
				for _, shared := range shareds {
					for _, rep := range replicas {
						for _, sharing := range sharings {
							for _, strategy := range strategies {
								for _, report := range reports {
									for _, dra := range dras {
										s := Snapshot{
											Node: "gpu-a", NodeUID: "uid", EvaluatedAt: now,
											Resources: Resources{Source: source, Capacity: map[string]int64{}},
											Labels:    map[string]string{},
											Report:    report(), MaxReportAge: 5 * time.Minute, DRA: dra,
										}
										if whole > 0 {
											s.Resources.Capacity[ResourceWholeGPU] = whole
										}
										if mig > 0 {
											s.Resources.Capacity["nvidia.com/mig-1g.5gb"] = mig
										}
										if shared > 0 {
											s.Resources.Capacity[ResourceSharedGPU] = shared
										}
										if rep != nil {
											s.Labels[LabelGPUReplicas] = *rep
										}
										if sharing != nil {
											s.Labels[LabelSharingStrategy] = *sharing
										}
										if strategy != nil {
											s.Labels[LabelMIGStrategy] = *strategy
										}
										c := Classify(s)
										total++
										if !slices.Contains(Topologies(), c.Topology) {
											t.Fatalf("topology %q outside the closed set", c.Topology)
										}
										if c.DeviceActions != DeviceActionsEligible && c.DeviceActions != DeviceActionsRefused {
											t.Fatalf("device_actions %q outside the closed set", c.DeviceActions)
										}
										// Eligible is whole-device with nothing against it. The one
										// refusal that can stand beside a whole-device layout is the
										// agent's own readiness shortfall (a verified, inventoried
										// device the agent cannot stand behind for a device action,
										// the shape of every virtualized GPU node); every other
										// refusal has turned the layout into something else.
										if (c.DeviceActions == DeviceActionsEligible) != (c.Topology == TopologyWholeDevice && len(c.Reasons) == 0) {
											t.Fatalf("eligible must be exactly whole-device with no reasons: %s", c.Summary)
										}
										if c.Topology == TopologyWholeDevice && c.DeviceActions == DeviceActionsRefused &&
											!slices.Equal(c.Reasons, []ReasonCode{ReasonReportNotReady}) {
											t.Fatalf("a refused whole-device layout must carry exactly ReportNotReady: %s", c.Summary)
										}
										if c.Topology != TopologyWholeDevice && len(c.Reasons) == 0 {
											t.Fatalf("a refusal must carry a reason: %s", c.Summary)
										}
										assertBoundedEvidence(t, c)
										if mig > MaxAdvertisedDevices && c.Evidence.MIGInstances != 0 {
											t.Fatalf("oversized mig advertisement rendered as %d: %+v", c.Evidence.MIGInstances, s)
										}
										if shared > MaxAdvertisedDevices && c.Evidence.SharedReplicas != 0 {
											t.Fatalf("oversized shared advertisement rendered as %d: %+v", c.Evidence.SharedReplicas, s)
										}
										if source == ResourceSourceNode && mig > 0 && !c.Has(ReasonMigPartitioned) {
											t.Fatalf("mig advertisement without a partition refusal: %+v", s)
										}
										if source == ResourceSourceNode && shared > 0 && !c.Has(ReasonTimeSliced) {
											t.Fatalf("shared advertisement without a sharing refusal: %+v", s)
										}
										// The count finding is made exactly where whole-device would
										// otherwise be the answer and the counts disagree: a trusted
										// (fresh, verified, inventoried) report, an in-bounds
										// whole-GPU advertisement from the node, and no partition,
										// sharing or DRA facet beside it. Readiness is not part of
										// trusting the layout, so a degraded report is counted too.
										trusted := s.Report != nil && s.Report.TopologySafety == types.AcceleratorTopologyVerifiedUnpartitioned &&
											now.Sub(s.Report.ObservedAt) <= s.MaxReportAge && s.Report.PhysicalDevices > 0
										// "Facetless" mirrors the classifier's facets, not its
										// refusals: a malformed replicas label or an MPS sharing
										// strategy refuses as unknown without being a sharing facet,
										// so the count finding is still made beside them.
										facetless := mig == 0 && shared == 0 && s.Report != nil && s.Report.PartitionDevices == 0 &&
											(rep == nil || *rep != "2") && (sharing == nil || *sharing != "time-slicing") &&
											!c.Has(ReasonDraManaged)
										wantMismatch := source == ResourceSourceNode && trusted && facetless &&
											whole > 0 && whole <= MaxAdvertisedDevices && whole != int64(s.Report.PhysicalDevices)
										if wantMismatch != c.Has(ReasonWholeDeviceCountMismatch) {
											t.Fatalf("count mismatch finding = %v, want %v: %s for %+v", c.Has(ReasonWholeDeviceCountMismatch), wantMismatch, c.Summary, s)
										}
										if c.Has(ReasonWholeDeviceCountMismatch) && (c.Topology != TopologyUnknown || c.Has(ReasonTimeSliced)) {
											t.Fatalf("a count mismatch must be unknown and never inferred as time-sliced: %s", c.Summary)
										}
										if !c.Eligible() {
											continue
										}
										eligible++
										// Every input that could refuse is in its permissive state.
										fresh := s.Report != nil && s.Report.TopologySafety == types.AcceleratorTopologyVerifiedUnpartitioned &&
											now.Sub(s.Report.ObservedAt) <= s.MaxReportAge && s.Report.PartitionDevices == 0 &&
											s.Report.Readiness == types.AcceleratorReadinessReady && s.Report.PhysicalDevices > 0
										if !fresh {
											t.Fatalf("eligible without a fresh, ready, verified report of a physical GPU: %+v", s)
										}
										if mig != 0 || shared != 0 || (rep != nil && *rep != "1") || (sharing != nil && *sharing != "none") {
											t.Fatalf("eligible with a sharing or partition signal: %+v", s)
										}
										if strategy != nil && *strategy != "none" && *strategy != "single" && *strategy != "mixed" {
											t.Fatalf("eligible with a mig.strategy outside the known spellings: %+v", s)
										}
										if source == ResourceSourceUnobserved || source == "bogus" {
											t.Fatalf("eligible without observed resources: %+v", s)
										}
										if source == ResourceSourceNode && (whole == 0 || whole > MaxAdvertisedDevices) {
											t.Fatalf("eligible without a plausible whole-GPU advertisement: %+v", s)
										}
										if source == ResourceSourceNode && whole != int64(s.Report.PhysicalDevices) {
											t.Fatalf("eligible with %d whole devices advertised against %d attested physical GPUs: %+v", whole, s.Report.PhysicalDevices, s)
										}
										if source == ResourceSourceNotApplicable && whole != 0 {
											t.Fatalf("eligible on a platform without advertisement despite %+v", s)
										}
										if dra.State != DRADisabled && (dra.State != DRAObserved || dra.APIVersion != DRAAPIVersion || !dra.PodsInspected ||
											dra.ResourceSlices != 0 || dra.ClaimPods != 0 || dra.DirectClaims != 0 || dra.TemplateClaims != 0) {
											t.Fatalf("eligible with DRA %+v", dra)
										}
										if dra.State == DRADisabled && (dra.ResourceSlices != 0 || dra.PodsInspected || dra.ClaimPods != 0 || dra.DirectClaims != 0) {
											t.Fatalf("eligible with a contradictory disabled DRA observation %+v", dra)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if eligible == 0 {
		t.Fatal("the matrix must contain eligible cases or it proves nothing")
	}
	t.Logf("matrix: %d classifications, %d eligible", total, eligible)
}

// TestClassifyNeverPanicsOnHostileInputs feeds node-written shapes that have
// crashed controllers before: saturated quantities, absurd labels, nil maps.
func TestClassifyNeverPanicsOnHostileInputs(t *testing.T) {
	for name, s := range map[string]Snapshot{
		"zero value": {},
		"saturated capacity": {
			Node: "n", EvaluatedAt: now,
			Resources: Resources{Source: ResourceSourceNode, Capacity: map[string]int64{ResourceWholeGPU: 1<<63 - 1, "nvidia.com/mig-1g.5gb": 1<<63 - 1}},
			Report:    freshVerified(), DRA: DRAObservation{State: DRADisabled},
		},
		"saturated capacity across several mig names": {
			Node: "n", EvaluatedAt: now,
			Resources: Resources{Source: ResourceSourceNode,
				Capacity:    map[string]int64{"nvidia.com/mig-1g.5gb": 1<<63 - 1, "nvidia.com/mig-2g.10gb": 1<<63 - 1, ResourceSharedGPU: 1<<63 - 1},
				Allocatable: map[string]int64{"nvidia.com/mig-3g.20gb": 1<<63 - 1, "nvidia.com/mig-7g.40gb": 2}},
			Report: freshVerified(), DRA: DRAObservation{State: DRADisabled},
		},
		"min int capacity": {
			Node: "n", EvaluatedAt: now,
			Resources: Resources{Source: ResourceSourceNode, Capacity: map[string]int64{ResourceWholeGPU: -1 << 63}},
			Report:    freshVerified(), DRA: DRAObservation{State: DRADisabled},
		},
		"garbage labels": {
			Node: "n", EvaluatedAt: now,
			Resources: Resources{Source: ResourceSourceNode, Capacity: map[string]int64{ResourceWholeGPU: 1}},
			Labels:    map[string]string{LabelGPUReplicas: strings.Repeat("9", 400), LabelSharingStrategy: "\x00", LabelMIGStrategy: strings.Repeat("x", 10000), LabelMIGCapable: "\xff"},
			Report:    freshVerified(), DRA: DRAObservation{State: DRADisabled},
		},
		"saturated dra counts": {
			Node: "n", EvaluatedAt: now,
			Resources: Resources{Source: ResourceSourceNode, Capacity: map[string]int64{ResourceWholeGPU: 1}},
			Report:    freshVerified(), DRA: DRAObservation{State: DRAObserved, APIVersion: DRAAPIVersion, ResourceSlices: -1 << 31, Devices: 1<<31 - 1},
		},
		"saturated dra claim counts": {
			Node: "n", EvaluatedAt: now,
			Resources: Resources{Source: ResourceSourceNode, Capacity: map[string]int64{ResourceWholeGPU: 1}},
			Report:    freshVerified(), DRA: DRAObservation{State: DRAObserved, APIVersion: DRAAPIVersion, PodsInspected: true, ClaimPods: 1<<63 - 1, DirectClaims: 1<<63 - 1, TemplateClaims: 1},
		},
	} {
		c := Classify(s)
		if c.Eligible() {
			t.Errorf("%s: hostile input must not be eligible: %s", name, c.Summary)
		}
		assertBoundedEvidence(t, c)
		if c.InputsDigest == "" {
			t.Errorf("%s: digest must be computed", name)
		}
		if blob, err := json.Marshal(c); err != nil || len(blob) > 4096 {
			t.Errorf("%s: rendered classification is %d bytes (%v); hostile input must not bloat it", name, len(blob), err)
		}
	}
}

// TestDRAObservationIsBoundedBeforeItIsRenderedOrDigested: a platform's
// observation is untrusted input. Driver strings, the not-observed reason and
// every count reach the rendered summary and the inputs digest only inside
// explicit bounds; anything outside them is refused as invalid and rendered as
// not-observed with a short reason and no counts, so neither the API answer
// nor the digest can carry a hostile or unbounded value.
func TestDRAObservationIsBoundedBeforeItIsRenderedOrDigested(t *testing.T) {
	hostile := []string{
		strings.Repeat("x", 10*MaxDRADriverLength),
		"gpu.nvidia.com\x00\x1b[31m",
		"ml/train-0",
		"GPU.NVIDIA.COM",
		" gpu.nvidia.com ",
		"",
	}
	// Malformed drivers that pass a naive character test: every one is
	// within MaxDRADriverLength and made of allowed bytes, and every one is
	// refused by the API's DNS-1123 subdomain rule.
	malformed := []string{
		"gpu..nvidia.com", "gpu.-nvidia.com", "gpu-.nvidia.com", ".gpu.nvidia.com", "gpu.nvidia.com.",
		strings.Repeat("l", maxDNSLabelLength+1) + ".nvidia.com", "gpu." + strings.Repeat("l", maxDNSLabelLength+1),
	}
	observed := func(mutate func(*DRAObservation)) DRAObservation {
		o := observedDRA(1, 8)
		mutate(&o)
		return o
	}
	manyDrivers := make([]string, 0, MaxDRADrivers+1)
	for i := 0; i <= MaxDRADrivers; i++ {
		manyDrivers = append(manyDrivers, "driver-"+strings.Repeat("a", i+1)+".example.com")
	}
	cases := map[string]struct {
		dra    DRAObservation
		reason string
	}{
		"driver too long":           {observed(func(o *DRAObservation) { o.Drivers = hostile[:1] }), "driver-invalid"},
		"driver with control chars": {observed(func(o *DRAObservation) { o.Drivers = hostile[1:2] }), "driver-invalid"},
		"driver that is a path":     {observed(func(o *DRAObservation) { o.Drivers = hostile[2:3] }), "driver-invalid"},
		"driver uppercase":          {observed(func(o *DRAObservation) { o.Drivers = hostile[3:4] }), "driver-invalid"},
		"driver untrimmed":          {observed(func(o *DRAObservation) { o.Drivers = hostile[4:5] }), "driver-invalid"},
		"driver empty":              {observed(func(o *DRAObservation) { o.Drivers = hostile[5:] }), "driver-invalid"},
		"driver duplicated":         {observed(func(o *DRAObservation) { o.Drivers = []string{"gpu.nvidia.com", "gpu.nvidia.com"} }), "driver-invalid"},
		"driver doubled dot":        {observed(func(o *DRAObservation) { o.Drivers = malformed[0:1] }), "driver-invalid"},
		"driver dot then hyphen":    {observed(func(o *DRAObservation) { o.Drivers = malformed[1:2] }), "driver-invalid"},
		"driver hyphen then dot":    {observed(func(o *DRAObservation) { o.Drivers = malformed[2:3] }), "driver-invalid"},
		"driver leading dot":        {observed(func(o *DRAObservation) { o.Drivers = malformed[3:4] }), "driver-invalid"},
		"driver trailing dot":       {observed(func(o *DRAObservation) { o.Drivers = malformed[4:5] }), "driver-invalid"},
		"driver first label too long": {observed(func(o *DRAObservation) {
			o.ResourceSlices, o.Drivers = 2, []string{"gpu.nvidia.com", malformed[5]}
		}), "driver-invalid"},
		"driver last label too long": {observed(func(o *DRAObservation) { o.Drivers = malformed[6:7] }), "driver-invalid"},
		"too many drivers":           {observed(func(o *DRAObservation) { o.Drivers = manyDrivers }), "too-many-drivers"},
		"devices with no slice":      {observed(func(o *DRAObservation) { o.ResourceSlices, o.Drivers = 0, nil }), "slice-counts-inconsistent"},
		"driver with no slice":       {observed(func(o *DRAObservation) { o.ResourceSlices, o.Devices = 0, 0 }), "slice-counts-inconsistent"},
		"slice with no driver":       {observed(func(o *DRAObservation) { o.Drivers = nil }), "slice-drivers-inconsistent"},
		"slice with empty driver list": {observed(func(o *DRAObservation) {
			o.Devices, o.Drivers = 0, []string{}
		}), "slice-drivers-inconsistent"},
		"more drivers than slices": {observed(func(o *DRAObservation) {
			o.Drivers = []string{"accel.example.com", "gpu.nvidia.com"}
		}), "slice-drivers-inconsistent"},
		"slices past the bound":  {observed(func(o *DRAObservation) { o.ResourceSlices = MaxDRACount + 1 }), "count-out-of-bounds"},
		"devices past the bound": {observed(func(o *DRAObservation) { o.Devices = 1<<31 - 1 }), "count-out-of-bounds"},
		"negative devices":       {observed(func(o *DRAObservation) { o.Devices = -1 }), "count-out-of-bounds"},
		"claim pods past the bound": {observed(func(o *DRAObservation) {
			o.ClaimPods, o.DirectClaims = MaxDRACount+1, MaxDRACount+1
		}), "count-out-of-bounds"},
		"direct plus template overflows": {observed(func(o *DRAObservation) {
			o.ClaimPods, o.DirectClaims, o.TemplateClaims = 1, 1<<63-1, 1
		}), "count-out-of-bounds"},
		"direct plus template wraps to zero": {observed(func(o *DRAObservation) {
			o.ClaimPods, o.DirectClaims, o.TemplateClaims = 0, 1<<63-1, -(1<<63 - 1)
		}), "count-out-of-bounds"},
		"claims do not add up": {observed(func(o *DRAObservation) { o.ClaimPods, o.DirectClaims = 2, 1 }), "claim-counts-inconsistent"},
		"beta version":         {observed(func(o *DRAObservation) { o.APIVersion = "resource.k8s.io/v1beta1" }), "api-version-invalid"},
		"pods not inspected":   {observed(func(o *DRAObservation) { o.PodsInspected = false }), "pods-not-inspected"},
		"disabled with data":   {DRAObservation{State: DRADisabled, Drivers: hostile[:1], Devices: 8}, "disabled-with-data"},
	}
	digests := map[string]string{}
	for name, tc := range cases {
		s := wholeDeviceSnapshot()
		s.DRA = tc.dra
		c := Classify(s)
		if c.Eligible() || !c.Has(ReasonDraObservationInvalid) || c.Topology != TopologyUnknown {
			t.Errorf("%s: classified %s, want unknown/DraObservationInvalid", name, c.Summary)
		}
		want := DRASummary{State: DRANotObserved, Reason: tc.reason}
		if !reflect.DeepEqual(c.DRA, want) {
			t.Errorf("%s: rendered dra = %+v, want %+v (no counts, no drivers, no version)", name, c.DRA, want)
		}
		blob, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range append(hostile[:5:5], malformed...) {
			if strings.Contains(string(blob), h) {
				t.Errorf("%s: hostile string rendered:\n%s", name, blob)
			}
		}
		if len(blob) > 2048 {
			t.Errorf("%s: rendered classification is %d bytes; a hostile observation must not bloat it", name, len(blob))
		}
		if c.InputsDigest == "" {
			t.Errorf("%s: digest must be computed", name)
		}
		digests[name] = c.InputsDigest
	}
	// The digest hashes the normalized observation, not the raw one: every
	// hostile variant of one failure class shares a digest, and two raw
	// observations that normalize alike agree.
	if digests["driver too long"] != digests["driver with control chars"] || digests["driver too long"] != digests["driver duplicated"] {
		t.Error("invalid drivers must normalize to one digest, or the raw strings are being hashed")
	}
	if digests["direct plus template overflows"] != digests["slices past the bound"] {
		t.Error("out-of-bounds counts must normalize to one digest, or the raw counts are being hashed")
	}
	if digests["driver too long"] == digests["too many drivers"] || digests["too many drivers"] == digests["slices past the bound"] ||
		digests["devices with no slice"] == digests["slice with no driver"] || digests["devices with no slice"] == digests["driver too long"] {
		t.Error("different failure classes must not share a digest")
	}
	if digests["driver doubled dot"] != digests["driver too long"] || digests["devices with no slice"] != digests["driver with no slice"] {
		t.Error("variants of one failure class must share a digest, or the raw observation is being hashed")
	}

	// The not-observed reason is bounded the same way: a token passes, a
	// hostile one is replaced and the raw value is neither rendered nor hashed.
	for name, tc := range map[string]struct{ raw, want string }{
		"stable reason":      {"api-unavailable", "api-unavailable"},
		"controller reason":  {"platform-cannot-observe", "platform-cannot-observe"},
		"locality reason":    {"locality-unsupported", "locality-unsupported"},
		"empty reason":       {"", ""},
		"longest token":      {strings.Repeat("r", MaxDRAReasonLength), strings.Repeat("r", MaxDRAReasonLength)},
		"too long":           {strings.Repeat("r", MaxDRAReasonLength+1), "invalid-reason"},
		"control characters": {"forbidden\x00\n\x1b[2J", "invalid-reason"},
		"with object name":   {"forbidden: ml/train-0", "invalid-reason"},
		"uppercase":          {"Forbidden", "invalid-reason"},
		"error text":         {`list failed: Get "https://10.0.0.1:443/apis/resource.k8s.io/v1/resourceslices": dial tcp: i/o timeout`, "invalid-reason"},
	} {
		s := wholeDeviceSnapshot()
		s.DRA = DRAObservation{State: DRANotObserved, Reason: tc.raw, APIVersion: DRAAPIVersion, ResourceSlices: 1, Drivers: hostile[:1]}
		c := Classify(s)
		if c.Eligible() || c.Has(ReasonDraObservationInvalid) || !c.Has(ReasonTopologyUnknown) {
			t.Errorf("%s: classified %s, want a plain not-observed refusal", name, c.Summary)
		}
		if want := (DRASummary{State: DRANotObserved, Reason: tc.want}); !reflect.DeepEqual(c.DRA, want) {
			t.Errorf("%s: rendered dra = %+v, want %+v", name, c.DRA, want)
		}
		blob, _ := json.Marshal(c)
		if tc.want != tc.raw && strings.Contains(string(blob), tc.raw) || strings.Contains(string(blob), hostile[0]) {
			t.Errorf("%s: raw reason or driver rendered:\n%s", name, blob)
		}
		other := wholeDeviceSnapshot()
		other.DRA = DRAObservation{State: DRANotObserved, Reason: tc.raw}
		if Classify(other).InputsDigest != c.InputsDigest {
			t.Errorf("%s: counts and drivers beside a not-observed state must not enter the digest", name)
		}
	}
	// A state outside the closed set is rendered as not-observed too, with
	// its reason bounded.
	s := wholeDeviceSnapshot()
	s.DRA = DRAObservation{State: DRAState(strings.Repeat("z", 5000)), Reason: strings.Repeat("y", 5000), Drivers: hostile[:1]}
	c := Classify(s)
	if want := (DRASummary{State: DRANotObserved, Reason: "invalid-reason"}); !reflect.DeepEqual(c.DRA, want) || c.Eligible() {
		t.Fatalf("unknown state rendered as %+v (%s), want %+v", c.DRA, c.Summary, want)
	}
	if blob, _ := json.Marshal(c); len(blob) > 2048 {
		t.Fatalf("unknown state bloats the rendering to %d bytes", len(blob))
	}

	// A slice that publishes no devices is a legitimate API shape: the
	// node is DRA-managed, the zero is rendered, nothing is refused.
	s = wholeDeviceSnapshot()
	s.DRA = observedDRA(1, 0)
	c = Classify(s)
	if c.Has(ReasonDraObservationInvalid) || !c.Has(ReasonDraManaged) || c.Eligible() || c.DRA.ResourceSlices != 1 || c.DRA.Devices != 0 {
		t.Fatalf("a zero-device slice must be accepted as DRA in use: %s dra=%+v", c.Summary, c.DRA)
	}

	// Valid observations are passed through unchanged: real driver names,
	// the longest allowed (labels at the DNS bound, 253 bytes in all),
	// exactly MaxDRADrivers of them, counts at the bound.
	label := strings.Repeat("a", maxDNSLabelLength)
	longest := strings.Join([]string{label, label, label, strings.Repeat("a", MaxDRADriverLength-3*(maxDNSLabelLength+1))}, ".")
	if len(longest) != MaxDRADriverLength {
		t.Fatalf("test bug: longest driver is %d bytes", len(longest))
	}
	full := observedDRA(MaxDRACount, MaxDRACount)
	full.Drivers = append([]string{longest, "gpu.nvidia.com", "accel.example.com"}, manyDrivers[:MaxDRADrivers-3]...)
	full.ClaimPods, full.DirectClaims, full.TemplateClaims = MaxDRACount, MaxDRACount, MaxDRACount
	s = wholeDeviceSnapshot()
	s.DRA = full
	c = Classify(s)
	if c.Has(ReasonDraObservationInvalid) || !c.Has(ReasonDraManaged) || c.DRA.State != DRAObserved {
		t.Fatalf("a valid observation at the bounds must be accepted: %s dra=%+v", c.Summary, c.DRA)
	}
	if !slices.IsSorted(c.DRA.Drivers) || len(c.DRA.Drivers) != MaxDRADrivers || !slices.Contains(c.DRA.Drivers, longest) {
		t.Fatalf("drivers = %v, want all %d sorted and intact", c.DRA.Drivers, MaxDRADrivers)
	}
	if c.DRA.ResourceSlices != MaxDRACount || c.DRA.Devices != MaxDRACount || c.DRA.ClaimPods != MaxDRACount {
		t.Fatalf("counts at the bound must pass through: %+v", c.DRA)
	}
}

func TestValidDRADriverIsADNS1123Subdomain(t *testing.T) {
	label := strings.Repeat("a", maxDNSLabelLength)
	longest := strings.Join([]string{label, label, label, strings.Repeat("a", MaxDRADriverLength-3*(maxDNSLabelLength+1))}, ".")
	for name, ok := range map[string]bool{
		"gpu.nvidia.com": true, "a": true, "0": true, "a-b.c-d": true, "a.b.c.d.e": true, "1-2.3": true,
		label: true, label + ".com": true, longest: true,
		"":                               false,
		"-a":                             false,
		"a-":                             false,
		".a":                             false,
		"a.":                             false,
		"a..b":                           false,
		"a.-b":                           false,
		"a-.b":                           false,
		"a_b":                            false,
		"A.b":                            false,
		"a b":                            false,
		"a/b":                            false,
		"a.b\x00":                        false,
		label + "a":                      false,
		label + "a.com":                  false,
		"gpu." + label + "a":             false,
		longest + "a":                    false,
		strings.Repeat("a.", 126) + "ab": false, // 254 bytes of valid labels
	} {
		if got := validDRADriver(name); got != ok {
			t.Errorf("validDRADriver(%q) = %v, want %v", boundedLabel(name), got, ok)
		}
	}
}

func ptr(s string) *string { return &s }
