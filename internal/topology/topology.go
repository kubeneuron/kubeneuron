// SPDX-License-Identifier: LicenseRef-KubeNeuron

// Package topology classifies the accelerator device topology of one node
// from a captured, immutable snapshot, and says whether a device-scoped
// action (today: a physical NVIDIA GPU reset) may be considered on it.
//
// The package is pure on purpose: it reads no clock, no Kubernetes API and no
// store. A caller captures node capacity, the GPU Feature Discovery labels,
// the agent's accelerator report and, when discovery was asked for, a
// read-only Dynamic Resource Allocation observation (ResourceSlices for the
// node and the claim references of the Pods on it) into a Snapshot, and
// Classify turns that into one versioned, deterministic Classification. The
// same inputs always give the same answer and the same inputs digest, which
// is what lets readiness, the reset gate and an audit record agree.
//
// Every rule here can only refuse. A classification never grants anything the
// existing runtime-profile gate did not already grant: the controller runs the
// legacy evidence test first and consults this package only to add refusals
// (see internal/controller admission). Missing, stale or contradictory inputs
// are `unknown`, and `unknown` always refuses — there is no implicit
// whole-device.
package topology

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Version is carried by every Classification. A bump is an explicit
// compatibility event for API consumers and audit readers.
const Version = "topology-classification/v1"

// Topology is the closed set of device layouts the classifier can name.
type Topology string

const (
	// TopologyWholeDevice: every advertised NVIDIA device is a whole physical
	// GPU, the agent has verified the devices are unpartitioned, and nothing
	// shares them. The only topology on which a device action can be eligible.
	TopologyWholeDevice Topology = "whole-device"
	// TopologyMIG: the node carries Multi-Instance GPU partitions.
	TopologyMIG Topology = "mig"
	// TopologyTimeSliced: the device plugin shares each physical GPU between
	// several pods (gpu.replicas > 1, gpu.shared, or the time-slicing strategy).
	TopologyTimeSliced Topology = "time-sliced"
	// TopologyDRAManaged: devices on this node are published through Dynamic
	// Resource Allocation ResourceSlices, or a Pod scheduled on the node
	// references a ResourceClaim, rather than (only) a device plugin.
	TopologyDRAManaged Topology = "dra-managed"
	// TopologyMixed: more than one of the above at once.
	TopologyMixed Topology = "mixed"
	// TopologyUnknown: the inputs are missing, implausible or contradict each
	// other. Always refused.
	TopologyUnknown Topology = "unknown"
)

// Topologies lists every value of Topology, for tests and schema pins.
func Topologies() []Topology {
	return []Topology{TopologyWholeDevice, TopologyMIG, TopologyTimeSliced, TopologyDRAManaged, TopologyMixed, TopologyUnknown}
}

// DeviceActions is the classifier's verdict on device-scoped actions.
type DeviceActions string

const (
	DeviceActionsEligible DeviceActions = "eligible"
	DeviceActionsRefused  DeviceActions = "refused"
)

// Attribution says how a fault on a device maps to the workloads using it.
// Under time-slicing every pod on the GPU is hit by any fault on it, so an
// impact preview must present the whole device, never a single pod.
type Attribution string

const (
	AttributionPerPod     Attribution = "per-pod"
	AttributionDeviceWide Attribution = "device-wide"
	AttributionUnknown    Attribution = "unknown"
)

// ReasonCode is a stable machine-readable explanation of a refusal. Do not
// rename a value: API consumers, CLI output and audit records use these as a
// contract. A new cause needs a new code.
type ReasonCode string

const (
	ReasonMigPartitioned       ReasonCode = "MigPartitioned"
	ReasonTimeSliced           ReasonCode = "TimeSliced"
	ReasonDraManaged           ReasonCode = "DraManaged"
	ReasonMixedTopology        ReasonCode = "MixedTopology"
	ReasonReportMissing        ReasonCode = "ReportMissing"
	ReasonReportStale          ReasonCode = "ReportStale"
	ReasonTopologyUnknown      ReasonCode = "TopologyUnknown"
	ReasonReplicasLabelInvalid ReasonCode = "ReplicasLabelInvalid"
	// ReasonMigStrategyLabelInvalid: nvidia.com/mig.strategy is present but
	// is not one of the spellings this version knows (none, single, mixed).
	// An unknown strategy is not "no MIG"; it is unknown.
	ReasonMigStrategyLabelInvalid ReasonCode = "MigStrategyLabelInvalid"
	// ReasonReportNotReady: the agent report is present and fresh but does
	// not declare the accelerator runtime ready. A device action needs a
	// ready runtime to attest the topology it is about to act on.
	ReasonReportNotReady ReasonCode = "ReportNotReady"
	// ReasonReportNoPhysicalDevices: the agent report declares no physical
	// GPU. There is no whole device to act on, so "verified unpartitioned"
	// over an empty inventory proves nothing.
	ReasonReportNoPhysicalDevices ReasonCode = "ReportNoPhysicalDevices"
	// ReasonDraObservationInvalid: the Dynamic Resource Allocation
	// observation contradicts itself or exceeds its bounds (observed from a
	// version other than resource.k8s.io/v1, counts beside a disabled state,
	// an observed state whose Pods were never inspected, a negative or
	// overlarge count, claim counts that do not add up, slice evidence that
	// does not add up (devices or drivers with no slice, a slice with no
	// driver, more drivers than slices), or a driver list that is too long
	// or not made of driver names). It is treated as not observed, and none
	// of its counts or drivers is rendered or digested.
	ReasonDraObservationInvalid ReasonCode = "DraObservationInvalid"
	// ReasonWholeDeviceCountMismatch: the node advertises a different number
	// of nvidia.com/gpu devices than the fresh, ready, verified-unpartitioned
	// agent report counts physical GPUs, and no partition, sharing or DRA
	// facet explains the difference. A device plugin configured to
	// time-slice advertises physical x replicas under the whole-GPU name,
	// and when GPU Feature Discovery labels are absent that surplus is the
	// only trace of it; the classifier does not infer what caused the
	// disagreement (that would be a guess), it refuses the whole-device
	// claim as unknown and names the contradiction.
	ReasonWholeDeviceCountMismatch ReasonCode = "WholeDeviceCountMismatch"
)

// reasonOrder is the canonical rendering order: the concrete finding first,
// the generic "unknown" last.
var reasonOrder = map[ReasonCode]int{
	ReasonReplicasLabelInvalid:     0,
	ReasonMigStrategyLabelInvalid:  1,
	ReasonMigPartitioned:           2,
	ReasonTimeSliced:               3,
	ReasonDraManaged:               4,
	ReasonDraObservationInvalid:    5,
	ReasonMixedTopology:            6,
	ReasonReportMissing:            7,
	ReasonReportStale:              8,
	ReasonReportNotReady:           9,
	ReasonReportNoPhysicalDevices:  10,
	ReasonWholeDeviceCountMismatch: 11,
	ReasonTopologyUnknown:          12,
}

// ReasonCodes lists every value of ReasonCode, for tests and schema pins.
func ReasonCodes() []ReasonCode {
	out := make([]ReasonCode, 0, len(reasonOrder))
	for code := range reasonOrder {
		out = append(out, code)
	}
	sort.Slice(out, func(i, j int) bool { return reasonOrder[out[i]] < reasonOrder[out[j]] })
	return out
}

// ResourceSource says where the Resources in a Snapshot came from. The zero
// value is "not observed", which refuses: a caller that forgot to capture
// capacity must not be read as a node with no partitions.
type ResourceSource string

const (
	// ResourceSourceUnobserved: the caller could not read the node's
	// advertised resources. Refused.
	ResourceSourceUnobserved ResourceSource = ""
	// ResourceSourceNode: Capacity/Allocatable are the node object's own
	// advertisement (Kubernetes status.capacity / status.allocatable).
	ResourceSourceNode ResourceSource = "node"
	// ResourceSourceNotApplicable: the platform has no device-plugin style
	// resource advertisement at all (a bare-metal inventory), so capacity and
	// the sharing labels cannot exist and the agent's attestation is the only
	// topology evidence there is. This is what the pre-existing reset gate
	// already relied on for such platforms; it is explicitly NOT the value a
	// Kubernetes platform may report.
	ResourceSourceNotApplicable ResourceSource = "not-applicable"
)

// DRAState is the closed set of Dynamic Resource Allocation observation
// states. The zero value is treated as not-observed and refuses: discovery
// that was neither performed nor explicitly switched off is unknown evidence.
type DRAState string

const (
	// DRADisabled: discovery is off by configuration; DRA is not consulted and
	// the classification says so.
	DRADisabled DRAState = "disabled"
	// DRANotObserved: discovery was asked for but could not be performed (the
	// API group/version is not served, the read was forbidden, or it failed).
	// Refused: the configuration says DRA may be in use and nothing proved
	// otherwise.
	DRANotObserved DRAState = "not-observed"
	// DRAObserved: resource.k8s.io/v1 was read successfully AND the Pods
	// scheduled on the node were listed and their claim references
	// inspected. Accepted only when APIVersion is exactly DRAAPIVersion and
	// PodsInspected is set; an observation claimed from any other
	// group/version, or one that read slices without inspecting Pods, is
	// invalid and refuses.
	DRAObserved DRAState = "observed"
)

// DRAAPIVersion is the only group/version a DRA observation may claim. Beta
// versions carry different schemas and are never consulted.
const DRAAPIVersion = "resource.k8s.io/v1"

// Well-known NVIDIA device plugin and GPU Feature Discovery spellings. The
// values behind them are written by the NODE and are never trusted without a
// clamp.
const (
	ResourceWholeGPU  = "nvidia.com/gpu"
	ResourceSharedGPU = "nvidia.com/gpu.shared"
	resourceMIGPrefix = "nvidia.com/mig-"

	LabelGPUReplicas     = "nvidia.com/gpu.replicas"
	LabelMIGStrategy     = "nvidia.com/mig.strategy"
	LabelMIGCapable      = "nvidia.com/mig.capable"
	LabelSharingStrategy = "nvidia.com/gpu.sharing-strategy"

	// MaxAdvertisedDevices bounds any node-written count. Mirrors the
	// platform's inventory clamp: a count past it is not knowable capacity.
	MaxAdvertisedDevices = 1024
	// DefaultMaxReportAge bounds report freshness when the caller supplies no
	// profile-specific maximum. It matches the decision evaluator's default.
	DefaultMaxReportAge = 10 * time.Minute

	// Bounds on a DRA observation. Every count (slices, devices, claiming
	// Pods, direct and template references) must lie in [0, MaxDRACount];
	// the two reference counts are only ever added once both are inside
	// that bound, so the sum cannot overflow. A node carries at most
	// MaxDRADrivers distinct drivers, each a DNS-1123 subdomain of at most
	// MaxDRADriverLength bytes (the shape the API enforces on spec.driver).
	// An observation outside any bound is invalid and refuses; nothing from
	// it is rendered or digested.
	MaxDRACount        = 4096
	MaxDRADrivers      = 8
	MaxDRADriverLength = 253
	// MaxDRAReasonLength bounds the not-observed reason: a token of
	// lowercase letters, digits and hyphens. Anything else is rendered and
	// digested as "invalid-reason", never echoed.
	MaxDRAReasonLength = 64
)

// Resources is the node's advertised extended-resource inventory. Only the
// NVIDIA names this version classifies (nvidia.com/gpu, nvidia.com/gpu.shared
// and nvidia.com/mig-*) are read or digested; core resources and other
// vendors' qualified names are ignored and cannot churn the digest.
type Resources struct {
	Source      ResourceSource
	Capacity    map[string]int64
	Allocatable map[string]int64
}

// Report is the subset of an agent accelerator report the classifier reads.
// Use ReportFacts to derive it so the two can never drift.
type Report struct {
	ObservedAt       time.Time
	TopologySafety   types.AcceleratorTopologySafety
	Readiness        types.AcceleratorReadiness
	PhysicalDevices  int
	PartitionDevices int
}

// ReportFacts projects an agent report onto the facts the classifier uses.
// A nil report stays nil (missing), never an empty fact set.
func ReportFacts(report *types.AgentAcceleratorReport) *Report {
	if report == nil {
		return nil
	}
	facts := &Report{
		ObservedAt:     report.ObservedAt,
		TopologySafety: report.TopologySafety,
		Readiness:      report.Readiness,
	}
	for _, device := range report.Devices {
		switch device.Kind {
		case types.AcceleratorDevicePhysical:
			facts.PhysicalDevices++
		case types.AcceleratorDevicePartition:
			facts.PartitionDevices++
		}
	}
	return facts
}

// DRAObservation is a read-only observation of Dynamic Resource Allocation
// use on one node: the resource.k8s.io/v1 ResourceSlices published for it,
// and the ResourceClaim references carried by the Pods scheduled on it (read
// from core/v1 Pod specs, never from ResourceClaim objects). It carries
// counts and driver names only: no namespace, pod, claim, device or object
// names, nothing a caller could mistake for an inventory to act on. The
// counts and drivers are bounded by the MaxDRA* constants; Classify
// re-checks every bound and refuses an observation outside them.
type DRAObservation struct {
	State DRAState
	// Reason is a short, stable explanation when State is not-observed
	// ("api-unavailable", "forbidden", "locality-unsupported", ...): a token
	// of lowercase letters, digits and hyphens, at most MaxDRAReasonLength
	// long. Classify renders anything else as "invalid-reason".
	Reason string
	// APIVersion is the group/version that was read; always resource.k8s.io/v1
	// for an observation this package accepts.
	APIVersion string
	// ResourceSlices counts the slices published for this node by name.
	ResourceSlices int
	// Devices counts the devices across those slices.
	Devices int
	// Drivers lists the distinct driver names across those slices, sorted.
	Drivers []string
	// PodsInspected is true only when every Pod scheduled on the node was
	// listed and every spec.resourceClaims entry on it was inspected. An
	// observed state without it is inconsistent and refuses: a successful
	// slice read alone never proves that no claim is in use, and inability
	// to inspect Pods is never "no claims".
	PodsInspected bool
	// ClaimPods counts the Pods scheduled on the node that carry at least
	// one ResourceClaim reference, direct or through a template.
	ClaimPods int
	// DirectClaims counts the spec.resourceClaims entries across those Pods
	// that name a ResourceClaim (resourceClaimName).
	DirectClaims int
	// TemplateClaims counts the spec.resourceClaims entries across those
	// Pods that name a ResourceClaimTemplate (resourceClaimTemplateName).
	TemplateClaims int
}

// ClaimReferences is the total number of ResourceClaim references observed
// on the node's Pods, direct and template together.
func (o DRAObservation) ClaimReferences() int {
	return o.DirectClaims + o.TemplateClaims
}

// Snapshot is the immutable input to Classify. The caller owns data access
// and time; this package owns only the meaning of the captured facts.
type Snapshot struct {
	Node        string
	NodeUID     string
	EvaluatedAt time.Time
	Resources   Resources
	// Labels are the node's current labels. Only the GPU Feature Discovery
	// keys above are read, and only those enter the digest.
	Labels map[string]string
	// Report is nil when no NVIDIA accelerator report is retained for the node.
	Report *Report
	// MaxReportAge bounds freshness; non-positive means DefaultMaxReportAge.
	MaxReportAge time.Duration
	DRA          DRAObservation
}

// Evidence is the normalized view of the inputs a Classification was made
// from. It is what an operator reads to understand a refusal, and it carries
// no raw payloads.
type Evidence struct {
	ResourceSource ResourceSource `json:"resource_source"`
	// WholeDevices is the nvidia.com/gpu advertisement; 0 when absent or past
	// MaxAdvertisedDevices (which refuses as unknown).
	WholeDevices int64 `json:"whole_devices"`
	// MIGInstances sums the nvidia.com/mig-* advertisements; 0 when any
	// value, or the sum, is past MaxAdvertisedDevices (which refuses as
	// unknown). It never exceeds the bound.
	MIGInstances int64 `json:"mig_instances"`
	// SharedReplicas is the nvidia.com/gpu.shared advertisement; 0 when
	// absent or past MaxAdvertisedDevices (which refuses as unknown).
	SharedReplicas int64 `json:"shared_replicas"`
	// The four GPU Feature Discovery label values are node-written. They are
	// rendered through boundedLabel: trimmed, cut to MaxLabelEvidence
	// printable ASCII characters, never the raw value. Only these four keys
	// are ever read; no other node label reaches a classification.
	GPUReplicasLabel      string                          `json:"gpu_replicas_label,omitempty"`
	MIGStrategyLabel      string                          `json:"mig_strategy_label,omitempty"`
	MIGCapableLabel       string                          `json:"mig_capable_label,omitempty"`
	SharingStrategyLabel  string                          `json:"sharing_strategy_label,omitempty"`
	ReportObservedAt      time.Time                       `json:"report_observed_at,omitzero"`
	ReportTopologySafety  types.AcceleratorTopologySafety `json:"report_topology_safety,omitempty"`
	ReportReadiness       types.AcceleratorReadiness      `json:"report_readiness,omitempty"`
	ReportPhysicalDevices int                             `json:"report_physical_devices"`
	ReportPartitions      int                             `json:"report_partitions"`
	ReportFresh           bool                            `json:"report_fresh"`
}

// MaxLabelEvidence bounds how much of a node-written label value a
// classification will echo back. The known GFD values are a few characters;
// anything longer is already invalid and is cut, not rendered.
const MaxLabelEvidence = 64

// DRASummary is the DRA part of a Classification. It is rendered from the
// normalized observation only: counts within MaxDRACount, at most
// MaxDRADrivers validated driver names, and a reason that is a short token.
// An observation outside those bounds is rendered as not-observed with a
// reason and no counts at all.
type DRASummary struct {
	State          DRAState `json:"state"`
	Reason         string   `json:"reason,omitempty"`
	APIVersion     string   `json:"api_version,omitempty"`
	ResourceSlices int      `json:"resource_slices"`
	Devices        int      `json:"devices"`
	Drivers        []string `json:"drivers,omitempty"`
	// PodsInspected says whether the node's Pods were listed and their claim
	// references inspected; false on a disabled or not-observed state.
	PodsInspected  bool `json:"pods_inspected"`
	ClaimPods      int  `json:"claim_pods"`
	DirectClaims   int  `json:"direct_claims"`
	TemplateClaims int  `json:"template_claims"`
}

// Classification is the complete, versioned answer. It is safe to render in
// the operator console, to embed in readiness, and to record in an audit row.
type Classification struct {
	Version       string        `json:"version"`
	Node          string        `json:"node"`
	NodeUID       string        `json:"node_uid,omitempty"`
	Topology      Topology      `json:"topology"`
	DeviceActions DeviceActions `json:"device_actions"`
	Reasons       []ReasonCode  `json:"reasons"`
	Attribution   Attribution   `json:"attribution"`
	Evidence      Evidence      `json:"evidence"`
	DRA           DRASummary    `json:"dra"`
	Summary       string        `json:"summary"`
	ObservedAt    time.Time     `json:"observed_at"`
	// InputsDigest is sha256 over the canonical, normalized inputs (not the
	// evaluation time), so two evaluations of the same evidence share it and a
	// change in any input changes it.
	InputsDigest string `json:"inputs_digest"`
}

// Eligible reports whether a device-scoped action may be considered. It is
// the only permissive answer this package gives, and it is given only for a
// fresh, agent-verified, unshared whole-device node.
func (c Classification) Eligible() bool {
	return c.DeviceActions == DeviceActionsEligible
}

// Has reports whether the classification carries the reason code.
func (c Classification) Has(code ReasonCode) bool {
	for _, have := range c.Reasons {
		if have == code {
			return true
		}
	}
	return false
}

// facets are the independent topology signals found in a snapshot. More
// than one at once is mixed.
type facets struct {
	whole, mig, timeSliced, dra bool
}

// Classify makes one deterministic classification from a snapshot.
func Classify(s Snapshot) Classification {
	// The raw DRA observation is never rendered or digested: only its
	// normalized form is, which carries nothing when the observation is
	// invalid or out of bounds.
	dra, draInvalid := normalizeDRA(s.DRA)
	out := Classification{
		Version:      Version,
		Node:         strings.TrimSpace(s.Node),
		NodeUID:      strings.TrimSpace(s.NodeUID),
		ObservedAt:   s.EvaluatedAt.UTC(),
		Evidence:     Evidence{ResourceSource: s.Resources.Source},
		DRA:          DRASummary(dra),
		InputsDigest: inputsDigest(s, dra),
	}
	reasons := map[ReasonCode]struct{}{}
	refuseUnknown := func() { reasons[ReasonTopologyUnknown] = struct{}{} }
	var found facets

	// An incomplete capture is not a node with nothing on it.
	if out.Node == "" || s.EvaluatedAt.IsZero() {
		refuseUnknown()
	}

	// Advertised resources.
	switch s.Resources.Source {
	case ResourceSourceNode:
		whole := resourceValue(s.Resources, ResourceWholeGPU)
		if whole > MaxAdvertisedDevices {
			// Node-written and past any real machine: not knowable capacity.
			refuseUnknown()
		} else if whole > 0 {
			found.whole = true
			out.Evidence.WholeDevices = whole
		}
		// Every nvidia.com/mig-* advertisement is a partition signal, but only
		// an in-bound value is a count. A value past MaxAdvertisedDevices, or
		// a sum of values that would pass it, is not knowable capacity: the
		// partition refusal stands, the classification is unknown, and the
		// total is not rendered at all, exactly like an oversized whole-GPU
		// count. A value is only ever added once both it and the running total
		// are inside the bound, so however many names a node writes the sum
		// cannot wrap.
		var migTotal int64
		migInvalid := false
		for name := range unionNames(s.Resources) {
			if !strings.HasPrefix(name, resourceMIGPrefix) {
				continue
			}
			n := resourceValue(s.Resources, name)
			if n <= 0 {
				continue
			}
			found.mig = true
			if n > MaxAdvertisedDevices || n > MaxAdvertisedDevices-migTotal {
				migInvalid = true
				continue
			}
			migTotal += n
		}
		if migInvalid {
			refuseUnknown()
		} else {
			out.Evidence.MIGInstances = migTotal
		}
		if shared := resourceValue(s.Resources, ResourceSharedGPU); shared > MaxAdvertisedDevices {
			// Shared in a way that is not knowable: the sharing refusal stands,
			// the count is not rendered.
			found.timeSliced = true
			refuseUnknown()
		} else if shared > 0 {
			found.timeSliced = true
			out.Evidence.SharedReplicas = shared
		}
	case ResourceSourceNotApplicable:
		// Nothing to read; the report below is the whole of the evidence. A
		// caller that says so and still hands over an NVIDIA accelerator
		// advertisement contradicts itself, and a contradiction refuses.
		if len(classifiedResources(s.Resources.Capacity))+len(classifiedResources(s.Resources.Allocatable)) > 0 {
			refuseUnknown()
		}
	default:
		refuseUnknown()
	}

	// GPU Feature Discovery labels. Node-written: parsed strictly, clamped,
	// and refused when malformed rather than ignored.
	if raw, present := s.Labels[LabelGPUReplicas]; present {
		out.Evidence.GPUReplicasLabel = boundedLabel(raw)
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		switch {
		case err != nil || n <= 0 || n > MaxAdvertisedDevices:
			reasons[ReasonReplicasLabelInvalid] = struct{}{}
			refuseUnknown()
		case n > 1:
			found.timeSliced = true
		}
	}
	// nvidia.com/mig.strategy is the strategy the device plugin and GFD were
	// started with: how MIG instances WOULD be advertised, not whether any
	// exist. The GPU Operator sets it to "single" by default even on a node
	// whose MIG configuration is all-disabled, so neither "single" nor
	// "mixed" is partition evidence on its own; the partitions themselves are
	// seen as nvidia.com/mig-* resources and in the agent's report (a
	// partitioned safety or partition devices), and those refuse above and
	// below. The label is still read strictly: "none", "single" and "mixed"
	// are the only spellings this version knows, and an empty or unrecognised
	// value is unknown, never "no MIG". The rendered value also decides
	// whether partitions beside a whole-GPU advertisement are the single
	// strategy's renamed instances or a mixed layout.
	if raw, present := s.Labels[LabelMIGStrategy]; present {
		out.Evidence.MIGStrategyLabel = boundedLabel(raw)
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "none", "single", "mixed":
		default:
			reasons[ReasonMigStrategyLabelInvalid] = struct{}{}
			refuseUnknown()
		}
	}
	// nvidia.com/mig.capable says whether the GPU can be partitioned, not
	// whether it is; it never grants anything, and a spelling outside
	// true/false is a malformed GFD signal and refuses.
	if raw, present := s.Labels[LabelMIGCapable]; present {
		out.Evidence.MIGCapableLabel = boundedLabel(raw)
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true", "false":
		default:
			refuseUnknown()
		}
	}
	if raw, present := s.Labels[LabelSharingStrategy]; present {
		out.Evidence.SharingStrategyLabel = boundedLabel(raw)
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "none":
		case "time-slicing":
			found.timeSliced = true
		default:
			// Empty, MPS or anything newer: shared in a way this version does
			// not model. Refused as unknown rather than guessed.
			refuseUnknown()
		}
	}

	// The agent's own attestation. Only a FRESH, verified-unpartitioned
	// report that declares at least one physical GPU can support the
	// whole-device LAYOUT; everything else is a refusal of some kind, and
	// each shortfall is named so the operator can see which one. Readiness
	// is a separate fact: it says whether the agent could stand behind a
	// device-scoped action, not what the device layout is. A report the
	// agent degrades because the hypervisor withholds the PCI reset (every
	// cloud VM: measured on EC2 g4dn, see docs/operations.md) still carries
	// a verified, inventoried layout, so the layout is reported and the
	// readiness shortfall refuses device actions with ReportNotReady. The
	// first hardware run of the classifier (2026-11-05) read every such node
	// as unknown, which would have made whole-device unreachable on any
	// virtualized fleet.
	reportVerified := false
	reportReady := false
	if s.Report == nil {
		reasons[ReasonReportMissing] = struct{}{}
	} else {
		maxAge := s.MaxReportAge
		if maxAge <= 0 {
			maxAge = DefaultMaxReportAge
		}
		fresh := !s.Report.ObservedAt.IsZero() && !s.Report.ObservedAt.After(s.EvaluatedAt) &&
			s.EvaluatedAt.Sub(s.Report.ObservedAt) <= maxAge
		ready := s.Report.Readiness == types.AcceleratorReadinessReady
		inventoried := s.Report.PhysicalDevices > 0
		out.Evidence.ReportObservedAt = s.Report.ObservedAt.UTC()
		out.Evidence.ReportTopologySafety = s.Report.TopologySafety
		out.Evidence.ReportReadiness = s.Report.Readiness
		out.Evidence.ReportPhysicalDevices = s.Report.PhysicalDevices
		out.Evidence.ReportPartitions = s.Report.PartitionDevices
		out.Evidence.ReportFresh = fresh
		if !fresh {
			reasons[ReasonReportStale] = struct{}{}
		}
		if !ready {
			reasons[ReasonReportNotReady] = struct{}{}
		}
		if !inventoried {
			reasons[ReasonReportNoPhysicalDevices] = struct{}{}
		}
		switch s.Report.TopologySafety {
		case types.AcceleratorTopologyVerifiedUnpartitioned:
			reportVerified = fresh && inventoried
			reportReady = ready
		case types.AcceleratorTopologyPartitioned:
			found.mig = true
		default:
			// unknown, unsafe, or a value this version does not know.
			refuseUnknown()
		}
		if s.Report.PartitionDevices > 0 {
			found.mig = true
		}
	}

	// Dynamic Resource Allocation. Disabled is a deliberate, recorded choice;
	// anything short of a successful, self-consistent, in-bounds read of
	// resource.k8s.io/v1 AND of the node's Pods is unknown evidence. A beta
	// group/version is never accepted as an observation. Either a slice
	// published for the node or a claim referenced by a Pod on it is DRA in
	// use: a claim with zero slices (the driver publishes elsewhere, or the
	// slices are gone) refuses exactly like a slice with no claims.
	switch {
	case draInvalid:
		reasons[ReasonDraObservationInvalid] = struct{}{}
		refuseUnknown()
	case dra.State == DRADisabled:
	case dra.State == DRAObserved:
		if dra.ResourceSlices > 0 || dra.ClaimPods > 0 {
			found.dra = true
		}
	default:
		refuseUnknown()
	}

	// Capacity says partitioned, the agent says it verified the opposite: a
	// contradiction, not a tie to break.
	if reportVerified && out.Evidence.MIGInstances > 0 {
		refuseUnknown()
	}

	// On the node-source whole-device path the advertisement and the
	// attestation must count the same devices. The device plugin advertises
	// physical x replicas under nvidia.com/gpu when it time-slices without
	// renaming, and a node whose GPU Feature Discovery labels are absent
	// (GFD not deployed, labels stripped, a plain device-plugin install)
	// leaves that surplus as the only trace of the sharing; the same
	// disagreement in the other direction (fewer advertised than attested)
	// is a device the plugin stopped advertising, or an advertisement the
	// agent did not see. Neither is a whole device the report vouches for,
	// so the whole-device claim is refused as unknown and the contradiction
	// is named. The cause is not inferred: the classifier does not call this
	// time-sliced, because nothing observed says so. A partition, sharing or
	// DRA facet beside the advertisement already refuses with its own
	// concrete code, and under those the counts are expected to differ, so
	// the test applies only where whole-device would otherwise be the answer.
	if reportVerified && found.whole && !found.mig && !found.timeSliced && !found.dra &&
		out.Evidence.WholeDevices != int64(s.Report.PhysicalDevices) {
		reasons[ReasonWholeDeviceCountMismatch] = struct{}{}
		refuseUnknown()
	}

	if found.mig {
		reasons[ReasonMigPartitioned] = struct{}{}
	}
	if found.timeSliced {
		reasons[ReasonTimeSliced] = struct{}{}
	}
	if found.dra {
		reasons[ReasonDraManaged] = struct{}{}
	}

	_, unknown := reasons[ReasonTopologyUnknown]
	switch {
	case unknown:
		out.Topology = TopologyUnknown
	case found.dra && (found.whole || found.mig || found.timeSliced):
		out.Topology = TopologyMixed
	case found.dra:
		out.Topology = TopologyDRAManaged
	case found.mig && found.timeSliced:
		out.Topology = TopologyMixed
	case found.mig && found.whole && strings.EqualFold(strings.TrimSpace(out.Evidence.MIGStrategyLabel), "single"):
		// Under the single strategy the plugin advertises the instances under
		// the whole-GPU name; the report is what says they are partitions.
		out.Topology = TopologyMIG
	case found.mig && found.whole:
		out.Topology = TopologyMixed
	case found.mig:
		out.Topology = TopologyMIG
	case found.timeSliced:
		// Time-slicing without renaming advertises physical x replicas under
		// the whole-GPU name, so whole-device evidence beside it is expected.
		out.Topology = TopologyTimeSliced
	case found.whole && reportVerified:
		// The count-agreement test above has already turned a disagreeing
		// advertisement into unknown; here the two count the same devices.
		out.Topology = TopologyWholeDevice
	case s.Resources.Source == ResourceSourceNotApplicable && reportVerified:
		// No advertisement exists on this platform; the fresh attestation
		// with a physical inventory is the evidence, exactly as the
		// pre-existing gate already required.
		out.Topology = TopologyWholeDevice
	default:
		out.Topology = TopologyUnknown
		refuseUnknown()
	}
	if out.Topology == TopologyMixed {
		reasons[ReasonMixedTopology] = struct{}{}
	}

	out.Reasons = canonicalReasons(reasons)
	_, notReady := reasons[ReasonReportNotReady]
	switch {
	case out.Topology == TopologyWholeDevice && len(out.Reasons) == 0:
		out.DeviceActions = DeviceActionsEligible
	case out.Topology == TopologyWholeDevice && !reportReady && notReady && len(out.Reasons) == 1:
		// A whole-device layout the agent attested but could not stand
		// behind for a device-scoped action: the layout is known and is
		// reported, so a preview can still say "one whole GPU, per-pod
		// attribution", and the action is refused by name. ReportNotReady
		// is the one shortfall that can stand beside a verified layout;
		// every other reason has already turned the layout into something
		// else or into unknown.
		out.DeviceActions = DeviceActionsRefused
	default:
		out.DeviceActions = DeviceActionsRefused
		if out.Topology == TopologyWholeDevice {
			// Whole-device layout but a refusal is pending that is not the
			// readiness shortfall above: the layout is known, the authority
			// is not, and unknown is the honest answer.
			out.Topology = TopologyUnknown
			reasons[ReasonTopologyUnknown] = struct{}{}
			out.Reasons = canonicalReasons(reasons)
		}
	}
	out.Attribution = attributionFor(out.Topology)
	out.Summary = summary(out)
	return out
}

func attributionFor(t Topology) Attribution {
	switch t {
	case TopologyWholeDevice:
		return AttributionPerPod
	case TopologyMIG, TopologyTimeSliced, TopologyMixed:
		// A fault on the physical device reaches every instance or replica
		// on it; nothing here can be attributed to one pod.
		return AttributionDeviceWide
	default:
		return AttributionUnknown
	}
}

func summary(c Classification) string {
	if len(c.Reasons) == 0 {
		return fmt.Sprintf("topology=%s device_actions=%s attribution=%s", c.Topology, c.DeviceActions, c.Attribution)
	}
	codes := make([]string, 0, len(c.Reasons))
	for _, code := range c.Reasons {
		codes = append(codes, string(code))
	}
	return fmt.Sprintf("topology=%s device_actions=%s attribution=%s reasons=%s",
		c.Topology, c.DeviceActions, c.Attribution, strings.Join(codes, ","))
}

func canonicalReasons(set map[ReasonCode]struct{}) []ReasonCode {
	out := make([]ReasonCode, 0, len(set))
	for code := range set {
		out = append(out, code)
	}
	sort.Slice(out, func(i, j int) bool { return reasonOrder[out[i]] < reasonOrder[out[j]] })
	return out
}

// resourceValue is the larger of the capacity and allocatable advertisement,
// clamped below at zero. Taking the larger is the conservative direction: a
// partition advertised in either list exists.
func resourceValue(r Resources, name string) int64 {
	value := max(r.Capacity[name], r.Allocatable[name])
	if value < 0 {
		return 0
	}
	return value
}

func unionNames(r Resources) map[string]struct{} {
	names := make(map[string]struct{}, len(r.Capacity)+len(r.Allocatable))
	for name := range r.Capacity {
		names[name] = struct{}{}
	}
	for name := range r.Allocatable {
		names[name] = struct{}{}
	}
	return names
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// boundedLabel is the only way a node-written label value reaches a
// classification: trimmed, cut to MaxLabelEvidence characters, and with
// anything outside printable ASCII replaced, so a hostile or merely huge
// value can neither bloat nor smuggle anything into an API answer or an
// audit record.
func boundedLabel(raw string) string {
	trimmed := strings.TrimSpace(raw)
	var b strings.Builder
	for i, r := range trimmed {
		if i >= MaxLabelEvidence {
			b.WriteString("...")
			break
		}
		if r < 0x20 || r > 0x7e {
			b.WriteByte('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// normalizeDRA is the closed-set, bounds and sanity test on a DRA
// observation, and the only form of one that reaches a classification or
// the inputs digest. Observed is accepted only from exactly DRAAPIVersion,
// with the node's Pods inspected, every count in [0, MaxDRACount], at most
// MaxDRADrivers distinct valid driver names, slice evidence that adds up
// (every slice names exactly one driver, so no slice means no devices and
// no drivers, and a slice means at least one driver and never more distinct
// drivers than slices; a slice with no devices is legitimate), and claim
// counts that add up (a Pod counted as claiming must carry a reference and
// vice versa, and no more Pods can claim than there are references). None of
// the counts is trusted on its own: a devices or drivers figure with no
// slice behind it is unexplained evidence, not use. Disabled must carry nothing at
// all: a disabled read that reports slices or claims is a caller bug, not
// evidence. Not-observed, the zero value and any state outside the closed
// set refuse regardless and keep only a bounded reason. An invalid
// observation (second result true) comes back as not-observed with a reason
// naming the failed check and nothing else, so an out-of-bounds count or a
// hostile driver string is never rendered or hashed.
func normalizeDRA(o DRAObservation) (DRAObservation, bool) {
	invalid := func(reason string) (DRAObservation, bool) {
		return DRAObservation{State: DRANotObserved, Reason: reason}, true
	}
	switch o.State {
	case DRADisabled:
		if o.APIVersion != "" || o.ResourceSlices != 0 || o.Devices != 0 || len(o.Drivers) != 0 ||
			o.PodsInspected || o.ClaimPods != 0 || o.DirectClaims != 0 || o.TemplateClaims != 0 {
			return invalid("disabled-with-data")
		}
		return DRAObservation{State: DRADisabled}, false
	case DRAObserved:
		if o.APIVersion != DRAAPIVersion {
			return invalid("api-version-invalid")
		}
		if !o.PodsInspected {
			return invalid("pods-not-inspected")
		}
		for _, n := range []int{o.ResourceSlices, o.Devices, o.ClaimPods, o.DirectClaims, o.TemplateClaims} {
			if n < 0 || n > MaxDRACount {
				return invalid("count-out-of-bounds")
			}
		}
		if len(o.Drivers) > MaxDRADrivers {
			return invalid("too-many-drivers")
		}
		drivers := sortedCopy(o.Drivers)
		for i, driver := range drivers {
			if !validDRADriver(driver) || (i > 0 && drivers[i-1] == driver) {
				return invalid("driver-invalid")
			}
		}
		// Slice evidence must explain itself: a device or a driver can only
		// have been counted from a slice, and every slice carries exactly
		// one driver. A zero-device slice is a legitimate shape and passes.
		if o.ResourceSlices == 0 && (o.Devices != 0 || len(drivers) != 0) {
			return invalid("slice-counts-inconsistent")
		}
		if o.ResourceSlices > 0 && (len(drivers) == 0 || len(drivers) > o.ResourceSlices) {
			return invalid("slice-drivers-inconsistent")
		}
		// Both terms are within MaxDRACount here, so the sum cannot overflow.
		references := o.DirectClaims + o.TemplateClaims
		if (o.ClaimPods == 0) != (references == 0) || o.ClaimPods > references {
			return invalid("claim-counts-inconsistent")
		}
		return DRAObservation{
			State: DRAObserved, APIVersion: DRAAPIVersion,
			ResourceSlices: o.ResourceSlices, Devices: o.Devices, Drivers: drivers,
			PodsInspected: true, ClaimPods: o.ClaimPods, DirectClaims: o.DirectClaims, TemplateClaims: o.TemplateClaims,
		}, false
	default:
		return DRAObservation{State: DRANotObserved, Reason: boundedReason(o.Reason)}, false
	}
}

// validDRADriver is the shape the API enforces on ResourceSlice spec.driver:
// a non-empty DNS-1123 subdomain of at most MaxDRADriverLength bytes, that
// is, one or more labels joined by single dots, each label 1 to
// maxDNSLabelLength bytes of lowercase letters, digits and '-', starting and
// ending with a letter or digit. It is checked here byte by byte rather than
// through the API machinery so the package stays free of Kubernetes
// dependencies; an empty label (a leading, trailing or doubled dot) or a
// hyphen beside a dot fails exactly as the API would fail it. Nothing else
// is a driver name, and nothing else is rendered or hashed as one.
func validDRADriver(name string) bool {
	if name == "" || len(name) > MaxDRADriverLength {
		return false
	}
	start := 0
	for i := 0; i <= len(name); i++ {
		if i < len(name) && name[i] != '.' {
			continue
		}
		if !validDNSLabel(name[start:i]) {
			return false
		}
		start = i + 1
	}
	return true
}

// maxDNSLabelLength is the DNS-1123 bound on one label of a subdomain.
const maxDNSLabelLength = 63

// validDNSLabel is one DNS-1123 label: 1 to maxDNSLabelLength bytes of
// lowercase letters, digits and '-', with a letter or digit at both ends.
func validDNSLabel(label string) bool {
	if label == "" || len(label) > maxDNSLabelLength {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(label)-1:
		default:
			return false
		}
	}
	return true
}

// boundedReason is the only way a platform's not-observed reason reaches a
// classification: a token of at most MaxDRAReasonLength lowercase letters,
// digits and hyphens is kept as is, an empty reason stays empty, and
// anything else is replaced by "invalid-reason" rather than echoed.
func boundedReason(raw string) string {
	if len(raw) > MaxDRAReasonLength {
		return "invalid-reason"
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return "invalid-reason"
		}
	}
	return raw
}

// digestInputs is the canonical form hashed into InputsDigest. Field order is
// fixed by the struct, map keys are sorted by encoding/json, times are UTC
// RFC 3339 with nanoseconds, and only the classified NVIDIA resources, the
// known labels and the normalized DRA observation are included, so an
// unrelated label or resource, or an unbounded DRA string, cannot enter it.
type digestInputs struct {
	Version        string            `json:"version"`
	Node           string            `json:"node"`
	NodeUID        string            `json:"node_uid"`
	ResourceSource ResourceSource    `json:"resource_source"`
	Capacity       map[string]int64  `json:"capacity"`
	Allocatable    map[string]int64  `json:"allocatable"`
	Labels         map[string]string `json:"labels"`
	Report         *digestReport     `json:"report"`
	MaxReportAgeNS int64             `json:"max_report_age_ns"`
	DRA            digestDRA         `json:"dra"`
}

type digestReport struct {
	ObservedAt       string `json:"observed_at"`
	TopologySafety   string `json:"topology_safety"`
	Readiness        string `json:"readiness"`
	PhysicalDevices  int    `json:"physical_devices"`
	PartitionDevices int    `json:"partition_devices"`
}

type digestDRA struct {
	State          string   `json:"state"`
	Reason         string   `json:"reason"`
	APIVersion     string   `json:"api_version"`
	ResourceSlices int      `json:"resource_slices"`
	Devices        int      `json:"devices"`
	Drivers        []string `json:"drivers"`
	PodsInspected  bool     `json:"pods_inspected"`
	ClaimPods      int      `json:"claim_pods"`
	DirectClaims   int      `json:"direct_claims"`
	TemplateClaims int      `json:"template_claims"`
}

// inputsDigest hashes the snapshot with the DRA observation already
// normalized by normalizeDRA; the raw observation never enters the digest.
func inputsDigest(s Snapshot, dra DRAObservation) string {
	in := digestInputs{
		Version:        Version,
		Node:           strings.TrimSpace(s.Node),
		NodeUID:        strings.TrimSpace(s.NodeUID),
		ResourceSource: s.Resources.Source,
		Capacity:       classifiedResources(s.Resources.Capacity),
		Allocatable:    classifiedResources(s.Resources.Allocatable),
		Labels:         map[string]string{},
		MaxReportAgeNS: int64(s.MaxReportAge),
		DRA: digestDRA{
			State: string(dra.State), Reason: dra.Reason, APIVersion: dra.APIVersion,
			ResourceSlices: dra.ResourceSlices, Devices: dra.Devices, Drivers: dra.Drivers,
			PodsInspected: dra.PodsInspected, ClaimPods: dra.ClaimPods,
			DirectClaims: dra.DirectClaims, TemplateClaims: dra.TemplateClaims,
		},
	}
	if in.DRA.Drivers == nil {
		in.DRA.Drivers = []string{}
	}
	for _, key := range []string{LabelGPUReplicas, LabelMIGStrategy, LabelMIGCapable, LabelSharingStrategy} {
		if value, present := s.Labels[key]; present {
			in.Labels[key] = value
		}
	}
	if s.Report != nil {
		in.Report = &digestReport{
			ObservedAt:       s.Report.ObservedAt.UTC().Format(time.RFC3339Nano),
			TopologySafety:   string(s.Report.TopologySafety),
			Readiness:        string(s.Report.Readiness),
			PhysicalDevices:  s.Report.PhysicalDevices,
			PartitionDevices: s.Report.PartitionDevices,
		}
	}
	blob, err := json.Marshal(in)
	if err != nil {
		// Unreachable for this fixed shape; refuse to fabricate a digest.
		return ""
	}
	sum := sha256.Sum256(blob)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// classifiedResources keeps only the names the classifier reads: the whole
// and shared NVIDIA GPU resources and the MIG instance resources. A core
// resource or another vendor's qualified name is neither evidence nor part
// of the digest.
func classifiedResources(in map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for name, value := range in {
		if name == ResourceWholeGPU || name == ResourceSharedGPU || strings.HasPrefix(name, resourceMIGPrefix) {
			out[name] = value
		}
	}
	return out
}
