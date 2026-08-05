package kmsg

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestParseLine(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantXID int
		wantPCI string
		wantOK  bool
	}{
		{
			name:    "fell off the bus, kmsg framing",
			line:    `3,4471,3023054882,-;NVRM: Xid (PCI:0000:3b:00): 79, pid=1234, GPU has fallen off the bus.`,
			wantXID: 79,
			wantPCI: "0000:3b:00",
			wantOK:  true,
		},
		{
			name:    "double bit ECC without PCI prefix",
			line:    `NVRM: Xid (0000:af:00): 48, pid='<unknown>', name=<unknown>, Ch 00000008`,
			wantXID: 48,
			wantPCI: "0000:af:00",
			wantOK:  true,
		},
		{
			name:    "GSP timeout with function suffix",
			line:    `4,9921,9370042162,-;NVRM: Xid (PCI:0000:17:00): 119, pid=5678, name=python3, Timeout waiting for RPC from GSP`,
			wantXID: 119,
			wantPCI: "0000:17:00",
			wantOK:  true,
		},
		{
			name:    "page fault",
			line:    `NVRM: Xid (PCI:0000:86:00): 31, pid=2468, name=trainer, Ch 00000010, intr 10000000`,
			wantXID: 31,
			wantPCI: "0000:86:00",
			wantOK:  true,
		},
		{
			name:   "unrelated kernel line",
			line:   `6,4470,3023054000,-;EXT4-fs (nvme0n1p2): mounted filesystem`,
			wantOK: false,
		},
		{
			name:   "nvidia line without xid",
			line:   `NVRM: loading NVIDIA UNIX x86_64 Kernel Module 550.54.15`,
			wantOK: false,
		},
		{
			name:   "empty",
			line:   "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, ok := ParseLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ParseLine(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if ev.XID != tt.wantXID {
				t.Errorf("XID = %d, want %d", ev.XID, tt.wantXID)
			}
			if ev.PCIAddr != tt.wantPCI {
				t.Errorf("PCIAddr = %q, want %q", ev.PCIAddr, tt.wantPCI)
			}
			if ev.Raw == "" {
				t.Error("Raw must carry the original message for evidence")
			}
		})
	}
}

func TestWatchStreamsXIDEventsFromAFIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kmsg")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	// O_RDWR: opening a FIFO write-only blocks until a reader appears, and
	// the watcher (the reader) starts below.
	writer, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &Watcher{Path: path}
	events, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	lines := []string{
		"6,1000,100;NVRM: Xid (PCI:0000:3b:00): 79, pid=1234, GPU has fallen off the bus.\n",
		"6,1001,101;usb 1-1: new high-speed USB device\n", // no XID: must be skipped
		"6,1002,102;NVRM: Xid (0000:af:00): 48, pid='<unknown>', name=<unknown>, Ch 00000008\n",
	}
	for _, line := range lines {
		if _, err := writer.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}

	first := <-events
	if first.XID != 79 || first.PCIAddr != "0000:3b:00" {
		t.Fatalf("first event = %+v", first)
	}
	second := <-events
	if second.XID != 48 || second.PCIAddr != "0000:af:00" {
		t.Fatalf("second event = %+v", second)
	}

	// Cancellation ends the stream: the channel closes instead of leaking a
	// reader parked on the FIFO forever.
	cancel()
	select {
	case _, open := <-events:
		if open {
			t.Fatal("expected the event channel to close after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event channel did not close after cancellation")
	}
}

func TestWatchSeeksToTailOfRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kmsg")
	historical := "6,1,1;NVRM: Xid (PCI:0000:3b:00): 79, pid=1, historical line\n"
	if err := os.WriteFile(path, []byte(historical), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &Watcher{Path: path}
	events, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// History must be skipped: the reader hits EOF immediately and exits.
	select {
	case ev, open := <-events:
		if open {
			t.Fatalf("unexpected historical event %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not finish reading a regular file")
	}
}

// TestWatchDeliversLowSeqAfterReboot is the regression test for the cross-boot
// cursor suppression bug. A durable cursor from the pre-reboot boot sits at a
// high sequence (500000); after a reboot the ring's sequence restarts near zero
// (42). With the old boot-blind cursor the watcher discarded every record with
// seq <= 500000, so a recurring XID was silently suppressed. Binding the cursor
// to the boot ID makes the stale cursor a no-op (fail safe to tail-seek), so the
// low-sequence XID from the new boot is delivered.
func TestWatchDeliversLowSeqAfterReboot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kmsg")
	cursorPath := filepath.Join(dir, "cursor.json")

	// Pre-reboot cursor: boot-A at a high sequence.
	if err := saveCursor(cursorPath, "boot-A", 500000); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	writer, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Fresh boot: kmsg sequence numbers have restarted near zero.
	w := &Watcher{Path: path, CursorPath: cursorPath, BootID: "boot-B"}
	events, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// seq 42 is far below the stale 500000 cursor; under the old rule this would
	// be suppressed and the read below would time out.
	if _, err := writer.WriteString(
		"6,42,100;NVRM: Xid (PCI:0000:3b:00): 79, recurring after reboot\n"); err != nil {
		t.Fatal(err)
	}

	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("event channel closed without delivering the post-reboot XID")
		}
		if ev.XID != 79 {
			t.Fatalf("delivered XID = %d, want 79", ev.XID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-reboot low-seq XID was suppressed by a stale cross-boot cursor")
	}
}

// TestCursorDoesNotAdvancePastAnUnacknowledgedGap is the regression test for the
// per-event cursor advancing past an undelivered lower sequence. If seq 100 fails
// BOTH the controller POST and the spool append (controller down + ENOSPC — the
// exact regime the spool exists for) it is never acknowledged. Under the old
// per-event cursor, when the LATER seq 101 delivered and acked, the durable cursor
// jumped to 101, and a restart replayed nothing <= 101 — losing seq 100 forever.
// The contiguous-ack watermark pins the cursor just below the gap so seq 100
// replays after a restart.
func TestCursorDoesNotAdvancePastAnUnacknowledgedGap(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kmsg")
	cursorPath := filepath.Join(dir, "cursor.json")

	// A prior cursor at seq 50 forces a seek-to-start (resume), so both records
	// below are read rather than tail-seeked past.
	if err := saveCursor(cursorPath, "boot-A", 50); err != nil {
		t.Fatal(err)
	}
	content := "6,100,1000;NVRM: Xid (PCI:0000:3b:00): 79, failed both post and spool\n" +
		"6,101,1010;NVRM: Xid (PCI:0000:af:00): 48, delivered and acknowledged\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
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
	if len(got) != 2 || got[0].XID != 79 || got[1].XID != 48 {
		t.Fatalf("delivered = %+v, want XID 79 (seq100) then XID 48 (seq101)", got)
	}
	// seq 100 (XID 79) fails delivery entirely and is NEVER acknowledged; only the
	// later seq 101 (XID 48) is acknowledged.
	if err := got[1].Acknowledge(); err != nil {
		t.Fatalf("acknowledge seq 101: %v", err)
	}

	// The persisted cursor must NOT have advanced to or past the un-acked seq 100.
	seq, ok, err := loadCursor(cursorPath, "boot-A")
	if err != nil {
		t.Fatalf("loadCursor: %v", err)
	}
	if ok && seq >= 100 {
		t.Fatalf("cursor advanced to %d, at/past the un-acked seq 100; event 100 would be lost forever", seq)
	}

	// Simulated restart: seq 100 must be replayed (redelivered) because the cursor
	// was pinned below it.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	w2 := &Watcher{Path: logPath, CursorPath: cursorPath, BootID: "boot-A"}
	events2, err := w2.Watch(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	var replayed []int
	for ev := range events2 {
		replayed = append(replayed, ev.XID)
	}
	found := false
	for _, x := range replayed {
		if x == 79 {
			found = true
		}
	}
	if !found {
		t.Fatalf("after restart replayed = %v, want the un-acked seq 100 (XID 79) redelivered", replayed)
	}
}

func TestWatchFailsWithoutDevice(t *testing.T) {
	w := &Watcher{Path: filepath.Join(t.TempDir(), "missing")}
	if _, err := w.Watch(context.Background()); err == nil {
		t.Fatal("missing kmsg device must be an error, not a silent no-op")
	}
}
