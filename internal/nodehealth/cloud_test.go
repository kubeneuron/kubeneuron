// SPDX-License-Identifier: Apache-2.0

package nodehealth

import (
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var cloudNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func tpuSnap(points ...types.CloudHealthPoint) types.NodeHealthSnapshot {
	return types.NodeHealthSnapshot{
		Node: "tpu-1", ReadAt: cloudNow, CreatedAt: cloudNow.Add(-24 * time.Hour),
		Devices: []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Allocatable: 4}},
		Cloud:   points,
	}
}

// pt is a point ago before now.
func pt(ago time.Duration, status, category string) types.CloudHealthPoint {
	return types.CloudHealthPoint{At: cloudNow.Add(-ago), Status: status, Category: category}
}

func TestEvaluateCloud(t *testing.T) {
	cases := []struct {
		name   string
		snap   types.NodeHealthSnapshot
		state  State
		class  types.ProblemClass
		reason string
		why    string
	}{
		{name: "healthy", snap: tpuSnap(pt(3*time.Minute, "HEALTHY", "")), state: StateHealthy, reason: "HEALTHY"},
		{name: "bad chip on two samples", snap: tpuSnap(
			pt(4*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP"), pt(3*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP")),
			state: StateFault, class: types.ClassAcceleratorFault, reason: "BAD_TPU_CHIP"},
		{name: "bad SC chip on two samples", snap: tpuSnap(
			pt(4*time.Minute, "UNHEALTHY", "BAD_SC_CHIP"), pt(3*time.Minute, "UNHEALTHY", "BAD_SC_CHIP")),
			state: StateFault, class: types.ClassAcceleratorFault, reason: "BAD_SC_CHIP"},
		{name: "bad chip on one sample", snap: tpuSnap(
			pt(4*time.Minute, "HEALTHY", ""), pt(3*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP")),
			state: StateUnknown, reason: "BAD_TPU_CHIP", why: "a fault needs 2"},
		{name: "one sample read twice is one sample", snap: tpuSnap(
			pt(3*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP"), pt(3*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP")),
			state: StateUnknown, why: "a fault needs 2"},
		{name: "another category breaks the run", snap: tpuSnap(
			pt(4*time.Minute, "UNHEALTHY", "BAD_SC_CHIP"), pt(3*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP")),
			state: StateUnknown, why: "a fault needs 2"},
		{name: "networking issue", snap: tpuSnap(
			pt(4*time.Minute, "UNHEALTHY", "NETWORKING_ISSUE"), pt(3*time.Minute, "UNHEALTHY", "NETWORKING_ISSUE")),
			state: StateIgnored, reason: "networking-issue"},
		{name: "degraded", snap: tpuSnap(pt(3*time.Minute, "DEGRADED", "")), state: StateIgnored, reason: "degraded"},
		{name: "unknown status", snap: tpuSnap(pt(3*time.Minute, "UNKNOWN", "")), state: StateUnknown, why: `"UNKNOWN"`},
		{name: "undocumented category", snap: tpuSnap(
			pt(4*time.Minute, "UNHEALTHY", "BAD_FAN"), pt(3*time.Minute, "UNHEALTHY", "BAD_FAN")),
			state: StateUnknown, why: "not in profile table"},
		{name: "unhealthy without a category", snap: tpuSnap(
			pt(4*time.Minute, "UNHEALTHY", ""), pt(3*time.Minute, "UNHEALTHY", "")),
			state: StateUnknown, why: "not in profile table"},
		{name: "two statuses at the newest time", snap: tpuSnap(
			pt(3*time.Minute, "HEALTHY", ""), pt(3*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP")),
			state: StateUnknown, why: "two health statuses"},
		{name: "no point", snap: tpuSnap(), state: StateUnknown, why: "no-series"},
		{name: "stale", snap: tpuSnap(pt(11*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP"), pt(10*time.Minute+time.Second, "UNHEALTHY", "BAD_TPU_CHIP")),
			state: StateUnknown, why: "stale"},
		{name: "read failed", snap: func() types.NodeHealthSnapshot {
			s := tpuSnap(pt(3*time.Minute, "HEALTHY", ""))
			s.CloudError = "permission-denied"
			return s
		}(), state: StateUnknown, why: "permission-denied"},
		{name: "points of the instance before the node", snap: func() types.NodeHealthSnapshot {
			s := tpuSnap(pt(4*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP"), pt(3*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP"))
			s.CreatedAt = cloudNow.Add(-3*time.Minute - time.Second)
			return s
		}(), state: StateUnknown, why: "a fault needs 2"},
		{name: "a node newer than every point", snap: func() types.NodeHealthSnapshot {
			s := tpuSnap(pt(3*time.Minute, "HEALTHY", ""))
			s.CreatedAt = cloudNow.Add(-time.Minute)
			return s
		}(), state: StateUnknown, why: "no-series"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := EvaluateCloud(tc.snap)
			if len(h.Observations) != 1 {
				t.Fatalf("observations %+v, want one", h.Observations)
			}
			o := h.Observations[0]
			if o.Profile != "google-tpu" || o.Rule != RuleGCPInfraHealth {
				t.Errorf("observation %+v: want profile google-tpu, rule %s", o, RuleGCPInfraHealth)
			}
			if o.State != tc.state || o.Class != tc.class {
				t.Errorf("state %s class %q, want %s %q (why %q)", o.State, o.Class, tc.state, tc.class, o.Why)
			}
			if tc.reason != "" && o.Reason != tc.reason {
				t.Errorf("reason %q, want %q", o.Reason, tc.reason)
			}
			if !strings.Contains(o.Why, tc.why) {
				t.Errorf("why %q, want it to say %q", o.Why, tc.why)
			}
			if o.Reads != 1 {
				t.Errorf("reads %d: the points confirm the fault, not repeated reads", o.Reads)
			}
			want := tc.state
			if want == StateIgnored {
				want = StateHealthy
			}
			if h.State != want {
				t.Errorf("node state %s, want %s", h.State, want)
			}
		})
	}
}

// A fault keeps one key while it lasts, and the unknown key of the rule is
// its prefix, so an unknown read holds the pending fault.
func TestEvaluateCloudFaultKey(t *testing.T) {
	fault := EvaluateCloud(tpuSnap(pt(4*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP"), pt(3*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP"))).Observations[0]
	later := EvaluateCloud(tpuSnap(pt(3*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP"), pt(2*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP"),
		pt(time.Minute, "UNHEALTHY", "BAD_TPU_CHIP"))).Observations[0]
	unknown := EvaluateCloud(tpuSnap()).Observations[0]
	if fault.Key != later.Key || fault.Key != "gcp-infra-health=UNHEALTHY/BAD_TPU_CHIP" {
		t.Errorf("keys %q, %q", fault.Key, later.Key)
	}
	if !strings.HasPrefix(fault.Key, unknown.Key) {
		t.Errorf("unknown key %q is not a prefix of the fault key %q", unknown.Key, fault.Key)
	}
	if !later.Since.Equal(cloudNow.Add(-3 * time.Minute)) {
		t.Errorf("since %v, want the first point of the run", later.Since)
	}
}

// One host of a multi-host slice is reported as such, so recovery is refused.
func TestEvaluateCloudMultiHostSlice(t *testing.T) {
	s := tpuSnap(pt(3*time.Minute, "HEALTHY", ""))
	s.Labels = map[string]string{labelTPUTopology: "4x4", labelGKENodePool: "slice-a"}
	if h := EvaluateCloud(s); h.Slice != "slice-a" {
		t.Errorf("slice %q, want slice-a", h.Slice)
	}
	// With the TPU count gone, the node may still be one host of many.
	s.Devices = nil
	if h := EvaluateCloud(s); h.Slice != "slice-a" {
		t.Errorf("without a TPU count: slice %q, want slice-a", h.Slice)
	}
	s.Labels = map[string]string{labelTPUTopology: "2x2", labelGKENodePool: "single"}
	s.Devices = []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Allocatable: 4}}
	if h := EvaluateCloud(s); h.Slice != "" {
		t.Errorf("single host: slice %q, want none", h.Slice)
	}
}
