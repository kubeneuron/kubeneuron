// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// A closed incident charges each recorded resource in its own device, the
// GPU series none of a TPU's chips, and an incident with no record nothing.
func TestDegradedAcceleratorSecondsPerResource(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(st, st, nil, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2}), nil, nil, nil, &notify.Log{Logger: log}, log)

	class := string(types.ClassAcceleratorFault)
	series := func(resource string) float64 {
		return testutil.ToFloat64(metrics.DegradedAcceleratorSeconds.WithLabelValues(class, "expired", resource))
	}
	tpuBefore, gpuBefore := series("google.com/tpu"), testutil.ToFloat64(metrics.DegradedGPUSeconds.WithLabelValues(class, "expired"))
	inc := &types.Incident{ID: "tpu", Target: types.Target{Node: "tpu-1"}, Class: types.ClassAcceleratorFault, OpenedAt: time.Now().Add(-time.Hour),
		Accelerators: []types.IncidentAccelerator{{Resource: "google.com/tpu", Unit: types.DeviceUnitTPUChip, Count: 4}}}
	c.recordRecoveryOutcome(context.Background(), inc, types.StateExpired)
	if got := series("google.com/tpu") - tpuBefore; math.Abs(got-4*3600) > 60 {
		t.Fatalf("TPU chip-seconds %v, want about %v", got, 4*3600)
	}
	if got := testutil.ToFloat64(metrics.DegradedGPUSeconds.WithLabelValues(class, "expired")) - gpuBefore; got != 0 {
		t.Fatalf("a TPU incident charged %v GPU-seconds", got)
	}

	before := testutil.CollectAndCount(metrics.DegradedAcceleratorSeconds)
	unknownBefore := testutil.ToFloat64(metrics.DegradedNodeSecondsDeviceCountUnknown.WithLabelValues(class, "expired"))
	old := &types.Incident{ID: "old", Target: types.Target{Node: "unknown-1"}, Class: types.ClassAcceleratorFault, OpenedAt: time.Now().Add(-time.Hour)}
	c.recordRecoveryOutcome(context.Background(), old, types.StateExpired)
	if got := testutil.CollectAndCount(metrics.DegradedAcceleratorSeconds); got != before {
		t.Fatalf("an incident with no recorded devices added a series: %d -> %d", before, got)
	}
	if got := testutil.ToFloat64(metrics.DegradedNodeSecondsDeviceCountUnknown.WithLabelValues(class, "expired")) - unknownBefore; math.Abs(got-3600) > 60 {
		t.Fatalf("node-seconds with an unknown count %v, want about 3600", got)
	}
}

// For GPUs the per-resource series add up to the GPU series: one count.
func TestDegradedAcceleratorSecondsMatchTheGPUSeries(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(st, st, nil, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2}), nil, nil, nil, &notify.Log{Logger: log}, log)

	class := string(types.ClassFellOffBus)
	gpu := func() float64 {
		return testutil.ToFloat64(metrics.DegradedAcceleratorSeconds.WithLabelValues(class, "expired", "nvidia.com/gpu")) +
			testutil.ToFloat64(metrics.DegradedAcceleratorSeconds.WithLabelValues(class, "expired", "amd.com/gpu"))
	}
	old := func() float64 {
		return testutil.ToFloat64(metrics.DegradedGPUSeconds.WithLabelValues(class, "expired"))
	}
	gpuBefore, oldBefore := gpu(), old()
	opened := time.Now().Add(-time.Hour)
	for _, inc := range []*types.Incident{
		{ID: "nv", Target: types.Target{Node: "gpu-1"}, Class: types.ClassFellOffBus, OpenedAt: opened,
			Accelerators: []types.IncidentAccelerator{{Resource: "nvidia.com/gpu", Unit: types.DeviceUnitGPU, Count: 8}}},
		{ID: "amd", Target: types.Target{Node: "mi-1"}, Class: types.ClassFellOffBus, OpenedAt: opened,
			Accelerators: []types.IncidentAccelerator{{Resource: "amd.com/gpu", Unit: types.DeviceUnitGPU, Count: 4}}},
		{ID: "nv-dev", Target: types.Target{Node: "gpu-1", GPUUUID: "GPU-1"}, Class: types.ClassFellOffBus, OpenedAt: opened,
			Accelerators: []types.IncidentAccelerator{{Resource: "nvidia.com/gpu", Unit: types.DeviceUnitGPU, Count: 1}}},
	} {
		c.recordRecoveryOutcome(context.Background(), inc, types.StateExpired)
	}
	got, want := gpu()-gpuBefore, old()-oldBefore
	if want < 13*3600-60 || math.Abs(got-want) > 1 {
		t.Fatalf("per-resource GPU-seconds %v, GPU series %v", got, want)
	}
}
