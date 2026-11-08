package impact

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/topology"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func policy() checkpoint.Policy {
	return checkpoint.Policy{
		Enabled: true, DefaultWait: 5 * time.Minute, MaxWait: 15 * time.Minute,
		SkipClasses: checkpoint.DefaultSkipClasses(), Namespaces: []string{"ml-training"},
		Advisory: checkpoint.AdvisoryPolicy{Enabled: true, Budget: 10 * time.Second},
	}
}

func optedIn(ns, name string, gpu bool, extra map[string]string) platform.Workload {
	ann := map[string]string{checkpoint.AnnotationOptIn: checkpoint.OptInValue}
	for k, v := range extra {
		ann[k] = v
	}
	return platform.Workload{Namespace: ns, Name: name, Kind: "Pod", UID: "uid-" + name, UsesGPU: gpu, Annotations: ann}
}

func input() Input {
	topo := &topology.Classification{
		Version: "topology-classification/v1", Node: "n1", Topology: topology.TopologyWholeDevice,
		DeviceActions: topology.DeviceActionsEligible, Attribution: topology.AttributionPerPod,
		Evidence: topology.Evidence{WholeDevices: 1},
	}
	return Input{
		IncidentID: "inc-8f2a", Round: 3, ComputedAt: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC),
		Node: "n1", Topology: topo, PendingStep: "reboot", Downstream: []string{"platform.drain"}, Op: "drain",
		Workloads: []platform.Workload{
			optedIn("ml-training", "trainer-3", true, map[string]string{checkpoint.AnnotationMaxWait: "8m", checkpoint.AnnotationGroup: "run-42"}),
			optedIn("ml-training", "trainer-4", true, map[string]string{checkpoint.AnnotationGroup: "run-42"}),
			{Namespace: "ml-training", Name: "sidecar", Kind: "Pod", UID: "uid-sidecar"},
			optedIn("research", "notebook", false, nil),
			{Namespace: "kube-system", Name: "agent", Kind: "Pod", UID: "uid-agent", DrainExclusion: platform.DrainExclusionInfrastructure},
		},
		Checkpoint: policy(), Class: types.ClassECCDBE,
		Groups:      &GroupPolicy{MaxGroupSize: 64},
		GroupTotals: map[string]int{"ml-training/run-42": 2},
		MaxItems:    DefaultMaxItems,
	}
}

// A group the controller could not list has an UNKNOWN total: the on-node
// count stands in as a lower bound, flagged, never dressed as the answer.
func TestGroupTotalUnknownIsSaidNotInvented(t *testing.T) {
	in := input()
	in.GroupTotals = nil
	p := Compute(in)
	if len(p.Workloads.Groups) != 1 {
		t.Fatalf("groups = %+v", p.Workloads.Groups)
	}
	g := p.Workloads.Groups[0]
	if g.MembersTotalKnown || g.MembersTotal != 2 || g.MembersOnNode != 2 {
		t.Fatalf("group = %+v, want an unknown total standing at the on-node count", g)
	}
	if !contains(p.Warnings, WarningGroupTotalUnknown) {
		t.Fatalf("warnings = %v, want %s", p.Warnings, WarningGroupTotalUnknown)
	}
	in.GroupTotals = map[string]int{"ml-training/run-42": 8}
	p = Compute(in)
	if g := p.Workloads.Groups[0]; !g.MembersTotalKnown || g.MembersTotal != 8 || contains(p.Warnings, WarningGroupTotalUnknown) {
		t.Fatalf("group = %+v warnings = %v, want a known total of 8", g, p.Warnings)
	}
}

// A round with no coordinating disruption downstream advertises nobody as
// eligible, and the effective wait shown is the step's, not the policy's.
func TestEligibilityFollowsTheDisruptionAndItsBudget(t *testing.T) {
	in := input()
	in.Op, in.Downstream = "", nil
	p := Compute(in)
	if p.Workloads.EligibleForCheckpoint != 0 || p.Workloads.Ineligible[string(checkpoint.IneligibleNoDisruption)] == 0 {
		t.Fatalf("no disruption downstream: eligible %d ineligible %v", p.Workloads.EligibleForCheckpoint, p.Workloads.Ineligible)
	}
	in = input()
	in.StepBudget = 90 * time.Second
	p = Compute(in)
	for _, it := range p.Workloads.Items {
		if it.Checkpoint.Eligible && it.Checkpoint.EffectiveWait != "1m30s" {
			t.Fatalf("%s effective wait = %s, want the step budget 1m30s", it.Name, it.Checkpoint.EffectiveWait)
		}
	}
}

func TestComputeCountsWhatItSawAndNothingElse(t *testing.T) {
	p := Compute(input())
	if p.Version != Version || p.Incident != "inc-8f2a" || p.Round != 3 || p.Unavailable != nil {
		t.Fatalf("identity: %+v", p)
	}
	w := p.Workloads
	if w.Total != 5 || w.GPUHolders != 2 || w.Namespaces != 3 || w.EligibleForCheckpoint != 2 {
		t.Fatalf("counts = total %d gpu %d ns %d eligible %d", w.Total, w.GPUHolders, w.Namespaces, w.EligibleForCheckpoint)
	}
	if w.Ineligible["not-opted-in"] != 1 || w.Ineligible["namespace-not-allowed"] != 1 || w.Ineligible["drain-excluded"] != 1 {
		t.Fatalf("ineligible = %v", w.Ineligible)
	}
	if len(w.Groups) != 1 || w.Groups[0].Name != "run-42" || w.Groups[0].MembersOnNode != 2 || w.Groups[0].MembersTotal != 2 || w.Groups[0].TooLarge {
		t.Fatalf("groups = %+v", w.Groups)
	}
	if len(w.Items) != 5 || w.Items[0].Namespace != "kube-system" {
		t.Fatalf("items must be sorted by namespace/name and complete: %+v", w.Items)
	}
	var trainer3 Item
	for _, it := range w.Items {
		if it.Name == "trainer-3" {
			trainer3 = it
		}
	}
	if !trainer3.Checkpoint.OptedIn || !trainer3.Checkpoint.Eligible || trainer3.Checkpoint.EffectiveWait != "8m0s" || trainer3.Checkpoint.Group != "run-42" {
		t.Fatalf("trainer-3 = %+v", trainer3.Checkpoint)
	}
	if p.Devices.Whole != 1 || p.Devices.Attribution != "per-pod" || p.Node.Topology == nil {
		t.Fatalf("devices = %+v", p.Devices)
	}
	if !p.CheckpointPolicy.Enabled || !p.CheckpointPolicy.Advisory || !p.CheckpointPolicy.Groups || p.CheckpointPolicy.MaxWait != "15m0s" {
		t.Fatalf("policy view = %+v", p.CheckpointPolicy)
	}
	if len(p.Warnings) != 0 || p.Limits.ItemsTruncated {
		t.Fatalf("warnings = %v truncated = %v", p.Warnings, p.Limits.ItemsTruncated)
	}
	// No invented numbers anywhere in the document.
	raw, _ := json.Marshal(p)
	for _, forbidden := range []string{"gpu_hours", "lost", "saved", "progress"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the preview must not estimate %q: %s", forbidden, raw)
		}
	}
}

func TestDigestIsStableAndSensitive(t *testing.T) {
	a, b := Compute(input()), Compute(input())
	if a.Digest == "" || a.Digest != b.Digest {
		t.Fatalf("same snapshot, digests %q vs %q", a.Digest, b.Digest)
	}
	if !strings.HasPrefix(a.Digest, "sha256:") {
		t.Fatalf("digest %q is not sha256-prefixed", a.Digest)
	}
	in := input()
	in.ComputedAt = in.ComputedAt.Add(time.Second)
	if c := Compute(in); c.Digest == a.Digest {
		t.Fatal("a recompute at another time must be a new digest: the human must see it again")
	}
	in = input()
	in.Workloads = in.Workloads[:1]
	if c := Compute(in); c.Digest == a.Digest {
		t.Fatal("a different listing must change the digest")
	}
	// The digest is over the document with an empty digest field: re-sealing
	// a parsed document reproduces it.
	rec, err := Record(a)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(rec)
	if err != nil {
		t.Fatal(err)
	}
	if seal(parsed).Digest != a.Digest {
		t.Fatal("a stored document does not re-seal to its own digest")
	}
}

func TestWarningsNameWhatTheHumanMustKnow(t *testing.T) {
	in := input()
	in.Topology.Topology = topology.TopologyTimeSliced
	in.Topology.DeviceActions = topology.DeviceActionsRefused
	in.Topology.Attribution = topology.AttributionDeviceWide
	in.Topology.Evidence.SharedReplicas = 2
	in.MaxItems = MinMaxItems
	for i := 0; i < 20; i++ {
		in.Workloads = append(in.Workloads, platform.Workload{Namespace: "ml-training", Name: "w" + string(rune('a'+i)), Kind: "Pod", UID: "u" + string(rune('a'+i))})
	}
	in.ContextNotObserved = []string{"kueue"}
	in.Groups.MaxGroupSize = 1
	p := Compute(in)
	for _, want := range []string{WarningTimeSlicedAttribution, WarningTopologyRefused, WarningItemsTruncated, WarningContextNotResolved, WarningGroupTooLarge} {
		if !contains(p.Warnings, want) {
			t.Fatalf("warnings %v lack %s", p.Warnings, want)
		}
	}
	if !p.Limits.ItemsTruncated || len(p.Workloads.Items) != MinMaxItems || p.Workloads.Total != 25 {
		t.Fatalf("truncation: items %d total %d truncated %v", len(p.Workloads.Items), p.Workloads.Total, p.Limits.ItemsTruncated)
	}
	if p.Devices.TimeSlicedReplicas != 2 || p.Devices.Attribution != "device-wide" {
		t.Fatalf("devices = %+v", p.Devices)
	}
	if !p.Workloads.Groups[0].TooLarge {
		t.Fatal("the group above maxGroupSize is not flagged")
	}
}

func TestUnavailableIsExplicitAndSealed(t *testing.T) {
	p := Unavailable(input(), UnavailableListingTimeout, "list n1: context deadline exceeded")
	if p.Unavailable == nil || p.Unavailable.Reason != UnavailableListingTimeout || p.Workloads.Total != 0 || p.Digest == "" {
		t.Fatalf("unavailable = %+v", p)
	}
	rec, err := Record(p)
	if err != nil {
		t.Fatal(err)
	}
	if rec.UnavailableReason != UnavailableListingTimeout || rec.Digest != p.Digest || len(rec.Payload) == 0 {
		t.Fatalf("record = %+v", rec)
	}
	if !strings.HasPrefix(Summary(p), "impact preview unavailable: ") {
		t.Fatalf("summary = %q", Summary(p))
	}
}

func TestRecordBoundsThePayload(t *testing.T) {
	in := input()
	in.MaxItems = MaxMaxItems
	in.Workloads = nil
	for i := 0; i < MaxMaxItems; i++ {
		in.Workloads = append(in.Workloads, platform.Workload{
			Namespace: "ml-training", Name: strings.Repeat("x", 200) + string(rune(i%26+'a')), Kind: "Pod", UID: strings.Repeat("u", 100),
			Annotations: map[string]string{checkpoint.AnnotationOptIn: checkpoint.OptInValue},
		})
	}
	rec, err := Record(Compute(in))
	if err != nil {
		t.Fatal(err)
	}
	if rec.UnavailableReason != UnavailablePayloadTooLarge {
		t.Fatalf("a %d-byte document was stored as %q, want PayloadTooLarge", len(rec.Payload), rec.UnavailableReason)
	}
	if len(rec.Payload) > MaxPayloadBytes {
		t.Fatalf("the unavailable record is itself %d bytes", len(rec.Payload))
	}
}

func TestSummaryIsShort(t *testing.T) {
	s := Summary(Compute(input()))
	lines := strings.Split(s, "\n")
	if len(lines) < 2 || len(lines) > 4 {
		t.Fatalf("summary has %d lines: %q", len(lines), s)
	}
	if !strings.Contains(lines[0], "whole-device") || !strings.Contains(lines[1], "5 total") {
		t.Fatalf("summary = %q", s)
	}
}
