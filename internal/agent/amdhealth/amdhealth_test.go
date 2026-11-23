// SPDX-License-Identifier: Apache-2.0

package amdhealth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// readFixture loads a captured-output fixture.
//
// EVERY fixture under testdata/ is SYNTHETIC: it was written by hand, the
// amd-smi ones from the amd-smi CLI source and the rocm-smi one from its
// documentation, never captured from an AMD accelerator, which is why each
// filename says so. They pin this package's PARSING CONTRACT and
// its refusal behavior; they are not evidence that real tooling emits these
// shapes. Re-verify them against a real node before treating a quiet poll on
// AMD hardware as evidence of health.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func testWatcher(t *testing.T, output func(args []string) ([]byte, error)) *Watcher {
	t.Helper()
	w := &Watcher{NodeName: "amd-node-1", AMDSMIPath: "amd-smi", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w.run = func(_ context.Context, _ string, args ...string) ([]byte, error) { return output(args) }
	return w
}

// amdSMIOutput answers `amd-smi list` with the synthetic list fixture and any
// other invocation (`metric`) with metric().
func amdSMIOutput(t *testing.T, metric func() []byte) func([]string) ([]byte, error) {
	t.Helper()
	return func(args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list" {
			return readFixture(t, "synthetic-amd-smi-list.json"), nil
		}
		return metric(), nil
	}
}

// eventsByCode indexes emitted events by their neutral fault code.
func eventsByCode(events []types.AgentEvent) map[string][]types.AgentEvent {
	out := map[string][]types.AgentEvent{}
	for _, ev := range events {
		if ev.Fault != nil {
			out[ev.Fault.Code] = append(out[ev.Fault.Code], ev)
		}
	}
	return out
}

func TestParseAMDSMIReadsECCAndHotspot(t *testing.T) {
	w := testWatcher(t, nil)
	samples, err := w.parseAMDSMI(readFixture(t, "synthetic-amd-smi-metric-faults.json"))
	if err != nil {
		t.Fatalf("parseAMDSMI: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
	gpu0, gpu1 := samples[0], samples[1]
	if gpu0.index != 0 || gpu1.index != 1 {
		t.Fatalf("indices = %d, %d, want 0, 1", gpu0.index, gpu1.index)
	}
	// `metric` names no device: the identity comes from `list`.
	if gpu0.uuid != "" || gpu0.bdf != "" {
		t.Fatalf("gpu0 identity = %+v, want none from metric alone", gpu0)
	}
	if gpu0.eccUncorrectable == nil || *gpu0.eccUncorrectable != 2 {
		t.Fatalf("gpu0 uncorrectable ECC = %v, want 2", gpu0.eccUncorrectable)
	}
	if gpu1.eccCorrectable == nil || *gpu1.eccCorrectable != 4 {
		t.Fatalf("gpu1 correctable ECC = %v, want 4", gpu1.eccCorrectable)
	}
	// The {"value": N, "unit": "C"} envelope must be understood, not skipped.
	if gpu1.hotspotC == nil || *gpu1.hotspotC != 108 {
		t.Fatalf("gpu1 hotspot = %v, want 108", gpu1.hotspotC)
	}
	// xgmi_err, power.throttle_status and the throttle block are in the
	// fixture and are not read.
	if gpu1.retiredPages != nil {
		t.Fatalf("gpu1 retired pages = %v, want none from amd-smi", gpu1.retiredPages)
	}
}

// TestAMDSMICountersBaselineThenEmitOnIncrease is the anti-startup-storm rule:
// the first poll absorbs whatever history the counters carry (they are lifetime
// aggregates of unknown age), and only a later increase is a fault.
func TestAMDSMICountersBaselineThenEmitOnIncrease(t *testing.T) {
	fixture := "synthetic-amd-smi-metric-healthy.json"
	w := testWatcher(t, func(args []string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "metric --json":
			return readFixture(t, fixture), nil
		case "list --json":
			return readFixture(t, "synthetic-amd-smi-list.json"), nil
		}
		t.Fatalf("unexpected amd-smi invocation: %v", args)
		return nil, nil
	})

	if first := w.poll(context.Background()); len(first) != 0 {
		t.Fatalf("baseline poll = %+v, want no events", first)
	}

	fixture = "synthetic-amd-smi-metric-faults.json"
	got := eventsByCode(w.poll(context.Background()))

	ecc := got[CodeECCUncorr]
	if len(ecc) != 1 || ecc[0].GPUIndex != 0 || ecc[0].GPUUUID != "amd-c3000000-0000-1000-8000-000000000001" {
		t.Fatalf("ecc-uncorrectable events = %+v, want one attributed to GPU 0", ecc)
	}
	if ecc[0].Fault.Vendor != FaultVendor || ecc[0].Fault.Source != SourceAMDSMI {
		t.Fatalf("fault provenance = %+v, want amd/amd-smi", ecc[0].Fault)
	}
	if ecc[0].PCIAddr != "0000:c3:00.0" {
		t.Fatalf("PCI address = %q, want the lowercased address from amd-smi list", ecc[0].PCIAddr)
	}
	if ecc[0].Fault.Attributes["uncorrectable_ecc_errors"] != "2" {
		t.Fatalf("fault attributes = %+v, want the observed counter carried as evidence", ecc[0].Fault.Attributes)
	}
	if _, marked := ecc[0].Fault.Attributes["pci_address"]; marked {
		t.Fatalf("fault attributes = %+v, want no not-read marker when the address was read", ecc[0].Fault.Attributes)
	}
	// GPU 1's correctable count did not move (4 -> 4) and no device vanished.
	// Its XGMI errors and THROTTLED status are not read from amd-smi, and its
	// 108C hotspot has no configured threshold.
	if len(got) != 1 {
		t.Fatalf("unexpected events: %+v", got)
	}

	// A repeat of the SAME reading is not a new fault: the counters have not
	// grown.
	if repeat := w.poll(context.Background()); len(repeat) != 0 {
		t.Fatalf("unchanged repeat poll = %+v, want none", repeat)
	}
}

// TestEmittedEventsCarryNoXID pins the encoding boundary. Nothing this source
// observes is an NVIDIA XID, and the controller rejects an event that claims
// both identities, so a stray XID here would make every AMD detection a
// permanently rejected payload.
func TestEmittedEventsCarryNoXID(t *testing.T) {
	fixture := "synthetic-amd-smi-metric-healthy.json"
	w := testWatcher(t, amdSMIOutput(t, func() []byte { return readFixture(t, fixture) }))
	w.poll(context.Background())
	fixture = "synthetic-amd-smi-metric-faults.json"
	events := w.poll(context.Background())
	if len(events) == 0 {
		t.Fatal("expected fault events to assert on")
	}
	for _, ev := range events {
		if ev.XID != 0 || ev.Fault == nil {
			t.Fatalf("event = %+v, want a neutral fault and a zero XID", ev)
		}
		if ev.Node != "amd-node-1" {
			t.Fatalf("event node = %q, want the watcher's node name", ev.Node)
		}
	}
}

// TestEveryEmittedCodeIsClassified is the contract between this source and the
// catalog: a code with no faultTable row would be counted by the agent and then
// silently open no incident — detection that looks like coverage and is not.
func TestEveryEmittedCodeIsClassified(t *testing.T) {
	for _, code := range []string{
		CodeECCUncorr, CodeECCCorrRate, CodePageRetire, CodePageRetireExhausted,
		CodeXGMILink, CodeThermal, CodeGPULost,
	} {
		info, ok := detect.ClassifyFault(FaultVendor, code)
		if !ok {
			t.Errorf("amd/%s has no neutral fault row; add one to internal/detect/fault.go", code)
			continue
		}
		if info.Class == "" {
			t.Errorf("amd/%s classifies to an empty problem class", code)
		}
	}
}

// TestAmbiguousReadingsNeverInventAFault is the table-driven core of the safety
// posture: every shape this parser cannot understand must produce silence. On
// hardware nobody has tested, a false fault that drains a healthy node is far
// worse than a missed one.
func TestAmbiguousReadingsNeverInventAFault(t *testing.T) {
	cases := []struct {
		name   string
		output []byte
		reason string
	}{
		{
			name:   "unreadable and unindexed records",
			output: readFixture(t, "synthetic-amd-smi-metric-unreadable.json"),
			reason: "N/A counters and temperatures, an N/A XGMI and throttle status and a device with no index",
		},
		{
			name:   "empty device list",
			output: []byte(`[]`),
			reason: "a tool that lists no devices says nothing about any device",
		},
		{
			name:   "metrics present but every value absent",
			output: []byte(`[{"gpu":0,"ecc":{"total_correctable_count":"N/A","total_uncorrectable_count":"N/A"}}]`),
			reason: "an absent counter must not be read as zero",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := testWatcher(t, func([]string) ([]byte, error) { return tc.output, nil })
			// Two polls: the first could only baseline, so a fault could only
			// appear on the second — which is where a zero-for-absent bug shows.
			w.poll(context.Background())
			if got := w.poll(context.Background()); len(got) != 0 {
				t.Fatalf("poll = %+v, want no events (%s)", got, tc.reason)
			}
		})
	}
}

// TestUnreadableSectionsAreReportedNotSilentlySkipped is the visibility half of
// the refusal rule. A tool version whose ECC block this parser cannot read
// leaves the GPU looking permanently healthy, and no downstream metric can tell
// that apart from an actually healthy GPU — so the blindness must be named.
func TestUnreadableSectionsAreReportedNotSilentlySkipped(t *testing.T) {
	var logged strings.Builder
	w := testWatcher(t, func([]string) ([]byte, error) {
		return readFixture(t, "synthetic-amd-smi-metric-unreadable.json"), nil
	})
	w.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	w.poll(context.Background())
	out := logged.String()
	for _, want := range []string{"ecc", "temperature"} {
		if !strings.Contains(out, `section=`+want) {
			t.Errorf("unreadable %q section was skipped silently; log was: %s", want, out)
		}
	}
	// The device record with no gpu index is a refusal too and must be named
	// rather than assumed.
	if !strings.Contains(out, "without a gpu index") {
		t.Errorf("an unindexed device must be reported; log was: %s", out)
	}
}

// TestUnparseableOutputDegradesToObservedOnly covers the tool that answers with
// something this code cannot read at all: it must fall back, then degrade, and
// never panic or fabricate.
func TestUnparseableOutputDegradesToObservedOnly(t *testing.T) {
	w := testWatcher(t, func([]string) ([]byte, error) {
		return []byte("amd-smi: command not found\n"), nil
	})
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("poll = %+v, want no events from unparseable output", got)
	}
}

// TestThermalRefusesWithoutAThresholdAndReportsWithOne is the ambiguity rule
// made concrete. A hotspot reading alone cannot say whether a GPU is in
// trouble: the critical temperature is SKU-specific. Without an
// operator-configured threshold the temperature is observed and nothing more,
// and amd-smi's THROTTLED status does not stand in for one: it covers every
// cause, power capping included.
func TestThermalRefusesWithoutAThresholdAndReportsWithOne(t *testing.T) {
	const hot = `[{"gpu":0,"temperature":{"hotspot":{"value":109,"unit":"C"}},` +
		`"power":{"throttle_status":"THROTTLED"},"throttle":{"socket_thermal_violation_status":"ACTIVE"}}]`

	var logged strings.Builder
	w := testWatcher(t, amdSMIOutput(t, func() []byte { return []byte(hot) }))
	w.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	w.poll(context.Background())
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("poll = %+v, want no thermal fault without a configured threshold", got)
	}
	if !strings.Contains(logged.String(), "observing only") {
		t.Fatalf("the refusal must be visible to an operator; log was: %s", logged.String())
	}

	// With an operator-supplied threshold the same reading is actionable.
	withThreshold := testWatcher(t, amdSMIOutput(t, func() []byte { return []byte(hot) }))
	withThreshold.ThermalCriticalC = 100
	got := eventsByCode(withThreshold.poll(context.Background()))
	if len(got[CodeThermal]) != 1 {
		t.Fatalf("thermal events = %+v, want one against the configured threshold", got[CodeThermal])
	}
	if attrs := got[CodeThermal][0].Fault.Attributes; attrs["threshold_c"] != "100" || attrs["hotspot_c"] != "109" {
		t.Fatalf("thermal attributes = %+v, want the reading and the threshold recorded as evidence", attrs)
	}
}

// TestCorrectableRateReportsOnlyOnAMeaningfulDelta stops a routine trickle of
// corrected ECC errors from becoming an event per poll, which would bury the
// uncorrectable faults that actually matter.
func TestCorrectableRateReportsOnlyOnAMeaningfulDelta(t *testing.T) {
	correctable := 0
	w := testWatcher(t, func([]string) ([]byte, error) {
		return []byte(`[{"gpu":0,"ecc":{"total_correctable_count":` + strconv.Itoa(correctable) + `,"total_uncorrectable_count":0}}]`), nil
	})
	w.CorrectableRateMinDelta = 10
	w.poll(context.Background()) // baseline at 0

	correctable = 9
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("poll = %+v, want silence below the reporting delta", got)
	}
	// The sub-threshold errors are not discarded: they accumulate, so one more
	// crossing the delta reports the whole run rather than starting over.
	correctable = 10
	got := eventsByCode(w.poll(context.Background()))
	if len(got[CodeECCCorrRate]) != 1 {
		t.Fatalf("events = %+v, want one correctable-rate report at the delta", got)
	}
	if got[CodeECCCorrRate][0].Fault.Attributes["correctable_ecc_errors"] != "10" {
		t.Fatalf("attributes = %+v, want the cumulative count", got[CodeECCCorrRate][0].Fault.Attributes)
	}
	// Immediately after reporting, the delta restarts: 5 more is silence again.
	correctable = 15
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("poll = %+v, want silence until the next full delta", got)
	}
}

// TestGPULostReportsADisappearanceOnceAndReArms covers the presence series: a
// device that vanishes from the tool's own inventory is ClassGPULost, it is
// reported once rather than on every tick, and its return re-arms the series so
// a second disappearance is reported again.
func TestGPULostReportsADisappearanceOnceAndReArms(t *testing.T) {
	both := `[{"gpu":0,"ecc":{"total_uncorrectable_count":0}},{"gpu":1,"ecc":{"total_uncorrectable_count":0}}]`
	one := `[{"gpu":0,"ecc":{"total_uncorrectable_count":0}}]`

	output := both
	w := testWatcher(t, amdSMIOutput(t, func() []byte { return []byte(output) }))
	w.poll(context.Background())

	output = one
	got := eventsByCode(w.poll(context.Background()))
	lost := got[CodeGPULost]
	if len(lost) != 1 || lost[0].GPUIndex != 1 {
		t.Fatalf("gpu-lost events = %+v, want exactly one for GPU 1", lost)
	}
	// The vanished device is still named: the identity remembered from the last
	// poll that saw it is what makes the incident actionable.
	if lost[0].GPUUUID != "amd-c6000000-0000-1000-8000-000000000002" || lost[0].PCIAddr != "0000:c6:00.0" {
		t.Fatalf("gpu-lost event = %+v, want the last known identity of GPU 1", lost[0])
	}
	if repeat := w.poll(context.Background()); len(repeat) != 0 {
		t.Fatalf("poll = %+v, want silence while the loss persists", repeat)
	}

	output = both
	if back := w.poll(context.Background()); len(back) != 0 {
		t.Fatalf("poll = %+v, want no event when the device returns", back)
	}
	output = one
	if got := eventsByCode(w.poll(context.Background()))[CodeGPULost]; len(got) != 1 {
		t.Fatalf("gpu-lost events = %+v, want the second disappearance reported", got)
	}
}

// TestEmptyInventoryRefusesToBlameEveryGPU is the fail-closed counterpart: zero
// devices where devices used to be is a statement about the tool (a wedged
// driver, a lost /dev/kfd), not a per-GPU diagnosis. Opening a gpu-lost
// incident on every accelerator at once would drain the whole node.
func TestEmptyInventoryRefusesToBlameEveryGPU(t *testing.T) {
	output := `[{"gpu":0,"ecc":{"total_uncorrectable_count":0}},{"gpu":1,"ecc":{"total_uncorrectable_count":0}}]`
	var logged strings.Builder
	w := testWatcher(t, func([]string) ([]byte, error) { return []byte(output), nil })
	w.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	w.poll(context.Background())

	output = `[]`
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("poll = %+v, want no per-GPU loss from an empty inventory", got)
	}
	if !strings.Contains(logged.String(), "refusing to attribute per-GPU loss") {
		t.Fatalf("the refusal must be visible to an operator; log was: %s", logged.String())
	}
}

// TestPresenceIsNeverComparedAcrossTools stops a fallback from inventing a lost
// GPU. amd-smi and rocm-smi need not enumerate the same devices or agree on
// indices, so a device missing from the OTHER tool's list is evidence about the
// tools, not about the hardware — and a false gpu-lost drains a healthy node.
func TestPresenceIsNeverComparedAcrossTools(t *testing.T) {
	amdSMIWorks := true
	var logged strings.Builder
	w := &Watcher{
		NodeName: "amd-node-1", AMDSMIPath: "amd-smi", ROCmSMIPath: "rocm-smi",
		Logger: slog.New(slog.NewTextHandler(&logged, nil)),
	}
	w.run = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "amd-smi" {
			if !amdSMIWorks {
				return nil, errors.New("amd-smi is gone")
			}
			// amd-smi sees two devices.
			return []byte(`[{"gpu":0,"ecc":{"total_uncorrectable_count":0}},{"gpu":1,"ecc":{"total_uncorrectable_count":0}}]`), nil
		}
		// rocm-smi's narrower view reports only card0.
		return []byte(`{"card0":{"Total Retired Pages":"0"}}`), nil
	}
	w.poll(context.Background())

	amdSMIWorks = false
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("poll = %+v, want no gpu-lost when the fallback tool simply lists fewer devices", got)
	}
	if !strings.Contains(logged.String(), "last seen by a different tool") {
		t.Fatalf("the skipped comparison must be visible to an operator; log was: %s", logged.String())
	}
}

func TestParseROCmSMIFallbackReadsPagesAndTemperature(t *testing.T) {
	w := testWatcher(t, nil)
	samples, err := w.parseROCmSMI(readFixture(t, "synthetic-rocm-smi-retiredpages-temp.json"))
	if err != nil {
		t.Fatalf("parseROCmSMI: %v", err)
	}
	// The "system" section is not a card and must not become a device.
	if len(samples) != 2 {
		t.Fatalf("samples = %+v, want exactly the two cards", samples)
	}
	byIndex := map[int]sample{}
	for _, s := range samples {
		byIndex[s.index] = s
	}
	card0 := byIndex[0]
	if card0.retiredPages == nil || *card0.retiredPages != 3 {
		t.Fatalf("card0 retired pages = %v, want 3", card0.retiredPages)
	}
	if card0.hotspotC == nil || *card0.hotspotC != 61 {
		t.Fatalf("card0 hotspot = %v, want the junction sensor", card0.hotspotC)
	}
	if card0.bdf != "0000:c3:00.0" {
		t.Fatalf("card0 bdf = %q", card0.bdf)
	}
	// rocm-smi's "Unique ID" is NOT amd-smi's UUID; adopting it would give one
	// GPU two identities depending on which tool answered.
	if card0.uuid != "" {
		t.Fatalf("card0 uuid = %q, want empty: rocm-smi reports no comparable UUID", card0.uuid)
	}
}

// TestROCmSMIUsedOnlyWhenAMDSMIFails pins the preference order and the fallback
// itself: amd-smi is the richer source, so rocm-smi runs only when amd-smi
// cannot answer this tick, and it is retried next tick rather than latched off.
func TestROCmSMIUsedOnlyWhenAMDSMIFails(t *testing.T) {
	var called []string
	amdSMIFails := true
	w := &Watcher{
		NodeName: "amd-node-1", AMDSMIPath: "amd-smi", ROCmSMIPath: "rocm-smi",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	w.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		called = append(called, name)
		if name == "amd-smi" {
			if amdSMIFails {
				return []byte("Unable to open /dev/kfd\n"), errors.New("exit status 1")
			}
			return readFixture(t, "synthetic-amd-smi-metric-healthy.json"), nil
		}
		if strings.Join(args, " ") != "--showretiredpages --showtemp --json" {
			t.Fatalf("rocm-smi args = %v, want the narrow retired-pages/temp query", args)
		}
		return readFixture(t, "synthetic-rocm-smi-retiredpages-temp.json"), nil
	}

	w.poll(context.Background()) // amd-smi fails, rocm-smi baselines
	if len(called) != 2 || called[0] != "amd-smi" || called[1] != "rocm-smi" {
		t.Fatalf("calls = %v, want amd-smi tried first then rocm-smi", called)
	}
	// A retired-page increase seen through the fallback is still a fault, and
	// it is unattributed by UUID but addressed by index and PCI address.
	called = nil
	got := eventsByCode(pollWithROCmPages(t, w, 5))
	if len(got[CodePageRetire]) != 1 || got[CodePageRetire][0].Fault.Source != SourceROCmSMI {
		t.Fatalf("events = %+v, want one page-retirement attributed to rocm-smi", got)
	}

	// amd-smi recovering must be picked up again: the fallback is per poll.
	amdSMIFails = false
	called = nil
	w.poll(context.Background())
	if len(called) != 2 || called[0] != "amd-smi" || called[1] != "amd-smi" {
		t.Fatalf("calls = %v, want only amd-smi (metric and list) once it recovers", called)
	}
}

// pollWithROCmPages re-polls with the rocm-smi fixture rewritten to a new
// retired-page count for card0, exercising the fallback's counter path.
func pollWithROCmPages(t *testing.T, w *Watcher, pages int) []types.AgentEvent {
	t.Helper()
	original := w.run
	w.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "amd-smi" {
			return nil, errors.New("still down")
		}
		out := strings.Replace(string(readFixture(t, "synthetic-rocm-smi-retiredpages-temp.json")),
			`"Total Retired Pages": "3"`, `"Total Retired Pages": "`+strconv.Itoa(pages)+`"`, 1)
		return []byte(out), nil
	}
	defer func() { w.run = original }()
	return w.poll(context.Background())
}

func TestNoUsableSourceDegradesToObservedOnly(t *testing.T) {
	var logged strings.Builder
	w := &Watcher{NodeName: "amd-node-1", Logger: slog.New(slog.NewTextHandler(&logged, nil))}
	w.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("no tool configured: run must not be called")
		return nil, nil
	}
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("degraded poll = %+v, want no events and no crash", got)
	}
	if !strings.Contains(logged.String(), "observing only") {
		t.Fatalf("degradation must be visible to an operator; log was: %s", logged.String())
	}
}

// TestNewRequiresRealBinaries is the no-fake-driver rule: a tool that is not on
// the node stays disabled, so this source can never report a fault it did not
// read from a real binary.
func TestNewRequiresRealBinaries(t *testing.T) {
	missing := New("amd-node-1", filepath.Join(t.TempDir(), "nope-amd-smi"), filepath.Join(t.TempDir(), "nope-rocm-smi"))
	if missing.AMDSMIPath != "" || missing.ROCmSMIPath != "" {
		t.Fatalf("absent binaries must stay disabled, got amd-smi=%q rocm-smi=%q", missing.AMDSMIPath, missing.ROCmSMIPath)
	}
	if missing.Enabled() {
		t.Fatal("a watcher with no tools must not report itself enabled")
	}
	if _, err := os.Stat("/bin/sh"); err == nil {
		present := New("amd-node-1", "/bin/sh", "")
		if present.AMDSMIPath != "/bin/sh" || !present.Enabled() {
			t.Fatalf("a present binary must enable the source, got %q enabled=%v", present.AMDSMIPath, present.Enabled())
		}
	}
}

func TestWatchStreamsEventsAndStopsWithContext(t *testing.T) {
	fixture := "synthetic-amd-smi-metric-healthy.json"
	w := testWatcher(t, func([]string) ([]byte, error) { return readFixture(t, fixture), nil })
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	for range ch { //nolint:revive // draining until the watcher closes the channel is the assertion
	}
}

// TestAMDSMISectionsNotReadStaySilent covers what amd-smi prints but this
// release does not read. XGMI in `metric` is a status, not a counter, retired
// pages are in other commands, and the throttle status covers every cause.
// Those sections give no event and no zero, and the agent log says once that
// they are not read.
func TestAMDSMISectionsNotReadStaySilent(t *testing.T) {
	xgmi, throttle := "AMDSMI_XGMI_STATUS_NO_ERRORS", "UNTHROTTLED"
	var logged strings.Builder
	w := testWatcher(t, amdSMIOutput(t, func() []byte {
		return []byte(`[{"gpu":0,"ecc":{"total_uncorrectable_count":0},"xgmi_err":"` + xgmi + `",` +
			`"power":{"throttle_status":"` + throttle + `"},"throttle":{"socket_thermal_violation_status":"NOT ACTIVE"}}]`)
	}))
	w.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	w.XGMILinkMinDelta = 1
	w.BadPageThreshold = 1
	w.poll(context.Background())

	xgmi, throttle = "AMDSMI_XGMI_STATUS_MULTIPLE_ERRORS", "THROTTLED"
	w.poll(context.Background())
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("poll = %+v, want no event from sections that are not read", got)
	}
	out := logged.String()
	if n := strings.Count(out, "section not available"); n != 1 {
		t.Fatalf("not-read notice logged %d times, want once; log was: %s", n, out)
	}
	for _, want := range []string{"XGMI link errors", "retired pages", "thermal throttling"} {
		if !strings.Contains(out, want) {
			t.Errorf("the notice must name %q; log was: %s", want, out)
		}
	}
}

// TestAMDSMIListFailureMarksTheAddressNotRead is the identity half of the
// join. Without `amd-smi list` the readings still report, and the event says
// that the PCI address was not read instead of carrying a blank field.
func TestAMDSMIListFailureMarksTheAddressNotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		list func() ([]byte, error)
		log  string
	}{
		{"list fails", func() ([]byte, error) { return []byte("Unknown command list\n"), errors.New("exit status 2") }, "amd-smi list failed"},
		{"list unreadable", func() ([]byte, error) { return []byte(`{"cpu_data":[]}`), nil }, "amd-smi list failed"},
		{"gpu not listed", func() ([]byte, error) { return []byte(`[{"gpu":1,"bdf":"0000:c6:00.0","uuid":"amd-gpu-1"}]`), nil }, "amd-smi list does not name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uncorr := 0
			var logged strings.Builder
			w := testWatcher(t, func(args []string) ([]byte, error) {
				if args[0] == "list" {
					return tc.list()
				}
				return []byte(`[{"gpu":0,"ecc":{"total_uncorrectable_count":` + strconv.Itoa(uncorr) + `}}]`), nil
			})
			w.Logger = slog.New(slog.NewTextHandler(&logged, nil))
			w.poll(context.Background())
			uncorr = 1
			ecc := eventsByCode(w.poll(context.Background()))[CodeECCUncorr]
			if len(ecc) != 1 {
				t.Fatalf("ecc events = %+v, want the reading reported without the address", ecc)
			}
			ev := ecc[0]
			if ev.PCIAddr != "" || ev.GPUUUID != "" {
				t.Fatalf("event = %+v, want no invented identity", ev)
			}
			if ev.Fault.Attributes["pci_address"] != "not read" || !strings.Contains(ev.Raw, "PCI address not read") {
				t.Fatalf("event = %+v, want the missing address named in the attributes and the text", ev)
			}
			if n := strings.Count(logged.String(), tc.log); n != 1 {
				t.Fatalf("%q logged %d times, want once; log was: %s", tc.log, n, logged.String())
			}
		})
	}
}

// amd-smi prints `list` in the same two shapes as `metric`.
func TestAMDSMIListJoinsInBothShapes(t *testing.T) {
	for _, list := range []string{"synthetic-amd-smi-list.json", "synthetic-amd-smi-list-gpu-data.json"} {
		t.Run(list, func(t *testing.T) {
			w := testWatcher(t, func(args []string) ([]byte, error) {
				if args[0] == "list" {
					return readFixture(t, list), nil
				}
				return readFixture(t, "synthetic-amd-smi-metric-faults-gpu-data.json"), nil
			})
			samples, err := w.pollAMDSMI(context.Background())
			if err != nil {
				t.Fatalf("pollAMDSMI: %v", err)
			}
			if len(samples) != 2 || samples[1].bdf != "0000:c6:00.0" ||
				samples[1].uuid != "amd-c6000000-0000-1000-8000-000000000002" {
				t.Fatalf("samples = %+v, want GPU 1 named by the list", samples)
			}
		})
	}
}

// TestBadPageThresholdSeparatesRetirementFromExhaustion pins the distinction
// the fault table encodes: retiring a page is the recovery mechanism WORKING
// (ClassRowRemapOK), while retiring past the device's budget means there are
// no spares left to retire into (ClassRowRemapFailure, the peer of XID 64).
// Retired pages come from rocm-smi in this release.
func TestBadPageThresholdSeparatesRetirementFromExhaustion(t *testing.T) {
	pages := 0
	newWatcher := func(threshold uint64) *Watcher {
		w := rocmWatcher(t, func() int { return pages })
		w.BadPageThreshold = threshold
		return w
	}

	// With no threshold configured KubeNeuron never claims a device is out of
	// spares — the budget is SKU-specific and inventing one would be a guess
	// that ends in a node replacement.
	w := newWatcher(0)
	w.poll(context.Background())
	pages = 5000
	for code := range eventsByCode(w.poll(context.Background())) {
		if code == CodePageRetireExhausted {
			t.Fatal("claimed the spare-page budget was exhausted with no threshold configured")
		}
	}

	pages = 0
	w = newWatcher(64)
	w.poll(context.Background()) // baseline
	pages = 10
	got := eventsByCode(w.poll(context.Background()))
	if len(got[CodePageRetire]) != 1 {
		t.Fatalf("events = %+v, want the routine retirement below the threshold", got)
	}
	if len(got[CodePageRetireExhausted]) != 0 {
		t.Fatalf("events = %+v, want no exhaustion claim below the threshold", got)
	}

	pages = 64
	got = eventsByCode(w.poll(context.Background()))
	if len(got[CodePageRetireExhausted]) != 1 {
		t.Fatalf("events = %+v, want the exhaustion level to fire at the threshold", got)
	}
	attrs := got[CodePageRetireExhausted][0].Fault.Attributes
	if attrs["retired_pages"] != "64" || attrs["threshold"] != "64" {
		t.Fatalf("attributes = %+v, want the count and the threshold that judged it", attrs)
	}
	info, ok := detect.ClassifyFault(FaultVendor, CodePageRetireExhausted)
	if !ok || info.Class != types.ClassRowRemapFailure {
		t.Fatalf("class = %v (found=%v), want ClassRowRemapFailure — an exhausted budget is XID 64's peer, not a successful retirement",
			info.Class, ok)
	}

	// It is a LEVEL: staying past the budget must not re-open an incident on
	// every poll.
	pages = 70
	if got := eventsByCode(w.poll(context.Background())); len(got[CodePageRetireExhausted]) != 0 {
		t.Fatalf("events = %+v, want silence while the exhausted condition persists", got)
	}
}

// TestExhaustedBudgetSurvivesTheStartupBaseline is the case the counter rules
// would get wrong. A GPU that is ALREADY past its bad-page budget when the
// agent starts is not history to be absorbed — it is a device that needs
// replacing right now.
func TestExhaustedBudgetSurvivesTheStartupBaseline(t *testing.T) {
	w := rocmWatcher(t, func() int { return 900 })
	w.BadPageThreshold = 64

	got := eventsByCode(w.poll(context.Background()))
	if len(got[CodePageRetire]) != 0 {
		t.Fatalf("events = %+v, want the retirement HISTORY absorbed by the baseline", got)
	}
	if len(got[CodePageRetireExhausted]) != 1 {
		t.Fatal("a GPU already out of spare pages at startup reported nothing; " +
			"the exhausted condition is current, not historical")
	}
}

// TestReplacedDeviceDoesNotInheritCounterHistory covers the RMA case. Series
// are keyed by GPU index (the rocm-smi fallback reports no UUID, so a
// UUID-keyed series would re-baseline on every failover), which means a
// replacement device is otherwise compared against its predecessor's lifetime
// totals.
//
// The dangerous direction is a replacement whose own lifetime counter is
// HIGHER than the baseline the dead device left behind — a refurbished spare,
// or simply a device with a longer history. The difference is then replayed as
// one enormous delta and opens a CRITICAL uncorrectable-ECC incident on a GPU
// that was installed minutes ago. (A replacement that reads LOWER is already
// caught by the counter-went-backwards rule, which is why that case proves
// nothing on its own.)
func TestReplacedDeviceDoesNotInheritCounterHistory(t *testing.T) {
	uuid, uncorr := "amd-gpu-old", 10
	w := testWatcher(t, func(args []string) ([]byte, error) {
		if args[0] == "list" {
			return []byte(`[{"gpu":0,"bdf":"0000:c3:00.0","uuid":"` + uuid + `"}]`), nil
		}
		return []byte(`[{"gpu":0,"ecc":{"total_uncorrectable_count":` + strconv.Itoa(uncorr) + `}}]`), nil
	})
	w.poll(context.Background()) // baselines the dying device at 10

	uuid, uncorr = "amd-gpu-new", 900
	if got := eventsByCode(w.poll(context.Background())); len(got[CodeECCUncorr]) != 0 {
		t.Fatalf("events = %+v: a freshly installed GPU was charged 890 uncorrectable ECC errors "+
			"it inherited from the device it replaced", got)
	}

	// It is baselined, not ignored: its own next error still reports.
	uncorr = 901
	got := eventsByCode(w.poll(context.Background()))
	if len(got[CodeECCUncorr]) != 1 {
		t.Fatalf("events = %+v, want the replacement's OWN first error reported", got)
	}
}

// The other half: a fallback poll that reports no UUID must NOT be read as a
// device swap, or every amd-smi failure would re-baseline the fleet and hide
// whatever went wrong while it was down.
func TestMissingUUIDIsNotReadAsAReplacement(t *testing.T) {
	withUUID, uncorr := true, 100
	w := testWatcher(t, func(args []string) ([]byte, error) {
		if args[0] == "list" {
			id := ""
			if withUUID {
				id = `,"uuid":"amd-gpu-0"`
			}
			return []byte(`[{"gpu":0,"bdf":"0000:c3:00.0"` + id + `}]`), nil
		}
		return []byte(`[{"gpu":0,"ecc":{"total_uncorrectable_count":` + strconv.Itoa(uncorr) + `}}]`), nil
	})
	w.poll(context.Background()) // baseline at 100

	withUUID, uncorr = false, 102
	got := eventsByCode(w.poll(context.Background()))
	if len(got[CodeECCUncorr]) != 1 {
		t.Fatalf("events = %+v, want the increase still reported when the tool omits the UUID", got)
	}
}

// rocmWatcher is a watcher with only the rocm-smi fallback, reporting card0 at
// 0000:c3:00.0 with pages() retired pages.
func rocmWatcher(t *testing.T, pages func() int) *Watcher {
	t.Helper()
	w := &Watcher{NodeName: "amd-node-1", ROCmSMIPath: "rocm-smi", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"card0":{"PCI Bus":"0000:C3:00.0","Total Retired Pages":"` + strconv.Itoa(pages()) + `"}}`), nil
	}
	return w
}
