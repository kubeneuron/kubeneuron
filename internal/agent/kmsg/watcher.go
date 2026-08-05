// Package kmsg watches the kernel log (/dev/kmsg) for NVIDIA XID error
// lines. This is the fast detection path: an XID reaches the controller
// seconds after the kernel prints it, long before a metrics scrape would.
package kmsg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

	// acknowledge is installed for events that have a /dev/kmsg sequence and
	// durable cursoring enabled. It deliberately remains private: consumers
	// must call Acknowledge only after the event is durably accepted by the
	// controller or its local outbox.
	acknowledge func() error
}

// Acknowledge advances the durable /dev/kmsg cursor for this event. It is a
// no-op for non-kmsg inputs and is safe to call more than once. A caller must
// acknowledge only after the event has either been accepted by the controller
// or fsynced to a local retry spool; otherwise a restart could skip an event
// that was only handed to an in-memory channel.
func (e XIDEvent) Acknowledge() error {
	if e.acknowledge == nil {
		return nil
	}
	return e.acknowledge()
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
	// CursorPath, when set, is a durable file recording the last kmsg
	// sequence number an XID was accepted by the downstream durable delivery
	// path. It closes the tail-seek loss window: after a restart the watcher
	// re-reads the ring from its oldest buffered record and skips everything at
	// or below the persisted sequence, recovering any XID printed while the
	// agent was down. Empty keeps the historical tail-seek behavior (used by
	// tests and any caller without durable node-local storage).
	CursorPath string
	// BootID binds the cursor to the current node boot. /dev/kmsg sequence
	// numbers restart near zero after a reboot, so a cursor recorded under a
	// different boot must not be trusted: loadCursor treats a boot mismatch (or
	// an empty BootID) as "no cursor" and the watcher fails safe to tail-seek.
	// Empty when the agent cannot read the host boot ID (non-Linux dev hosts),
	// which simply disables cross-restart resume.
	BootID string
	// Logger, when set, records cursor load/persist problems. Detection still
	// proceeds (fail safe to tail-seek) when it is nil.
	Logger *slog.Logger

	// cursorMu guards the contiguous-ack cursor state below. Acknowledgements
	// arrive from the agent's event-loop goroutine while emissions are tracked
	// from the reader goroutine, so the two must not race.
	cursorMu sync.Mutex
	// watermark is the highest sequence safe to persist: every emitted XID at or
	// below it has been acknowledged. It never advances past an un-acked (pending)
	// lower sequence, so a delivery gap pins the durable cursor below the gap and
	// the un-acked event replays after a restart instead of being lost.
	watermark uint64
	// maxAcked is the highest acknowledged sequence, used to raise the watermark
	// once a gap below it finally drains.
	maxAcked uint64
	// pending is the set of emitted-but-not-yet-acknowledged sequences. A
	// permanent gap keeps exactly its own sequence here (O(1) memory), pinning the
	// watermark just below it until that event is redelivered and acknowledged.
	pending map[uint64]struct{}
}

// NewWatcher builds a watcher for /dev/kmsg.
func NewWatcher() *Watcher { return &Watcher{Path: "/dev/kmsg"} }

// parseSeq extracts the monotonic sequence number from a /dev/kmsg record
// prefix ("priority,seq,timestamp,flags;message"). It returns ok=false for
// bare messages without a prefix (tests, journald sources), which therefore
// bypass sequence filtering and are always processed.
func parseSeq(line string) (uint64, bool) {
	i := strings.IndexByte(line, ';')
	if i < 0 {
		return 0, false
	}
	fields := strings.Split(line[:i], ",")
	if len(fields) < 2 {
		return 0, false
	}
	seq, err := strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

// Watch opens the kernel log and streams XID events until ctx is done.
// Reading /dev/kmsg requires CAP_SYSLOG (the agent runs privileged /
// hostPID on Kubernetes, root via systemd on bare metal).
func (w *Watcher) Watch(ctx context.Context) (<-chan XIDEvent, error) {
	f, err := os.Open(w.Path)
	if err != nil {
		return nil, fmt.Errorf("kmsg: %w", err)
	}

	// A persisted cursor lets a restarted agent resume where it left off. A
	// missing or unreadable cursor fails safe to the historical behavior:
	// seek to the tail so only new messages are read and the whole ring is
	// never replayed uncontrolled.
	resumeSeq, haveCursor, cerr := loadCursor(w.CursorPath, w.BootID)
	if cerr != nil && w.Logger != nil {
		w.Logger.Warn("kmsg cursor unreadable, resuming at tail", "err", cerr)
	}
	// Seed the in-memory contiguous-ack watermark from the persisted cursor so a
	// reopen never regresses it, and so acknowledgements below the resume point
	// are treated as already durable.
	w.initCursorState(resumeSeq)
	if seeker, ok := any(f).(io.Seeker); ok {
		if haveCursor {
			// SEEK_SET/0 positions /dev/kmsg at the first record still in the
			// kernel ring buffer (the start of a regular file in tests); the
			// sequence filter below discards records already consumed.
			_, _ = seeker.Seek(0, io.SeekStart)
		} else {
			// Only new messages matter; history is covered by the metrics
			// path and the event spool.
			_, _ = seeker.Seek(0, io.SeekEnd)
		}
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
				seq, hasSeq := parseSeq(line)
				skip := haveCursor && hasSeq && seq <= resumeSeq
				if !skip {
					if ev, ok := ParseLine(line); ok {
						if hasSeq {
							// Register the sequence as pending BEFORE it is handed
							// out, so a later ack cannot advance the watermark past
							// this event while its own delivery is still in flight.
							w.trackEmitted(seq)
							ev.acknowledge = w.cursorAcknowledger(seq)
						}
						select {
						case ch <- ev:
						case <-ctx.Done():
							return
						}
					}
				}
			}
			if err != nil {
				// /dev/kmsg returns EPIPE when the reader falls behind and the
				// kernel overwrote records that were still unread — routine under
				// a log storm, not a fatal condition. The next read resumes at
				// the oldest record still buffered, so recover in place instead
				// of tearing the watcher down and waiting up to a full retry tick
				// to reopen: a reopen on a node that has never acked (no cursor)
				// tail-seeks past every XID printed in the gap. Any other error
				// (EOF on a test file/FIFO, a real device failure) ends the
				// stream so the agent's retry loop reopens it.
				if errors.Is(err, syscall.EPIPE) {
					if w.Logger != nil {
						w.Logger.Warn("kmsg reader overran the kernel ring; records were lost", "err", err)
					}
					continue
				}
				return
			}
		}
	}()
	return ch, nil
}

// initCursorState seeds the in-memory watermark from the persisted cursor and
// initializes the pending set. It never lowers the watermark, so a reopen (which
// re-reads the same persisted value) is idempotent.
func (w *Watcher) initCursorState(resumeSeq uint64) {
	w.cursorMu.Lock()
	defer w.cursorMu.Unlock()
	if w.pending == nil {
		w.pending = make(map[uint64]struct{})
	}
	if resumeSeq > w.watermark {
		w.watermark = resumeSeq
	}
	if resumeSeq > w.maxAcked {
		w.maxAcked = resumeSeq
	}
}

// trackEmitted records that an XID at seq was handed to a consumer and now awaits
// acknowledgement. Until it is acknowledged it pins the contiguous-ack watermark
// below seq, so a restart cannot skip it.
func (w *Watcher) trackEmitted(seq uint64) {
	w.cursorMu.Lock()
	defer w.cursorMu.Unlock()
	if w.pending == nil {
		w.pending = make(map[uint64]struct{})
	}
	if seq <= w.watermark {
		return // already durably covered
	}
	w.pending[seq] = struct{}{}
}

// acknowledgeSeq marks seq acknowledged and advances the persisted cursor to the
// highest CONTIGUOUS-ack watermark: the largest sequence with no un-acked lower
// emitted event. A gap (an earlier un-acked seq) holds the watermark just below
// it, so acknowledging a LATER seq can never persist a cursor that would skip the
// gap's event on the next restart. A failed persist leaves the watermark
// unadvanced, so the ack is retryable and no loss window opens.
func (w *Watcher) acknowledgeSeq(seq uint64) error {
	w.cursorMu.Lock()
	defer w.cursorMu.Unlock()
	if w.pending == nil {
		w.pending = make(map[uint64]struct{})
	}
	if seq <= w.watermark {
		return nil // already durable
	}
	delete(w.pending, seq)
	if seq > w.maxAcked {
		w.maxAcked = seq
	}
	safe := w.maxAcked
	if len(w.pending) > 0 {
		minPending := uint64(0)
		for p := range w.pending {
			if minPending == 0 || p < minPending {
				minPending = p
			}
		}
		// The watermark must stay strictly below the lowest un-acked sequence, so
		// that sequence still replays after a restart.
		if minPending > 0 && minPending-1 < safe {
			safe = minPending - 1
		}
	}
	if safe <= w.watermark {
		return nil // a lower gap pins the watermark: no contiguous advance yet
	}
	// Persist under the lock so a concurrent ack cannot interleave a lower
	// watermark write after a higher one.
	if err := saveCursor(w.CursorPath, w.BootID, safe); err != nil {
		return err
	}
	w.watermark = safe
	return nil
}

// cursorAcknowledger returns a once-only acknowledgement closure for one event's
// sequence. A failed cursor write is intentionally retryable: replaying an
// already delivered event is safe, while advancing the cursor after a failed
// write would create an avoidable loss window.
func (w *Watcher) cursorAcknowledger(seq uint64) func() error {
	var mu sync.Mutex
	acknowledged := false
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		if acknowledged {
			return nil
		}
		if err := w.acknowledgeSeq(seq); err != nil {
			return err
		}
		acknowledged = true
		return nil
	}
}
