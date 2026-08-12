package kmsg

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/detect"
)

// TestParseAMDGPULineFamilies is the amdgpu grammar contract.
//
// The lines below are REAL-SHAPED, not captured: they were written from the
// amdgpu driver's message vocabulary without an AMD node to check them against.
// They pin what this parser accepts and — more importantly — what it refuses,
// which is the half that protects a healthy fleet from a wrong guess.
func TestParseAMDGPULineFamilies(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantOK      bool
		wantCode    string
		wantPCI     string
		wantAttrs   map[string]string
		wantRefusal string
		why         string
	}{
		{
			name:     "graphics ring timeout, kmsg framing",
			line:     `3,2201,4053054882,-;amdgpu 0000:c3:00.0: amdgpu: ring gfx_0.0.0 timeout, signaled seq=1148, emitted seq=1150`,
			wantOK:   true,
			wantCode: codeAMDRingTimeout,
			wantPCI:  "0000:c3:00.0",
			// The ring name is evidence: gfx, sdma and compute rings fail for
			// different reasons and an operator triages on it.
			wantAttrs: map[string]string{"ring": "gfx_0.0.0"},
		},
		{
			name:      "compute ring timeout without kmsg prefix",
			line:      `amdgpu 0000:c6:00.0: amdgpu: ring comp_1.0.1 timeout, signaled seq=44, emitted seq=46`,
			wantOK:    true,
			wantCode:  codeAMDRingTimeout,
			wantPCI:   "0000:c6:00.0",
			wantAttrs: map[string]string{"ring": "comp_1.0.1"},
		},
		{
			name:        "soft-recovered ring timeout is not a hang",
			line:        `amdgpu 0000:c6:00.0: amdgpu: ring comp_1.0.1 timeout, but soft recovered`,
			wantOK:      false,
			wantRefusal: amdRefusalSoftRecov,
			// The driver killed the offending job and the engine kept running.
			// No reset, no lost device. Treating this as a hang cordons and
			// drains a node that never stopped working — and a kernel that
			// soft-recovers once tends to do it on a loop.
			why: "a soft recovery is the driver succeeding, not a fault to remediate",
		},
		{
			name:        "RAS all-clear in the countless spelling",
			line:        `3,2212,4053155111,-;amdgpu 0000:c3:00.0: amdgpu: no uncorrectable hardware errors detected in umc block`,
			wantOK:      false,
			wantRefusal: amdRefusalRASClear,
			why:         "the fault spelling is a literal substring of the all-clear; without a negation guard every clean poll opens an ECC incident",
		},
		{
			name:      "RAS uncorrectable errors with a nonzero count",
			line:      `3,2210,4053154882,-;amdgpu 0000:c3:00.0: amdgpu: 2 uncorrectable hardware errors detected in umc block`,
			wantOK:    true,
			wantCode:  codeAMDECCUncorr,
			wantPCI:   "0000:c3:00.0",
			wantAttrs: map[string]string{"uncorrectable_errors": "2", "ras_block": "umc"},
		},
		{
			name:   "RAS report of ZERO uncorrectable errors is a health report",
			line:   `3,2211,4053154999,-;amdgpu 0000:c3:00.0: amdgpu: 0 uncorrectable hardware errors detected in umc block`,
			wantOK: false,
			why:    "the driver prints this line routinely with a zero count; reading it as a fault would open an incident on every healthy poll",
		},
		{
			name:      "RAS uncorrectable event without a count",
			line:      `amdgpu 0000:c3:00.0: amdgpu: RAS event (block: gfx): uncorrectable error detected`,
			wantOK:    true,
			wantCode:  codeAMDECCUncorr,
			wantPCI:   "0000:c3:00.0",
			wantAttrs: map[string]string{"uncorrectable_errors": "unspecified"},
		},
		{
			name:     "bad page reserved is a recorded retirement",
			line:     `amdgpu 0000:c3:00.0: amdgpu: bad page 0x00012340 reserved successfully`,
			wantOK:   true,
			wantCode: codeAMDPageRetire,
			wantPCI:  "0000:c3:00.0",
		},
		{
			name:        "FAILED bad-page reservation refuses rather than claiming a retirement",
			line:        `amdgpu 0000:c3:00.0: amdgpu: failed to reserve bad page 0x00012340`,
			wantOK:      false,
			wantRefusal: amdRefusalRetireBad,
			why:         "a failed reservation is the opposite outcome; filing it as a recorded retirement would report a dying GPU as self-healed",
		},
		{
			name:      "PCI error callback",
			line:      `2,2299,4055554882,-;amdgpu 0000:c3:00.0: amdgpu: PCI error: detected callback, state(1)!!`,
			wantOK:    true,
			wantCode:  codeAMDPCIeError,
			wantPCI:   "0000:c3:00.0",
			wantAttrs: map[string]string{"detail": "detected callback, state(1)!!"},
		},
		{
			name:        "fault line with no device address refuses",
			line:        `amdgpu: ring gfx_0.0.0 timeout, signaled seq=1148, emitted seq=1150`,
			wantOK:      false,
			wantRefusal: amdRefusalNoDevice,
			why:         "without the device prefix the fault cannot be tied to any accelerator on a multi-GPU node",
		},
		{
			name:   "GPU reset lines are the recovery, not the fault",
			line:   `amdgpu 0000:c3:00.0: amdgpu: GPU reset begin!`,
			wantOK: false,
			why:    "the ring timeout that caused it is already reported; matching both would open two incidents for one failure",
		},
		{
			name:   "retry page fault is application-level noise",
			line:   `amdgpu 0000:c3:00.0: amdgpu: [gfxhub0] retry page fault (src_id:0 ring:24 vmid:3 pasid:32771)`,
			wantOK: false,
			why:    "page faults are workload bugs; no shipping AMD source claims them, so the parser stays silent rather than guessing a class",
		},
		{
			name:   "unrelated kernel line",
			line:   `6,4470,3023054000,-;EXT4-fs (nvme0n1p2): mounted filesystem`,
			wantOK: false,
		},
		{
			name:   "amdgpu module load banner",
			line:   `6,1200,4050000000,-;amdgpu 0000:c3:00.0: amdgpu: Fetched VBIOS from VFCT`,
			wantOK: false,
			why:    "informational driver chatter must never look like a fault",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, ok, refusal := parseLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("parseLine(%q) ok = %v, want %v (%s)", tt.line, ok, tt.wantOK, tt.why)
			}
			if refusal != tt.wantRefusal {
				t.Fatalf("refusal = %q, want %q", refusal, tt.wantRefusal)
			}
			if !ok {
				return
			}
			if ev.XID != 0 {
				t.Fatalf("XID = %d, want 0: an amdgpu line has no XID to claim", ev.XID)
			}
			if ev.Fault == nil {
				t.Fatal("an amdgpu event must carry a neutral fault descriptor")
			}
			if ev.Fault.Vendor != faultVendorAMD || ev.Fault.Source != faultSourceKmsg {
				t.Fatalf("fault provenance = %+v, want amd/kmsg", ev.Fault)
			}
			if ev.Fault.Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", ev.Fault.Code, tt.wantCode)
			}
			if ev.PCIAddr != tt.wantPCI {
				t.Fatalf("PCIAddr = %q, want %q", ev.PCIAddr, tt.wantPCI)
			}
			for k, want := range tt.wantAttrs {
				if got := ev.Fault.Attributes[k]; got != want {
					t.Fatalf("attribute %q = %q, want %q", k, got, want)
				}
			}
			if ev.Raw == "" {
				t.Error("Raw must carry the original message for evidence")
			}
			if ev.Timestamp.IsZero() {
				t.Error("Timestamp must be set")
			}
		})
	}
}

// TestNVRMLinesStillParseUnchanged guards the vendor that already worked: the
// amdgpu families are additive, and an Xid line must still produce an XID
// identity with no fault envelope.
func TestNVRMLinesStillParseUnchanged(t *testing.T) {
	ev, ok := ParseLine(`3,4471,3023054882,-;NVRM: Xid (PCI:0000:3b:00): 79, pid=1234, GPU has fallen off the bus.`)
	if !ok || ev.XID != 79 || ev.PCIAddr != "0000:3b:00" {
		t.Fatalf("ParseLine = %+v, %v; want XID 79 on 0000:3b:00", ev, ok)
	}
	if ev.Fault != nil {
		t.Fatalf("Fault = %+v, want nil: a genuine XID must not also claim a neutral identity", ev.Fault)
	}
}

// TestEveryAMDGPUCodeIsClassified is the same contract amdhealth carries: a
// kernel family whose code has no faultTable row is counted and then silently
// drops, which looks like detection and is not.
func TestEveryAMDGPUCodeIsClassified(t *testing.T) {
	for _, code := range []string{codeAMDRingTimeout, codeAMDECCUncorr, codeAMDPageRetire, codeAMDPCIeError} {
		if _, ok := detect.ClassifyFault(faultVendorAMD, code); !ok {
			t.Errorf("amd/%s has no neutral fault row; add one to internal/detect/fault.go", code)
		}
	}
}

// TestAMDGPUEventsRideTheExistingCursorAndAckPath is the §1.2 requirement that
// nothing about delivery is vendor-specific: an amdgpu fault is sequenced,
// acknowledged, and cursored by the same machinery as an Xid, and a mixed
// stream resumes correctly across a restart.
func TestAMDGPUEventsRideTheExistingCursorAndAckPath(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kmsg")
	cursorPath := filepath.Join(dir, "cursor.json")
	content := strings.Join([]string{
		`6,300,100,-;NVRM: Xid (PCI:0000:3b:00): 48, pid=1, Ch 00000008`,
		`6,301,101,-;amdgpu 0000:c3:00.0: amdgpu: ring gfx_0.0.0 timeout, signaled seq=1, emitted seq=2`,
		`6,302,102,-;amdgpu 0000:c3:00.0: amdgpu: GPU reset begin!`,
		`6,303,103,-;amdgpu 0000:c6:00.0: amdgpu: 3 uncorrectable hardware errors detected in umc block`,
		"",
	}, "\n")
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// A prior cursor below the first record forces a seek-to-start (resume)
	// instead of the historical tail-seek, so the fixture is actually read.
	if err := saveCursor(cursorPath, "boot-A", 299); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w := &Watcher{Path: logPath, CursorPath: cursorPath, BootID: "boot-A"}
	events, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	for ev := range events {
		got = append(got, ev)
	}
	// Three events: the Xid, the ring timeout, the RAS report. The GPU-reset
	// line is deliberately not one of them.
	if len(got) != 3 {
		t.Fatalf("delivered %d events (%+v), want 3", len(got), got)
	}
	if got[0].XID != 48 || got[1].Fault == nil || got[1].Fault.Code != codeAMDRingTimeout ||
		got[2].Fault == nil || got[2].Fault.Code != codeAMDECCUncorr {
		t.Fatalf("delivered = %+v, want Xid 48, amd ring-timeout, amd ecc-uncorrectable in order", got)
	}

	// Acknowledging the amdgpu events advances the SAME durable cursor: the
	// Xid before them must be acked first or the contiguous watermark pins.
	for _, ev := range got {
		if err := ev.Acknowledge(); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
	}
	seq, ok, err := loadCursor(cursorPath, "boot-A")
	if err != nil || !ok {
		t.Fatalf("loadCursor = %d, %v, %v; want the amdgpu acknowledgement persisted", seq, ok, err)
	}
	if seq != 303 {
		t.Fatalf("cursor = %d, want 303 (the last acknowledged amdgpu event)", seq)
	}

	// A restart resumes past everything acknowledged, amdgpu lines included.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	w2 := &Watcher{Path: logPath, CursorPath: cursorPath, BootID: "boot-A"}
	events2, err := w2.Watch(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	for ev := range events2 {
		t.Fatalf("replayed %+v after a full acknowledgement; the amdgpu path must use the same cursor semantics", ev)
	}
}

// TestUnackedAMDGPUEventPinsTheCursor is the loss-window guarantee applied to
// the new family: an amdgpu fault that was never durably accepted must replay
// after a restart, exactly like an un-acked Xid.
func TestUnackedAMDGPUEventPinsTheCursor(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kmsg")
	cursorPath := filepath.Join(dir, "cursor.json")
	content := strings.Join([]string{
		`6,400,100,-;amdgpu 0000:c3:00.0: amdgpu: ring gfx_0.0.0 timeout, signaled seq=1, emitted seq=2`,
		`6,401,101,-;NVRM: Xid (PCI:0000:3b:00): 48, pid=1, Ch 00000008`,
		"",
	}, "\n")
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveCursor(cursorPath, "boot-A", 399); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w := &Watcher{Path: logPath, CursorPath: cursorPath, BootID: "boot-A"}
	events, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("delivered = %+v, want 2", got)
	}
	// Only the LATER Xid is acknowledged; the amdgpu event's delivery failed.
	if err := got[1].Acknowledge(); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if seq, ok, _ := loadCursor(cursorPath, "boot-A"); ok && seq >= 400 {
		t.Fatalf("cursor advanced to %d, at/past the un-acked amdgpu seq 400", seq)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	w2 := &Watcher{Path: logPath, CursorPath: cursorPath, BootID: "boot-A"}
	events2, err := w2.Watch(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	replayedAMD := false
	for ev := range events2 {
		if ev.Fault != nil && ev.Fault.Code == codeAMDRingTimeout {
			replayedAMD = true
		}
	}
	if !replayedAMD {
		t.Fatal("the un-acked amdgpu event was not replayed after a restart")
	}
}

// TestRefusalsAreLoggedOnceWithTheOffendingLine keeps a refusal from being an
// unexplained silence, without letting a repeated driver message become a log
// storm that hides everything else.
func TestRefusalsAreLoggedOnceWithTheOffendingLine(t *testing.T) {
	var logged strings.Builder
	w := &Watcher{Logger: slog.New(slog.NewTextHandler(&logged, nil))}
	line := `amdgpu 0000:c3:00.0: amdgpu: failed to reserve bad page 0x00012340`
	for i := 0; i < 5; i++ {
		_, ok, refusal := parseLine(line)
		if ok {
			t.Fatal("a failed reservation must not parse as a fault")
		}
		w.logRefusal(refusal, line)
	}
	out := logged.String()
	if !strings.Contains(out, "reserve bad page") || !strings.Contains(out, "refusing") {
		t.Fatalf("the refusal must name the reason and the line; log was: %s", out)
	}
	if strings.Count(out, "kernel fault line not classified") != 1 {
		t.Fatalf("the refusal must be logged exactly once per reason; log was: %s", out)
	}
}

// TestRefusalLoggingSurvivesANilLogger keeps detection working on a caller that
// configured no logger: refusing to classify must never panic the reader.
func TestRefusalLoggingSurvivesANilLogger(t *testing.T) {
	w := &Watcher{}
	w.logRefusal(amdRefusalNoDevice, "amdgpu: ring gfx timeout")
}
