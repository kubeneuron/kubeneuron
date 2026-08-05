package gpuhealth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// isDCGM reports whether an injected run call is the dcgmi dmon probe.
func isDCGM(args []string) bool {
	for _, a := range args {
		if a == "dmon" {
			return true
		}
	}
	return false
}

func TestParseDCGMXIDSkipsHealthyAndNA(t *testing.T) {
	got, err := parseDCGMXID(readFixture(t, "dcgmi-dmon-xid.txt"))
	if err != nil {
		t.Fatalf("parseDCGMXID: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("candidates = %+v, want exactly one (GPU 1 XID 79)", got)
	}
	c := got[0]
	if c.index != 1 || c.xid != 79 || !c.level || c.value != 79 {
		t.Fatalf("candidate = %+v, want index 1 xid 79 level", c)
	}
}

func TestParseSMIHealthReadsVolatileNotAggregate(t *testing.T) {
	got := parseSMIHealth(readFixture(t, "nvidia-smi-q-faults.txt"))
	// GPU 0: volatile DRAM uncorrectable = 2 (aggregate 9 must be ignored).
	// GPU 1: remap failure occurred.
	// R6: the nvidia-smi fallback no longer synthesizes XIDs; it emits neutral
	// NVIDIA faults. Identify the candidates by their vendor-native fault code.
	var ecc0, remap1 *candidate
	for i := range got {
		switch {
		case got[i].index == 0 && got[i].fault != nil && got[i].fault.Code == codeECCDBE:
			ecc0 = &got[i]
		case got[i].index == 1 && got[i].fault != nil && got[i].fault.Code == codeRowRemapFailure:
			remap1 = &got[i]
		}
	}
	if ecc0 == nil || ecc0.value != 2 {
		t.Fatalf("GPU0 ECC candidate = %+v, want volatile uncorrectable 2", ecc0)
	}
	if ecc0.uuid != "GPU-aaaa1111-bbbb-2222-cccc-333344445555" {
		t.Fatalf("GPU0 UUID = %q, want inline nvidia-smi UUID", ecc0.uuid)
	}
	if remap1 == nil || remap1.value != 1 {
		t.Fatalf("GPU1 remap candidate = %+v, want failure value 1", remap1)
	}
}

func TestDCGMLevelTriggeredRecoversThenSuppresses(t *testing.T) {
	w := &Watcher{NodeName: "gpu-node", DCGMPath: "dcgmi"}
	w.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if !isDCGM(args) {
			t.Fatalf("unexpected non-dcgm call: %v", args)
		}
		return []byte(readFixture(t, "dcgmi-dmon-xid.txt")), nil
	}

	// First poll emits the retained XID: this is the recovery of a fault that
	// may have been printed while the agent was down.
	first := w.poll(context.Background())
	if len(first) != 1 || first[0].XID != 79 || first[0].GPUIndex != 1 || first[0].Node != "gpu-node" {
		t.Fatalf("first poll = %+v, want one XID 79 on GPU 1", first)
	}
	// An unchanged repeat is suppressed: the field still reads the same XID.
	if second := w.poll(context.Background()); len(second) != 0 {
		t.Fatalf("second identical poll = %+v, want none", second)
	}
}

func TestSMICounterBaselinedThenEmitsOnIncrease(t *testing.T) {
	fixture := "nvidia-smi-q-healthy.txt"
	w := &Watcher{NodeName: "gpu-node", NVIDIASMIPath: "nvidia-smi"}
	w.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) == 0 || args[0] != "-q" {
			t.Fatalf("unexpected non-smi call: %v", args)
		}
		return []byte(readFixture(t, fixture)), nil
	}

	// First poll only baselines the historical counters: a pre-existing
	// aggregate must never be replayed as a fresh fault on startup.
	if first := w.poll(context.Background()); len(first) != 0 {
		t.Fatalf("baseline poll = %+v, want no events", first)
	}

	// Now a genuine increase: GPU0 volatile uncorrectable 0 -> 2, GPU1 remap
	// No -> Yes.
	fixture = "nvidia-smi-q-faults.txt"
	got := w.poll(context.Background())
	var sawECC, sawRemap bool
	for _, ev := range got {
		// R6: emitted events carry a neutral NVIDIA fault, not a synthesized XID.
		if ev.GPUIndex == 0 && ev.XID == 0 && ev.Fault != nil && ev.Fault.Code == codeECCDBE {
			sawECC = true
		}
		if ev.GPUIndex == 1 && ev.XID == 0 && ev.Fault != nil && ev.Fault.Code == codeRowRemapFailure {
			sawRemap = true
		}
	}
	if !sawECC || !sawRemap {
		t.Fatalf("increase poll = %+v, want ECC(GPU0) and remap(GPU1) events", got)
	}
}

// dmonForXIDs renders a dcgmi dmon table reporting the given per-index last-XID.
func dmonForXIDs(xids map[int]int) []byte {
	b := strings.Builder{}
	b.WriteString("#Entity XIDERRORS\n")
	for idx, xid := range xids {
		b.WriteString("GPU " + strconv.Itoa(idx) + " " + strconv.Itoa(xid) + "\n")
	}
	return []byte(b.String())
}

// TestDCGMDurableStateSuppressesRetainedXIDAcrossRestart is the regression test
// for re-emitting DCGM's retained last-XID on every agent restart. The XID field
// retains the last code for the life of nv-hostengine, so a process-local "first
// sight" baseline re-emits it on every DaemonSet rollout and re-opens incidents
// for long-remediated faults. Persisting the emitted (GPU, code) durably and
// boot-scoped fixes this: a restart on the same boot does not re-emit an
// already-persisted XID, a genuinely new code still emits, and a boot-ID change
// resets the record.
func TestDCGMDurableStateSuppressesRetainedXIDAcrossRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "gpuhealth-state.json")
	const bootA = "boot-aaaa"

	newWatcher := func(bootID string, xids map[int]int) *Watcher {
		w := &Watcher{NodeName: "gpu-node", DCGMPath: "dcgmi", StatePath: statePath, BootID: bootID}
		w.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if !isDCGM(args) {
				t.Fatalf("unexpected non-dcgm call: %v", args)
			}
			return dmonForXIDs(xids), nil
		}
		return w
	}

	// First agent, boot A: GPU 1 retains XID 79. With no durable record yet, the
	// fail-safe posture baselines on first sight rather than re-emitting a
	// possibly long-remediated retained XID — and persists that it was accounted
	// for so a restart stays quiet.
	w1 := newWatcher(bootA, map[int]int{1: 79})
	if got := w1.poll(context.Background()); len(got) != 0 {
		t.Fatalf("first poll = %+v, want none (baseline on first sight, no durable record)", got)
	}

	// Second agent, SAME boot A (a routine DaemonSet restart): the retained XID
	// is the same 79 and must NOT be re-emitted — this is the bug being fixed.
	w2 := newWatcher(bootA, map[int]int{1: 79})
	if got := w2.poll(context.Background()); len(got) != 0 {
		t.Fatalf("restart poll = %+v, want none (retained XID already accounted for this boot)", got)
	}

	// A genuinely new code on the same boot IS a fresh fault and must emit.
	w3 := newWatcher(bootA, map[int]int{1: 79, 2: 94})
	got := w3.poll(context.Background())
	if len(got) != 1 || got[0].GPUIndex != 2 || got[0].XID != 94 {
		t.Fatalf("new-code poll = %+v, want exactly one GPU 2 XID 94", got)
	}
	// And once persisted, that new code is not re-emitted on the next restart.
	w4 := newWatcher(bootA, map[int]int{1: 79, 2: 94})
	if got := w4.poll(context.Background()); len(got) != 0 {
		t.Fatalf("post-new-code restart poll = %+v, want none", got)
	}

	// A boot-ID change resets the record: the reboot cleared nv-hostengine's
	// retained field, so the durable state must not suppress the new boot. With
	// the record reset, the first sight on boot B baselines again (no re-emit of
	// a stale cross-boot XID).
	w5 := newWatcher("boot-bbbb", map[int]int{1: 79})
	if got := w5.poll(context.Background()); len(got) != 0 {
		t.Fatalf("post-reboot poll = %+v, want none (boot change resets to a fresh baseline)", got)
	}
}

// TestDCGMLevelFaultAfterCleanStartIsEmitted is the regression test for the
// durable-baseline swallowing the first FRESH fault after a reboot. After a
// reboot the persisted state is cross-boot, so baselineFirstSight is set for the
// whole process. The old code suppressed the first sight of ANY level series for
// the process lifetime — so a GPU clean at startup that develops XID 79 two hours
// in was baselined (and the suppression persisted), making the fault permanently
// invisible to this second source. First-sight suppression must apply only to a
// series already nonzero at process start; a clean-at-start series that later
// becomes nonzero is a genuine new fault and must emit.
func TestDCGMLevelFaultAfterCleanStartIsEmitted(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "gpuhealth-state.json")
	const bootA = "boot-aaaa"

	xids := map[int]int{} // GPU clean at startup: no retained XID.
	w := &Watcher{NodeName: "gpu-node", DCGMPath: "dcgmi", StatePath: statePath, BootID: bootA}
	w.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if !isDCGM(args) {
			t.Fatalf("unexpected non-dcgm call: %v", args)
		}
		return dmonForXIDs(xids), nil
	}

	// First poll: clean. Nothing to emit and nothing to baseline (absent series).
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("first poll = %+v, want none (GPU clean at start)", got)
	}

	// Two hours in, a genuinely new retained XID develops on GPU 1. This is an
	// absent/zero -> nonzero transition and MUST be emitted, not swallowed.
	xids = map[int]int{1: 79}
	got := w.poll(context.Background())
	if len(got) != 1 || got[0].GPUIndex != 1 || got[0].XID != 79 {
		t.Fatalf("post-start fault poll = %+v, want exactly one GPU 1 XID 79 emitted", got)
	}

	// The emission is persisted, so a restart on the same boot stays quiet (the
	// fault stays accounted-for/emitted rather than re-opening a new incident), and
	// crucially is NOT re-swallowed by a fresh first-sight baseline.
	w2 := &Watcher{NodeName: "gpu-node", DCGMPath: "dcgmi", StatePath: statePath, BootID: bootA}
	w2.run = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return dmonForXIDs(map[int]int{1: 79}), nil
	}
	if got := w2.poll(context.Background()); len(got) != 0 {
		t.Fatalf("restart poll = %+v, want none (emitted fault stays accounted, not re-emitted)", got)
	}
}

// TestDCGMWithoutStatePathKeepsEmitOnFirstSight confirms the in-memory behavior
// is unchanged when no durable state file is configured (unit tests, CPU-only
// installs): the retained XID is still recovered on first sight.
func TestDCGMWithoutStatePathKeepsEmitOnFirstSight(t *testing.T) {
	w := &Watcher{NodeName: "gpu-node", DCGMPath: "dcgmi"} // no StatePath
	w.run = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return dmonForXIDs(map[int]int{1: 79}), nil
	}
	got := w.poll(context.Background())
	if len(got) != 1 || got[0].XID != 79 {
		t.Fatalf("first poll = %+v, want one XID 79 emitted on first sight", got)
	}
}

func TestDCGMFailureFallsBackToNVIDIASMI(t *testing.T) {
	w := &Watcher{NodeName: "gpu-node", DCGMPath: "dcgmi", NVIDIASMIPath: "nvidia-smi"}
	w.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if isDCGM(args) {
			return []byte("Error: Unable to connect to host engine\n"), errors.New("exit status 1")
		}
		return []byte(readFixture(t, "nvidia-smi-q-faults.txt")), nil
	}
	// First poll (fallback) baselines; second sees the fault again — but since
	// the same fixture is returned, the counter is unchanged after baseline,
	// so nothing is emitted. Prime a healthy baseline by hand instead.
	w.seen = map[string]uint64{"smi-ecc-0": 0, "smi-remap-1": 0}
	w.baselined = map[string]bool{"smi-ecc-0": true, "smi-remap-1": true}
	got := w.poll(context.Background())
	if len(got) == 0 {
		t.Fatalf("expected nvidia-smi fallback to produce events after a dcgmi failure")
	}
}

func TestNoUsableSourceDegradesToObservedOnly(t *testing.T) {
	w := &Watcher{NodeName: "gpu-node"} // no DCGMPath, no NVIDIASMIPath
	w.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("no source configured: run must not be called")
		return nil, nil
	}
	if got := w.poll(context.Background()); len(got) != 0 {
		t.Fatalf("degraded poll = %+v, want no events and no crash", got)
	}
}

func TestNewDisablesAbsentBinaries(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope-dcgmi")
	w := New("gpu-node", missing, "", filepath.Join(t.TempDir(), "nope-smi"))
	if w.DCGMPath != "" || w.NVIDIASMIPath != "" {
		t.Fatalf("New must disable absent binaries, got dcgm=%q smi=%q", w.DCGMPath, w.NVIDIASMIPath)
	}
	// A present binary is enabled (use the test shell, guaranteed to exist).
	if _, err := os.Stat("/bin/sh"); err == nil {
		w2 := New("gpu-node", "/bin/sh", "", "")
		if w2.DCGMPath != "/bin/sh" {
			t.Fatalf("New must enable a present binary, got %q", w2.DCGMPath)
		}
	}
}

func TestResolveGPUFillsUUIDForDCGMPath(t *testing.T) {
	w := &Watcher{
		NodeName: "gpu-node",
		DCGMPath: "dcgmi",
		ResolveGPU: func(_ context.Context, index int) (types.GPUInfo, bool) {
			return types.GPUInfo{Index: index, UUID: "GPU-resolved-1"}, true
		},
	}
	w.run = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte(readFixture(t, "dcgmi-dmon-xid.txt")), nil
	}
	got := w.poll(context.Background())
	if len(got) != 1 || got[0].GPUUUID != "GPU-resolved-1" {
		t.Fatalf("poll = %+v, want resolved UUID for GPU 1", got)
	}
}

func TestDCGMEndpointFollowsSubcommand(t *testing.T) {
	var captured []string
	w := &Watcher{NodeName: "gpu-node", DCGMPath: "dcgmi", DCGMEndpoint: "10.0.1.7:5555"}
	w.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		captured = args
		return []byte("#Entity XIDERRORS\n"), nil
	}
	_ = w.poll(context.Background())
	want := []string{"dmon", "--host", "10.0.1.7:5555", "-e", dcgmXIDFieldID, "-c", "1"}
	if strings.Join(captured, " ") != strings.Join(want, " ") {
		t.Fatalf("dcgmi args = %v, want %v", captured, want)
	}
}
