// SPDX-License-Identifier: Apache-2.0

package amdhealth

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// amd-smi prints a bare device array up to ROCm 6.4 and groups the records
// under "gpu_data" from ROCm 7.0. Both must give the same samples.
func TestAMDSMIGroupedOutputParsesLikeTheBareArray(t *testing.T) {
	w := testWatcher(t, nil)
	bare, err := w.parseAMDSMI(readFixture(t, "synthetic-amd-smi-metric-faults.json"))
	if err != nil {
		t.Fatalf("bare array: %v", err)
	}
	grouped, err := w.parseAMDSMI(readFixture(t, "synthetic-amd-smi-metric-faults-gpu-data.json"))
	if err != nil {
		t.Fatalf("gpu_data object: %v", err)
	}
	if len(bare) != 2 || !reflect.DeepEqual(bare, grouped) {
		t.Fatalf("gpu_data samples = %+v, want the bare array's %+v", grouped, bare)
	}
}

// The grouped form carries the same counter series through polling: a
// baseline, then an increase that reports.
func TestAMDSMIGroupedOutputReportsCounterIncreases(t *testing.T) {
	count := "0"
	w := testWatcher(t, func([]string) ([]byte, error) {
		return []byte(`{"cpu_data":[{"cpu":0}],"gpu_data":[{"gpu":0,"ecc":{"total_uncorrectable_count":` + count + `}}]}`), nil
	})
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("first poll = %+v, want only a baseline", got)
	}
	count = "2"
	if got := eventsByCode(w.poll(context.Background())); len(got[CodeECCUncorr]) != 1 {
		t.Fatalf("events = %+v, want one uncorrectable ECC increase", got)
	}
}

// An empty gpu_data is the grouped spelling of "no devices": the same
// statement about the tool as an empty array, not a decode error.
func TestAMDSMIEmptyGPUDataIsAnEmptyInventory(t *testing.T) {
	w := testWatcher(t, nil)
	for _, out := range []string{`[]`, `{"gpu_data":[]}`, `{"cpu_data":[{"cpu":0}],"gpu_data":[]}`} {
		samples, err := w.parseAMDSMI([]byte(out))
		if err != nil || len(samples) != 0 {
			t.Fatalf("%s: samples %+v, err %v; want none and no error", out, samples, err)
		}
	}

	var logged strings.Builder
	output := `{"gpu_data":[{"gpu":0,"ecc":{"total_uncorrectable_count":0}}]}`
	w = testWatcher(t, func([]string) ([]byte, error) { return []byte(output), nil })
	w.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	w.poll(context.Background())
	output = `{"gpu_data":[]}`
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("poll = %+v, want no per-GPU loss from an empty gpu_data", got)
	}
	if !strings.Contains(logged.String(), "refusing to attribute per-GPU loss") {
		t.Fatalf("an empty gpu_data must take the empty-inventory path; log was: %s", logged.String())
	}
}

// Any other shape is a read error with a reason, never zero devices, and the
// poll says so and falls back to rocm-smi.
func TestAMDSMIUnknownShapesAreReadErrors(t *testing.T) {
	for _, tc := range []struct{ name, out, reason string }{
		{"malformed", `{"gpu_data":[{"gpu":0,`, "decode amd-smi JSON"},
		{"empty object", `{}`, "an object without a gpu_data array (keys: )"},
		{"cpu only", `{"cpu_data":[{"cpu":0}]}`, "an object without a gpu_data array (keys: cpu_data)"},
		{"null gpu_data", `{"gpu_data":null}`, "an object without a gpu_data array (keys: gpu_data)"},
		{"gpu_data object", `{"gpu_data":{"gpu":0}}`, "decode amd-smi JSON gpu_data"},
		{"array of non-records", `[1,2]`, "decode amd-smi JSON device array"},
		{"string", `"amd-smi is not supported on this GPU"`, "top level is a string"},
		{"null", `null`, "top level is null"},
		{"number", `42`, "top level is a number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := testWatcher(t, nil)
			samples, err := w.parseAMDSMI([]byte(tc.out))
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("samples %+v, err %v; want an error containing %q", samples, err, tc.reason)
			}

			var logged strings.Builder
			var called []string
			w = &Watcher{NodeName: "amd-node-1", AMDSMIPath: "amd-smi", ROCmSMIPath: "rocm-smi",
				Logger: slog.New(slog.NewTextHandler(&logged, nil))}
			w.run = func(_ context.Context, name string, _ ...string) ([]byte, error) {
				called = append(called, name)
				if name == "amd-smi" {
					return []byte(tc.out), nil
				}
				return nil, errors.New("rocm-smi is absent")
			}
			if got := w.poll(context.Background()); len(got) != 0 {
				t.Fatalf("poll = %+v, want no events", got)
			}
			if !reflect.DeepEqual(called, []string{"amd-smi", "rocm-smi"}) {
				t.Fatalf("calls = %v, want amd-smi then the rocm-smi fallback", called)
			}
			if log := logged.String(); !strings.Contains(log, "amd-smi health probe failed") || !strings.Contains(log, tc.reason) {
				t.Fatalf("the unreadable shape must be named in the agent log; log was: %s", log)
			}
		})
	}
}
