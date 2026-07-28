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

func TestWatchFailsWithoutDevice(t *testing.T) {
	w := &Watcher{Path: filepath.Join(t.TempDir(), "missing")}
	if _, err := w.Watch(context.Background()); err == nil {
		t.Fatal("missing kmsg device must be an error, not a silent no-op")
	}
}
