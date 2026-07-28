// Package kmsg watches the kernel log (/dev/kmsg) for NVIDIA XID error
// lines. This is the fast detection path: an XID reaches the controller
// seconds after the kernel prints it, long before a metrics scrape would.
package kmsg

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// XIDEvent is a parsed NVIDIA XID line from the kernel log.
type XIDEvent struct {
	// XID is the error code (e.g. 79 for "GPU has fallen off the bus").
	XID int
	// PCIAddr is the GPU's PCI address as printed by the driver,
	// e.g. "0000:3b:00". The agent maps it to a GPU index/UUID via NVML.
	PCIAddr string
	// Raw is the message part of the kmsg line, kept for evidence.
	Raw string
	// Timestamp is when the agent read the line (kmsg's own monotonic
	// timestamp is not wall-clock).
	Timestamp time.Time
}

// xidRe matches NVRM Xid lines. Formats seen in the wild:
//
//	NVRM: Xid (PCI:0000:3b:00): 79, pid=1234, GPU has fallen off the bus.
//	NVRM: Xid (0000:af:00): 48, pid='<unknown>', name=<unknown>, Ch 00000008
var xidRe = regexp.MustCompile(`NVRM: Xid \((?:PCI:)?([0-9a-fA-F:.]+)\):? (\d+),`)

// ParseLine extracts an XID event from one kmsg line (or any kernel log
// line), returning ok=false for lines without an XID.
func ParseLine(line string) (XIDEvent, bool) {
	// /dev/kmsg lines are "prefix;message" — parse the message part, but
	// accept bare messages too (tests, journald sources).
	msg := line
	if i := strings.IndexByte(line, ';'); i >= 0 {
		msg = line[i+1:]
	}
	m := xidRe.FindStringSubmatch(msg)
	if m == nil {
		return XIDEvent{}, false
	}
	xid, err := strconv.Atoi(m[2])
	if err != nil {
		return XIDEvent{}, false
	}
	return XIDEvent{
		XID:       xid,
		PCIAddr:   strings.ToLower(m[1]),
		Raw:       strings.TrimSpace(msg),
		Timestamp: time.Now(),
	}, true
}

// Watcher tails a kernel log stream and emits XID events.
type Watcher struct {
	// Path is the kmsg device, default /dev/kmsg. Tests may point it at a
	// FIFO or regular file.
	Path string
}

// NewWatcher builds a watcher for /dev/kmsg.
func NewWatcher() *Watcher { return &Watcher{Path: "/dev/kmsg"} }

// Watch opens the kernel log and streams XID events until ctx is done.
// Reading /dev/kmsg requires CAP_SYSLOG (the agent runs privileged /
// hostPID on Kubernetes, root via systemd on bare metal).
func (w *Watcher) Watch(ctx context.Context) (<-chan XIDEvent, error) {
	f, err := os.Open(w.Path)
	if err != nil {
		return nil, fmt.Errorf("kmsg: %w", err)
	}
	// Seek to the end: only new messages matter; history is covered by the
	// metrics path and the event spool.
	if seeker, ok := any(f).(io.Seeker); ok {
		_, _ = seeker.Seek(0, io.SeekEnd)
	}

	ch := make(chan XIDEvent, 64)
	// The file closes exactly once, from whichever side finishes first: the
	// ctx goroutine (to unblock a reader parked in ReadString) or the reader
	// itself. done also releases the ctx goroutine when the reader exits
	// first, so it cannot leak for the process lifetime.
	var closeOnce sync.Once
	closeFile := func() { closeOnce.Do(func() { _ = f.Close() }) }
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		closeFile()
	}()
	go func() {
		defer close(ch)
		defer close(done)
		defer closeFile()

		r := bufio.NewReader(f)
		for {
			line, err := r.ReadString('\n')
			if len(line) > 0 {
				if ev, ok := ParseLine(line); ok {
					select {
					case ch <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return ch, nil
}
