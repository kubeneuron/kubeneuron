// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

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
	golden := filepath.Join("testdata", "report_accelerators.golden")
	if os.Getenv("KUBENEURON_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("output differs from %s:\n%s", golden, out.String())
	}
}
