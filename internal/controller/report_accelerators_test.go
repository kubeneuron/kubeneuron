// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func withDevices(inc *types.Incident, resource string, unit types.DeviceUnit, count int64) *types.Incident {
	inc.Accelerators = []types.IncidentAccelerator{{Resource: resource, Unit: unit, Count: count}}
	return inc
}

func resourceRow(t *testing.T, h types.AcceleratorHours, group, resource string) types.AcceleratorResourceReport {
	t.Helper()
	for _, g := range h.Groups {
		if g.Group != group {
			continue
		}
		for _, r := range g.Resources {
			if r.Resource == resource {
				return r
			}
		}
	}
	t.Fatalf("no %s row in group %s: %+v", resource, group, h.Groups)
	return types.AcceleratorResourceReport{}
}

// A fleet of NVIDIA and AMD GPUs, TPU chips and AWS Neuron devices: each
// resource in its own unit, no sum across them, overlapping recoveries on one
// node counted once, and GPU-hours that count GPUs only. An incident with no
// recorded count is node-hours apart. Cost only where a rate is set, and no
// total while a rate is missing.
func TestRecoveryReportPerAcceleratorResource(t *testing.T) {
	to := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	from := to.Add(-24 * time.Hour)
	nv := withDevices(incident("nv", types.ClassFellOffBus, "gpu-1", "", types.StateResolved, at(to, -10), at(to, -9)), "nvidia.com/gpu", types.DeviceUnitGPU, 8)
	// The same node's GPU, recovering inside the node-wide one: counted once.
	nvDev := withDevices(incident("nv-dev", types.ClassECCDBE, "gpu-1", "GPU-1", types.StateResolved, at(to, -10), at(to, -9.5)), "nvidia.com/gpu", types.DeviceUnitGPU, 1)
	amd := withDevices(incident("amd", types.ClassFellOffBus, "mi-1", "", types.StateResolved, at(to, -6), at(to, -5)), "amd.com/gpu", types.DeviceUnitGPU, 8)
	tpu := withDevices(incident("tpu", types.ClassAcceleratorFault, "tpu-1", "", types.StateResolved, at(to, -4), at(to, -2)), "google.com/tpu", types.DeviceUnitTPUChip, 4)
	trn := withDevices(incident("trn", types.ClassAcceleratorFault, "trn-1", "", types.StateResolved, at(to, -3), at(to, -2)), "aws.amazon.com/neuron", types.DeviceUnitAWSDevice, 16)
	unknown := incident("old", types.ClassFellOffBus, "old-1", "", types.StateResolved, at(to, -8), at(to, -6))
	incidents := []*types.Incident{nv, nvDev, amd, tpu, trn, unknown}
	inv := reportInventory{
		gpusPerNode: map[string]int{"gpu-1": 8, "mi-1": 8},
		nodes: map[string]*types.Node{
			"tpu-1": {Name: "tpu-1", Devices: []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Unit: types.DeviceUnitTPUChip}}},
			"trn-1": {Name: "trn-1", Devices: []types.DeviceCount{{Resource: "aws.amazon.com/neuron", Capacity: 16, Unit: types.DeviceUnitAWSDevice}}},
		},
		rates: &config.CostRates{Currency: "USD", PerDeviceHour: map[string]float64{"nvidia.com/gpu": 4, "aws.amazon.com/neuron": 1.5}},
	}
	r := aggregateRecoveryOf(incidents, inv, allRemediated(incidents), from, to, 4*time.Hour)

	if len(r.Groups) != 3 || r.Groups[0].Group != types.AcceleratorGroupGPU || r.Groups[1].Group != types.AcceleratorGroupTPU ||
		r.Groups[2].Group != types.AcceleratorGroupAWSNeuron || r.Groups[2].Unit != "AWS Neuron device-hours" || r.Groups[1].Unit != "TPU chip-hours" {
		t.Fatalf("groups %+v", r.Groups)
	}
	if got := resourceRow(t, r.AcceleratorHours, "gpu", "nvidia.com/gpu"); got.Incidents != 2 || got.DegradedHours != 8.5 || got.RecoveredHours != 8 ||
		got.SavedEstimate == nil || got.SavedEstimate.Hours != 24.5 || got.SavedEstimate.ManualBaselineSeconds != 4*3600 ||
		got.SavedEstimate.Cost == nil || got.SavedEstimate.Cost.Amount != 98 || got.SavedEstimate.Cost.RatePerHour != 4 {
		t.Fatalf("nvidia %+v saved %+v", got, got.SavedEstimate)
	}
	if got := resourceRow(t, r.AcceleratorHours, "gpu", "amd.com/gpu"); got.DegradedHours != 8 || got.SavedEstimate.Hours != 24 || got.SavedEstimate.Cost != nil {
		t.Fatalf("amd %+v", got)
	}
	if got := resourceRow(t, r.AcceleratorHours, "tpu", "google.com/tpu"); got.DegradedHours != 8 || got.RecoveredHours != 8 || got.SavedEstimate.Hours != 8 || got.SavedEstimate.Cost != nil {
		t.Fatalf("tpu %+v", got)
	}
	if got := resourceRow(t, r.AcceleratorHours, "aws-neuron", "aws.amazon.com/neuron"); got.DegradedHours != 16 || got.SavedEstimate.Hours != 48 ||
		got.SavedEstimate.Cost == nil || got.SavedEstimate.Cost.Amount != 72 {
		t.Fatalf("neuron %+v", got)
	}
	if r.SavedCostTotal == nil || r.SavedCostTotal.Amount != nil || !reflect.DeepEqual(r.SavedCostTotal.RateNotSetFor, []string{"amd.com/gpu", "google.com/tpu"}) {
		t.Fatalf("cost total %+v", r.SavedCostTotal)
	}
	if u := r.DeviceCountUnknown; u == nil || u.Incidents != 1 || u.DegradedNodeHours != 2 || u.RecoveredNodeHours != 2 {
		t.Fatalf("device count unknown %+v", u)
	}
	// GPU-hours: NVIDIA 8×1 + 1×0.5, AMD 8×1, and the old incident charged
	// one GPU as before; never the TPU chips or the Neuron devices.
	if r.DegradedGPUHours != 18.5 || r.AssumedSingleGPU != 1 || r.Incidents != 6 {
		t.Fatalf("gpu-hours %v assumed %d incidents %d", r.DegradedGPUHours, r.AssumedSingleGPU, r.Incidents)
	}

	// Per class: its own hours per resource, no saved estimate and no cost.
	for _, class := range r.Classes {
		switch class.Class {
		case types.ClassAcceleratorFault:
			tpuRow, trnRow := resourceRow(t, class.AcceleratorHours, "tpu", "google.com/tpu"), resourceRow(t, class.AcceleratorHours, "aws-neuron", "aws.amazon.com/neuron")
			if tpuRow.DegradedHours != 8 || tpuRow.RecoveredHours != 8 || trnRow.DegradedHours != 16 || tpuRow.SavedEstimate != nil ||
				class.SavedCostTotal != nil || class.DeviceCountUnknown != nil || class.DegradedGPUHours != 0 {
				t.Fatalf("accelerator-fault class %+v", class)
			}
		case types.ClassFellOffBus:
			if u := class.DeviceCountUnknown; u == nil || u.Incidents != 1 || resourceRow(t, class.AcceleratorHours, "gpu", "amd.com/gpu").DegradedHours != 8 {
				t.Fatalf("fell-off-bus class %+v", class)
			}
		}
	}
	for _, d := range r.IncidentDetails {
		if d.ID == "tpu" && (len(d.Accelerators) != 1 || d.Accelerators[0].Resource != "google.com/tpu") {
			t.Fatalf("tpu detail %+v", d)
		}
	}

	// Every rate set: a total; no baseline: no saved and no total.
	inv.rates.PerDeviceHour["amd.com/gpu"], inv.rates.PerDeviceHour["google.com/tpu"] = 3, 2
	r = aggregateRecoveryOf(incidents, inv, allRemediated(incidents), from, to, 4*time.Hour)
	if r.SavedCostTotal == nil || r.SavedCostTotal.Amount == nil || *r.SavedCostTotal.Amount != 98+72+16+72 || r.SavedCostTotal.Currency != "USD" {
		t.Fatalf("cost total %+v", r.SavedCostTotal)
	}
	r = aggregateRecoveryOf(incidents, inv, allRemediated(incidents), from, to, 0)
	if r.SavedCostTotal != nil || resourceRow(t, r.AcceleratorHours, "tpu", "google.com/tpu").SavedEstimate != nil {
		t.Fatalf("saved without a baseline: %+v", r.SavedCostTotal)
	}
}

// A TPU node with no record is judged by its inventory: no GPU-hours.
func TestRecoveryReportOldTPUIncidentIsNoGPU(t *testing.T) {
	to := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	inc := incident("tpu-old", types.ClassAcceleratorFault, "tpu-1", "", types.StateResolved, at(to, -4), at(to, -2))
	inv := reportInventory{nodes: map[string]*types.Node{
		"tpu-1": {Name: "tpu-1", Devices: []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Unit: types.DeviceUnitTPUChip}}},
	}}
	r := aggregateRecoveryOf([]*types.Incident{inc}, inv, allRemediated([]*types.Incident{inc}), to.Add(-24*time.Hour), to, 0)
	if r.DegradedGPUHours != 0 || r.AssumedSingleGPU != 0 || r.Incidents != 1 || r.DeviceCountUnknown == nil || r.DeviceCountUnknown.DegradedNodeHours != 2 {
		t.Fatalf("report %+v unknown %+v", r, r.DeviceCountUnknown)
	}
}

// The dry-run block counts per resource as well.
func TestRecoveryReportSimulatedPerResource(t *testing.T) {
	to := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	inc := withDevices(incident("trn", types.ClassAcceleratorFault, "trn-1", "", types.StateResolved, at(to, -3), at(to, -2)), "aws.amazon.com/neuron", types.DeviceUnitAWSDevice, 16)
	inc.DryRun = true
	r := aggregateRecoveryOf([]*types.Incident{inc}, reportInventory{}, allRemediated([]*types.Incident{inc}), to.Add(-24*time.Hour), to, 0)
	if len(r.Groups) != 0 || r.Simulated == nil || len(r.Simulated.Groups) != 1 || r.Simulated.Groups[0].Resources[0].DegradedHours != 16 {
		t.Fatalf("report %+v simulated %+v", r.AcceleratorHours, r.Simulated)
	}
}

// What an incident records when it opens: the agent's GPUs by vendor, the
// Kubernetes Node's devices otherwise, one device for a device-scoped
// incident, and nothing when the count or the resource is not known.
func TestIncidentAcceleratorsAtOpen(t *testing.T) {
	agent := &types.Node{Name: "gpu-1", GPUs: []types.GPUInfo{{UUID: "GPU-a", Model: "NVIDIA H100"}, {UUID: "GPU-b", Model: "NVIDIA H100"}}}
	tpu := &types.Node{Name: "tpu-1", Devices: []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Unit: types.DeviceUnitTPUChip}}}
	tpuNoChips := &types.Node{Name: "tpu-2", Devices: []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 0, Unit: types.DeviceUnitTPUChip}}}
	mystery := &types.Node{Name: "x", GPUs: []types.GPUInfo{{UUID: "abc"}}}
	for _, tc := range []struct {
		name string
		node *types.Node
		inc  types.Incident
		want []types.IncidentAccelerator
	}{
		{"agent node", agent, types.Incident{Target: types.Target{Node: "gpu-1"}}, []types.IncidentAccelerator{{Resource: "nvidia.com/gpu", Unit: types.DeviceUnitGPU, Count: 2}}},
		{"agent device", agent, types.Incident{Target: types.Target{Node: "gpu-1", GPUUUID: "GPU-a"}}, []types.IncidentAccelerator{{Resource: "nvidia.com/gpu", Unit: types.DeviceUnitGPU, Count: 1}}},
		{"amd by vendor", mystery, types.Incident{Target: types.Target{Node: "x"}, Vendor: types.AcceleratorVendorAMD}, []types.IncidentAccelerator{{Resource: "amd.com/gpu", Unit: types.DeviceUnitGPU, Count: 1}}},
		{"unknown vendor", mystery, types.Incident{Target: types.Target{Node: "x"}}, nil},
		{"tpu chips", tpu, types.Incident{Target: types.Target{Node: "tpu-1"}}, []types.IncidentAccelerator{{Resource: "google.com/tpu", Unit: types.DeviceUnitTPUChip, Count: 4}}},
		{"tpu node only", tpuNoChips, types.Incident{Target: types.Target{Node: "tpu-2"}}, nil},
		{"no node", nil, types.Incident{Target: types.Target{Node: "gone"}}, nil},
	} {
		if got := incidentAccelerators(tc.node, &tc.inc); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// An incident opened on a TPU node records its chips, and charges no GPU to
// the GPU-seconds metric.
func TestIncidentRecordsItsDevicesAndIsNoGPU(t *testing.T) {
	c, st, _, _ := healthFixture(t)
	ctx := context.Background()
	if err := st.UpsertKubernetesInventory(ctx, &types.Node{Name: "tpu-1", Platform: "kubernetes", InventorySource: types.InventoryKubernetes,
		Devices: []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Allocatable: 4, Unit: types.DeviceUnitTPUChip}}}); err != nil {
		t.Fatal(err)
	}
	if err := c.IngestSignal(ctx, types.Signal{Target: types.Target{Node: "tpu-1"}, Class: types.ClassAcceleratorFault,
		Severity: types.SeverityCritical, Source: types.SourceNodeHealth, ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	inc, err := st.GetOpenIncident(ctx, types.Target{Node: "tpu-1"}, types.ClassAcceleratorFault)
	if err != nil {
		t.Fatal(err)
	}
	want := []types.IncidentAccelerator{{Resource: "google.com/tpu", Unit: types.DeviceUnitTPUChip, Count: 4}}
	if !reflect.DeepEqual(inc.Accelerators, want) {
		t.Fatalf("accelerators %+v", inc.Accelerators)
	}
	if n := c.affectedGPUs(ctx, inc); n != 0 {
		t.Fatalf("a TPU incident charged %d GPUs", n)
	}
}

// An incident that recorded 8 GPUs on a node that has 4 when the report is
// read counts 8 everywhere: in its row, the GPU-hour fields and its class.
func TestRecoveryReportGPUHoursFromTheRecord(t *testing.T) {
	to := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	nv := withDevices(incident("nv", types.ClassFellOffBus, "gpu-1", "", types.StateResolved, at(to, -10), at(to, -9)), "nvidia.com/gpu", types.DeviceUnitGPU, 8)
	incidents := []*types.Incident{nv}
	inv := reportInventory{gpusPerNode: map[string]int{"gpu-1": 4}, nodes: map[string]*types.Node{}}
	r := aggregateRecoveryOf(incidents, inv, allRemediated(incidents), to.Add(-24*time.Hour), to, 4*time.Hour)
	row := resourceRow(t, r.AcceleratorHours, "gpu", "nvidia.com/gpu")
	if row.RecoveredHours != 8 || row.SavedEstimate == nil || row.SavedEstimate.Hours != 24 {
		t.Fatalf("row %+v saved %+v", row, row.SavedEstimate)
	}
	if r.RecoveredGPUHours != 8 || r.DegradedGPUHours != 8 || r.SavingsEstimate == nil || r.SavingsEstimate.GPUHoursSaved != 24 || r.AssumedSingleGPU != 0 {
		t.Fatalf("gpu-hours recovered %v degraded %v saved %+v", r.RecoveredGPUHours, r.DegradedGPUHours, r.SavingsEstimate)
	}
	if len(r.Classes) != 1 || r.Classes[0].DegradedGPUHours != 8 || r.Classes[0].RecoveredGPUHours != 8 {
		t.Fatalf("classes %+v", r.Classes)
	}
	if len(r.IncidentDetails) != 1 || r.IncidentDetails[0].DegradedGPUHours != 8 {
		t.Fatalf("details %+v", r.IncidentDetails)
	}

	// With no record, the inventory when the report is read.
	old := incident("old", types.ClassFellOffBus, "gpu-1", "", types.StateResolved, at(to, -10), at(to, -9))
	incidents = []*types.Incident{old}
	r = aggregateRecoveryOf(incidents, inv, allRemediated(incidents), to.Add(-24*time.Hour), to, 4*time.Hour)
	if r.RecoveredGPUHours != 4 || r.SavingsEstimate == nil || r.SavingsEstimate.GPUHoursSaved != 12 || r.DeviceCountUnknown == nil {
		t.Fatalf("no record: recovered %v saved %+v unknown %+v", r.RecoveredGPUHours, r.SavingsEstimate, r.DeviceCountUnknown)
	}
}
