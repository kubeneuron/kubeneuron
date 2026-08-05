package kmsg

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseSeq(t *testing.T) {
	for _, tc := range []struct {
		line string
		want uint64
		ok   bool
	}{
		{"6,1000,100;NVRM: Xid (PCI:0000:3b:00): 79, ...", 1000, true},
		{"4,9921,9370042162,-;NVRM: Xid (PCI:0000:17:00): 119, ...", 9921, true},
		{"NVRM: Xid (0000:af:00): 48, bare message without prefix", 0, false},
		{"garbage,notnum,x;msg", 0, false},
	} {
		got, ok := parseSeq(tc.line)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseSeq(%q) = %d,%v want %d,%v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCursorRoundTripAndFailSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "kmsg-cursor.json")
	const boot = "boot-A"

	// Missing cursor: not an error, and not usable (fail safe to tail-seek).
	if seq, ok, err := loadCursor(path, boot); err != nil || ok || seq != 0 {
		t.Fatalf("missing cursor = %d,%v,%v; want 0,false,nil", seq, ok, err)
	}

	if err := saveCursor(path, boot, 4242); err != nil {
		t.Fatalf("saveCursor: %v", err)
	}
	seq, ok, err := loadCursor(path, boot)
	if err != nil || !ok || seq != 4242 {
		t.Fatalf("round trip = %d,%v,%v; want 4242,true,nil", seq, ok, err)
	}

	// A zero sequence is treated as no usable cursor: it must not cause a
	// whole-ring replay on every start.
	if err := os.WriteFile(path, []byte(`{"version":1,"boot_id":"boot-A","seq":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := loadCursor(path, boot); ok {
		t.Fatal("seq 0 must not be a usable cursor")
	}

	// A wrong version is unusable, and surfaced as an error for logging.
	if err := os.WriteFile(path, []byte(`{"version":999,"boot_id":"boot-A","seq":5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := loadCursor(path, boot); ok || err == nil {
		t.Fatal("unsupported cursor version must be unusable and reported")
	}

	// Corrupt JSON is unusable and reported, never a panic.
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := loadCursor(path, boot); ok || err == nil {
		t.Fatal("corrupt cursor must be unusable and reported")
	}

	// Empty path disables persistence entirely.
	if err := saveCursor("", boot, 1); err != nil {
		t.Fatalf("saveCursor(\"\") = %v, want nil", err)
	}
	if seq, ok, err := loadCursor("", boot); err != nil || ok || seq != 0 {
		t.Fatalf("loadCursor(\"\") = %d,%v,%v; want 0,false,nil", seq, ok, err)
	}
}

// TestCursorBootScoping is the regression test for the cross-boot suppression
// bug: /dev/kmsg sequence numbers restart near zero after a reboot, so a cursor
// carrying a pre-reboot (large) seq must not be trusted once the boot changes.
func TestCursorBootScoping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kmsg-cursor.json")

	// A cursor recorded under boot A at a high sequence.
	if err := saveCursor(path, "boot-A", 500000); err != nil {
		t.Fatalf("saveCursor: %v", err)
	}
	// Same boot: the cursor resumes as recorded.
	if seq, ok, err := loadCursor(path, "boot-A"); err != nil || !ok || seq != 500000 {
		t.Fatalf("same-boot load = %d,%v,%v; want 500000,true,nil", seq, ok, err)
	}
	// After a reboot (boot B) the stale seq must be ignored, not surfaced as an
	// error — a reboot is the ordinary post-remediation state. The watcher then
	// fails safe to tail-seek instead of suppressing every new XID.
	if seq, ok, err := loadCursor(path, "boot-B"); err != nil || ok || seq != 0 {
		t.Fatalf("cross-boot load = %d,%v,%v; want 0,false,nil", seq, ok, err)
	}

	// An old cursor with no boot ID at all (written before boot binding) is
	// likewise treated as a mismatch, not a usable resume point.
	if err := os.WriteFile(path, []byte(`{"version":1,"seq":500000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if seq, ok, err := loadCursor(path, "boot-A"); err != nil || ok || seq != 0 {
		t.Fatalf("boot-less cursor = %d,%v,%v; want 0,false,nil", seq, ok, err)
	}

	// An unknown live boot ID (non-Linux dev host) also refuses to resume.
	if err := saveCursor(path, "boot-A", 500000); err != nil {
		t.Fatal(err)
	}
	if seq, ok, err := loadCursor(path, ""); err != nil || ok || seq != 0 {
		t.Fatalf("unknown-boot load = %d,%v,%v; want 0,false,nil", seq, ok, err)
	}
}

// TestWatchResumesFromCursorAfterRestart is the regression test for the
// tail-seek loss bug: an XID printed while the agent was down (higher sequence
// than the persisted cursor) must be delivered on restart, while everything at
// or below the cursor is skipped as already consumed.
func TestWatchResumesFromCursorAfterRestart(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kmsg")
	cursorPath := filepath.Join(dir, "cursor.json")

	// seq 10 was already consumed before the restart; seq 20 arrived while the
	// agent was down; seq 30 arrives after it comes back.
	content := "6,10,100;NVRM: Xid (PCI:0000:3b:00): 31, old already-seen\n" +
		"6,20,200;NVRM: Xid (PCI:0000:3b:00): 48, missed while down\n" +
		"6,30,300;NVRM: Xid (PCI:0000:af:00): 79, after restart\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveCursor(cursorPath, "boot-A", 10); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w := &Watcher{Path: logPath, CursorPath: cursorPath, BootID: "boot-A"}
	events, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var got []int
	for ev := range events {
		got = append(got, ev.XID)
		if err := ev.Acknowledge(); err != nil {
			t.Fatalf("acknowledge XID %d: %v", ev.XID, err)
		}
	}
	// seq 10 skipped (<= cursor); 48 (missed) and 79 (new) delivered in order.
	if len(got) != 2 || got[0] != 48 || got[1] != 79 {
		t.Fatalf("resumed XIDs = %v, want [48 79]", got)
	}

	// The cursor advanced to the last consumed record.
	seq, ok, err := loadCursor(cursorPath, "boot-A")
	if err != nil || !ok || seq != 30 {
		t.Fatalf("cursor after replay = %d,%v,%v; want 30,true,nil", seq, ok, err)
	}
}

// The cursor must not move when a kmsg event was merely placed into an
// in-memory channel. The agent acknowledges only after POST or spool fsync.
func TestWatchCursorAdvancesOnlyAfterAcknowledge(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kmsg")
	cursorPath := filepath.Join(dir, "cursor.json")
	if err := os.WriteFile(logPath,
		[]byte("6,20,200;NVRM: Xid (PCI:0000:3b:00): 48, pending delivery\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveCursor(cursorPath, "boot-A", 10); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := (&Watcher{Path: logPath, CursorPath: cursorPath, BootID: "boot-A"}).Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := <-events
	if !ok || ev.XID != 48 {
		t.Fatalf("event = %+v, open=%v; want XID 48", ev, ok)
	}
	for range events {
	}
	if seq, ok, err := loadCursor(cursorPath, "boot-A"); err != nil || !ok || seq != 10 {
		t.Fatalf("cursor before acknowledge = %d,%v,%v; want 10,true,nil", seq, ok, err)
	}
	if err := ev.Acknowledge(); err != nil {
		t.Fatal(err)
	}
	if seq, ok, err := loadCursor(cursorPath, "boot-A"); err != nil || !ok || seq != 20 {
		t.Fatalf("cursor after acknowledge = %d,%v,%v; want 20,true,nil", seq, ok, err)
	}
}

func TestWatchWithoutCursorStillTailSeeks(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kmsg")
	cursorPath := filepath.Join(dir, "cursor.json") // never written

	if err := os.WriteFile(logPath, []byte("6,1,1;NVRM: Xid (PCI:0000:3b:00): 79, historical\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w := &Watcher{Path: logPath, CursorPath: cursorPath}
	events, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// No cursor => fail safe to tail-seek: history is skipped, channel closes.
	for ev := range events {
		t.Fatalf("unexpected historical event %+v with no cursor", ev)
	}
	if _, err := os.Stat(cursorPath); !os.IsNotExist(err) {
		t.Fatalf("no cursor file should be created when none existed: %v", err)
	}
}

func TestCursorFileIsWellFormed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	if err := saveCursor(path, "boot-A", 7); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cf cursorFile
	if err := json.Unmarshal(data, &cf); err != nil {
		t.Fatalf("cursor is not valid JSON: %v", err)
	}
	if cf.Version != cursorVersion || cf.Seq != 7 || cf.BootID != "boot-A" {
		t.Fatalf("cursor = %+v, want version %d seq 7 boot boot-A", cf, cursorVersion)
	}
}
