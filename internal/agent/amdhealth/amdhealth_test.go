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
// EVERY fixture under testdata/ is SYNTHETIC: it was written by hand from
// amd-smi/rocm-smi documentation, never captured from an AMD accelerator, which
// is why each filename says so. They pin this package's PARSING CONTRACT and
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

func TestParseAMDSMIReadsEveryFirstPassSeries(t *testing.T) {
	w := testWatcher(t, nil)
	samples, err := w.parseAMDSMI(readFixture(t, "synthetic-amd-smi-metric-faults.json"))
	if err != nil {
		t.Fatalf("parseAMDSMI: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
	gpu0, gpu1 := samples[0], samples[1]
	if gpu0.index != 0 || gpu0.uuid != "amd-c3000000-0000-1000-8000-000000000001" {
		t.Fatalf("gpu0 identity = %+v", gpu0)
	}
	// The BDF is lowercased so the same device compares equal whichever tool
	// (or kernel line) printed it.
	if gpu0.bdf != "0000:c3:00.0" {
		t.Fatalf("gpu0 bdf = %q, want the lowercased address", gpu0.bdf)
	}
	if gpu0.eccUncorrectable == nil || *gpu0.eccUncorrectable != 2 {
		t.Fatalf("gpu0 uncorrectable ECC = %v, want 2", gpu0.eccUncorrectable)
	}
	if gpu0.retiredPages == nil || *gpu0.retiredPages != 3 {
		t.Fatalf("gpu0 retired pages = %v, want 3", gpu0.retiredPages)
	}
	if gpu1.xgmiErrors == nil || *gpu1.xgmiErrors != 7 {
		t.Fatalf("gpu1 XGMI errors = %v, want 7", gpu1.xgmiErrors)
	}
	if gpu1.thermalThrottled == nil || !*gpu1.thermalThrottled {
		t.Fatalf("gpu1 thermal throttle = %v, want the tool's ACTIVE verdict", gpu1.thermalThrottled)
	}
	// The {"value": N, "unit": "C"} envelope must be understood, not skipped.
	if gpu1.hotspotC == nil || *gpu1.hotspotC != 108 {
		t.Fatalf("gpu1 hotspot = %v, want 108", gpu1.hotspotC)
	}
}

// TestAMDSMICountersBaselineThenEmitOnIncrease is the anti-startup-storm rule:
// the first poll absorbs whatever history the counters carry (they are lifetime
// aggregates of unknown age), and only a later increase is a fault.
func TestAMDSMICountersBaselineThenEmitOnIncrease(t *testing.T) {
	fixture := "synthetic-amd-smi-metric-healthy.json"
	w := testWatcher(t, func(args []string) ([]byte, error) {
		if len(args) == 0 || args[0] != "metric" {
			t.Fatalf("unexpected amd-smi invocation: %v", args)
		}
		return readFixture(t, fixture), nil
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
	if ecc[0].Fault.Attributes["uncorrectable_ecc_errors"] != "2" {
		t.Fatalf("fault attributes = %+v, want the observed counter carried as evidence", ecc[0].Fault.Attributes)
	}
	if len(got[CodePageRetire]) != 1 || got[CodePageRetire][0].GPUIndex != 0 {
		t.Fatalf("page-retirement events = %+v, want one on GPU 0", got[CodePageRetire])
	}
	if len(got[CodeXGMILink]) != 1 || got[CodeXGMILink][0].GPUIndex != 1 {
		t.Fatalf("xgmi-link-error events = %+v, want one on GPU 1", got[CodeXGMILink])
	}
	if len(got[CodeThermal]) != 1 || got[CodeThermal][0].GPUIndex != 1 {
		t.Fatalf("thermal-throttle events = %+v, want one on GPU 1", got[CodeThermal])
	}
	// GPU 1's correctable count did not move (4 -> 4) and no device vanished.
	if len(got[CodeECCCorrRate]) != 0 || len(got[CodeGPULost]) != 0 {
		t.Fatalf("unexpected events: %+v", got)
	}

	// A repeat of the SAME reading is not a new fault: the counters have not
	// grown and the throttle state has not changed.
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
	w := testWatcher(t, func([]string) ([]byte, error) { return readFixture(t, fixture), nil })
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
		CodeECCUncorr, CodeECCCorrRate, CodePageRetire, CodeXGMILink, CodeThermal, CodeGPULost,
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
			reason: "N/A counters, a null, a negative count, an unknown throttle status and a device with no index",
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
	for _, want := range []string{"ecc", "ras", "xgmi", "temperature"} {
		if !strings.Contains(out, `section=`+want) {
			t.Errorf("unreadable %q section was skipped silently; log was: %s", want, out)
		}
	}
	// The device record with no gpu index, and the unrecognized throttle
	// status, are refusals too and must be named rather than assumed.
	if !strings.Contains(out, "without a gpu index") || !strings.Contains(out, "unrecognized thermal throttle status") {
		t.Errorf("an unindexed device and an unknown throttle status must both be reported; log was: %s", out)
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
// trouble: the critical temperature is SKU-specific. Without the tool's own
// throttle flag and without an operator-configured threshold, the temperature
// is observed and nothing more.
func TestThermalRefusesWithoutAThresholdAndReportsWithOne(t *testing.T) {
	const hot = `[{"gpu":0,"bdf":"0000:c3:00.0","temperature":{"hotspot":{"value":109,"unit":"C"}}}]`

	var logged strings.Builder
	w := testWatcher(t, func([]string) ([]byte, error) { return []byte(hot), nil })
	w.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	w.poll(context.Background())
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("poll = %+v, want no thermal fault without a configured threshold", got)
	}
	if !strings.Contains(logged.String(), "observing only") {
		t.Fatalf("the refusal must be visible to an operator; log was: %s", logged.String())
	}

	// With an operator-supplied threshold the same reading is actionable.
	withThreshold := testWatcher(t, func([]string) ([]byte, error) { return []byte(hot), nil })
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
	both := `[{"gpu":0,"bdf":"0000:c3:00.0","uuid":"amd-gpu-0","ecc":{"total_uncorrectable_count":0}},` +
		`{"gpu":1,"bdf":"0000:c6:00.0","uuid":"amd-gpu-1","ecc":{"total_uncorrectable_count":0}}]`
	one := `[{"gpu":0,"bdf":"0000:c3:00.0","uuid":"amd-gpu-0","ecc":{"total_uncorrectable_count":0}}]`

	output := both
	w := testWatcher(t, func([]string) ([]byte, error) { return []byte(output), nil })
	w.poll(context.Background())

	output = one
	got := eventsByCode(w.poll(context.Background()))
	lost := got[CodeGPULost]
	if len(lost) != 1 || lost[0].GPUIndex != 1 {
		t.Fatalf("gpu-lost events = %+v, want exactly one for GPU 1", lost)
	}
	// The vanished device is still named: the identity remembered from the last
	// poll that saw it is what makes the incident actionable.
	if lost[0].GPUUUID != "amd-gpu-1" || lost[0].PCIAddr != "0000:c6:00.0" {
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
	if len(called) != 1 || called[0] != "amd-smi" {
		t.Fatalf("calls = %v, want only amd-smi once it recovers", called)
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
