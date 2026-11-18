// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The window per accelerator resource: NVIDIA and AMD apart in GPU-hours, TPU
// chip-hours, AWS Neuron device-hours, node-hours for an unknown count, cost
// for the two resources with a rate and no total. KUBENEURON_UPDATE_GOLDEN=1
// writes the golden file.
func TestReportAcceleratorHoursGolden(t *testing.T) {
	baseline := 4 * 3600.0
	cost := func(rate, hours float64) *types.CostEstimate {
		return &types.CostEstimate{Currency: "USD", RatePerHour: rate, Amount: rate * hours}
	}
	row := func(resource string, n int, degraded, recovered, saved float64, c *types.CostEstimate) types.AcceleratorResourceReport {
		return types.AcceleratorResourceReport{Resource: resource, Incidents: n, DegradedHours: degraded, RecoveredHours: recovered,
			SavedEstimate: &types.AcceleratorSavedEstimate{Hours: saved, ManualBaselineSeconds: baseline, Cost: c}}
	}
	h := types.AcceleratorHours{
		Groups: []types.AcceleratorGroupReport{
			{Group: "gpu", Unit: "GPU-hours", Resources: []types.AcceleratorResourceReport{
				row("amd.com/gpu", 1, 10, 8, 4, nil),
				row("nvidia.com/gpu", 2, 20, 16, 8, cost(4, 8)),
			}},
			{Group: "tpu", Unit: "TPU chip-hours", Resources: []types.AcceleratorResourceReport{row("google.com/tpu", 1, 12, 8, 4, nil)}},
			{Group: "aws-neuron", Unit: "AWS Neuron device-hours", Resources: []types.AcceleratorResourceReport{row("aws.amazon.com/neuron", 1, 8, 6, 2, cost(1.5, 2))}},
		},
		SavedCostTotal:     &types.CostEstimateTotal{Currency: "USD", RateNotSetFor: []string{"amd.com/gpu", "google.com/tpu"}},
		DeviceCountUnknown: &types.DeviceCountUnknown{Incidents: 2, DegradedNodeHours: 6, RecoveredNodeHours: 4},
	}
	var out bytes.Buffer
	if err := printAcceleratorHours(&out, h, ""); err != nil {
		t.Fatal(err)
	}
	// Without a baseline: no saved hours and no cost.
	h.Groups = h.Groups[1:2]
	h.Groups[0].Resources[0].SavedEstimate = nil
	h.SavedCostTotal, h.DeviceCountUnknown = nil, nil
	if err := printAcceleratorHours(&out, h, ""); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "report_accelerators.golden", out.Bytes())
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	golden := filepath.Join("testdata", name)
	if os.Getenv("KUBENEURON_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("output differs from %s:\n%s", golden, got)
	}
}

func TestBaselineAndIncidentsText(t *testing.T) {
	for v, want := range map[float64]string{14400: "4h", 5400: "1h30m", 1800: "30m", 45: "45s", 3661: "1h1m1s", 0: "-"} {
		if got := baseline(v); got != want {
			t.Errorf("baseline(%v) = %q, want %q", v, got, want)
		}
	}
	if incidents(1) != "1 incident" || incidents(2) != "2 incidents" {
		t.Fatal(incidents(1), incidents(2))
	}
}

// The whole report: GPU-hour lines only for a window of one GPU resource;
// any other window has its hours per resource and nowhere else.
func TestRecoveryReportTextGolden(t *testing.T) {
	to := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	baseline := 4 * 3600.0
	p50 := 1800.0
	// The numbers add up to the report's: 2 incidents, 20 degraded, 8
	// recovered, 3 saved.
	resource := func(name string, n int, degraded, recovered, saved float64) types.AcceleratorResourceReport {
		return types.AcceleratorResourceReport{Resource: name, Incidents: n, DegradedHours: degraded, RecoveredHours: recovered,
			SavedEstimate: &types.AcceleratorSavedEstimate{Hours: saved, ManualBaselineSeconds: baseline}}
	}
	gpu := func(rs ...types.AcceleratorResourceReport) types.AcceleratorGroupReport {
		return types.AcceleratorGroupReport{Group: types.AcceleratorGroupGPU, Unit: "GPU-hours", Resources: rs}
	}
	report := func(h types.AcceleratorHours, n int) *types.RecoveryReport {
		return &types.RecoveryReport{
			From: to.Add(-168 * time.Hour), To: to, Incidents: n, Recovered: 1, RecoveredUnattended: 1, ObservedOnly: 1,
			DegradedGPUHours: 20, RecoveredGPUHours: 8, ObservedOnlyGPUHours: 2, Superseded: 0,
			MTTR:             types.RecoveryLatency{Samples: 1, P50Seconds: p50, P90Seconds: p50, MeanSeconds: p50},
			SavingsEstimate:  &types.SavingsEstimate{GPUHoursSaved: 3, ManualBaselineSeconds: baseline},
			IncidentDetails:  []types.RecoveryIncident{{ID: "inc-1", Node: "node-a", Vendor: "nvidia", State: types.IncidentState("RESOLVED"), DegradedGPUHours: 8, RecoverySeconds: &p50}},
			Classes:          []types.RecoveryClassReport{{Class: "xid-79", Incidents: n, DegradedGPUHours: 20, RecoveredGPUHours: 8, ObservedOnly: 1, RecoveredUnattended: 1, MTTR: types.RecoveryLatency{P50Seconds: p50, P90Seconds: p50}}},
			Open:             []types.RecoveryOpenIncident{{ID: "inc-2", Class: "xid-79", Node: "node-b", State: types.IncidentState("OPEN"), OpenedAt: to.Add(-2 * time.Hour), DegradedGPUHours: 2}},
			AcceleratorHours: h,
		}
	}
	tpuOnly := types.AcceleratorHours{Groups: []types.AcceleratorGroupReport{{Group: types.AcceleratorGroupTPU, Unit: "TPU chip-hours",
		Resources: []types.AcceleratorResourceReport{resource("google.com/tpu", 1, 12, 6, 3)}}}}
	dryRun := &types.RecoveryReport{From: to.Add(-168 * time.Hour), To: to, DryRunExcluded: 1, Classes: []types.RecoveryClassReport{}, Open: []types.RecoveryOpenIncident{},
		Simulated: &types.SimulatedRecovery{Incidents: 1, WouldRecover: 1, DegradedGPUHours: 12, WouldRecoverGPUHours: 6,
			MTTR:             types.RecoveryLatency{P50Seconds: p50, P90Seconds: p50},
			WouldHaveSaved:   &types.SavingsEstimate{GPUHoursSaved: 3, ManualBaselineSeconds: baseline},
			IncidentDetails:  []types.RecoveryIncident{{ID: "inc-3", Node: "tpu-node", State: types.IncidentState("RESOLVED"), DegradedGPUHours: 12, RecoverySeconds: &p50}},
			AcceleratorHours: tpuOnly}}
	for _, tc := range []struct {
		name string
		r    *types.RecoveryReport
	}{
		{"mixed", report(types.AcceleratorHours{Groups: []types.AcceleratorGroupReport{gpu(resource("amd.com/gpu", 1, 8, 3, 1), resource("nvidia.com/gpu", 1, 12, 5, 2))}}, 2)},
		{"node_hours", report(types.AcceleratorHours{DeviceCountUnknown: &types.DeviceCountUnknown{Incidents: 2, DegradedNodeHours: 6, RecoveredNodeHours: 4}}, 2)},
		{"tpu_dry_run", dryRun},
		{"nvidia", report(types.AcceleratorHours{Groups: []types.AcceleratorGroupReport{gpu(resource("nvidia.com/gpu", 2, 20, 8, 3))}}, 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			if err := printRecoveryReport(cmd, tc.r, "168h"); err != nil {
				t.Fatal(err)
			}
			if tc.name != "nvidia" && strings.Contains(out.String(), "GPU-hours:") || strings.Contains(out.String(), "GPU-HOURS") && tc.name != "nvidia" {
				t.Errorf("GPU-hour lines outside the table:\n%s", out.String())
			}
			checkGolden(t, "report_text_"+tc.name+".golden", out.Bytes())
		})
	}
}
