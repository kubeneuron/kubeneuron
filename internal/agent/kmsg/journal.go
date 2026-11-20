// SPDX-License-Identifier: Apache-2.0

package kmsg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/journal"
)

// ErrJournalNotMounted is returned by JournalSource.Watch when the agent has
// no journal directory. It wraps os.ErrNotExist, like a missing /dev/kmsg.
var ErrJournalNotMounted = fmt.Errorf("journal not mounted: %w", os.ErrNotExist)

// journalCursorVersion guards the on-disk format of the journal cursors.
const journalCursorVersion = 1

// journalIdle is how long the source waits after a read that reached the
// tail or failed. A batch that was not delivered waits twice as long each
// time, up to journalRedeliverMax.
const (
	journalIdle         = 2 * time.Second
	journalRedeliverMax = time.Minute
)

// JournalSource reads kernel entries from a systemd journal directory and
// classifies them with the same parser as /dev/kmsg. The reader returns only
// the MESSAGE of entries with _TRANSPORT=kernel; this source never logs or
// stores a message, and the events it emits carry no Raw text, only the
// classified fields.
type JournalSource struct {
	// Dir is the read-only journal mount, empty when the agent has none.
	Dir string
	// Open opens a reader on Dir.
	Open journal.OpenReader
	// CursorPath is the durable file of resume cursors, one per journal
	// directory, beside the spool. Empty disables resume: every start reads
	// from the tail.
	CursorPath string
	// Logger records read and cursor problems, never journal content.
	Logger *slog.Logger
	// BootID is the node's current boot. A cursor saved under another boot
	// is not used: the read starts at the tail, so faults of the previous
	// boot, which the reboot may have been meant to clear, do not open
	// incidents again. Empty means unknown, and the read also starts at the
	// tail.
	BootID string
	// Idle overrides journalIdle; tests set it.
	Idle time.Duration
	// RedeliverMax overrides journalRedeliverMax; tests set it.
	RedeliverMax time.Duration
	// Unreadable, when set, is called from the reading goroutine with the
	// number of entries the reader skipped in a batch, once, when the batch
	// is done and its cursor moves past them. The kernel faults in those
	// entries are lost; nothing of their content is passed.
	Unreadable func(entries int)

	refusalMu     sync.Mutex
	refusalLogged map[string]bool
}

// journalCursorFile keeps one cursor per directory, so switching a node
// between /var/log/journal and /run/log/journal neither applies one
// journal's cursor to the other nor loses the place in either.
type journalCursorFile struct {
	Version int                           `json:"version"`
	Cursors map[string]journalCursorEntry `json:"cursors"`
}

// journalCursorEntry is a read position and the boot it was saved under.
type journalCursorEntry struct {
	Cursor journal.Cursor `json:"cursor"`
	BootID string         `json:"boot_id"`
}

// Watch opens the journal and streams classified kernel fault events until
// ctx ends or the reader cannot continue. Each batch's cursor is saved only
// after every event of the batch is acknowledged; an event that is not
// acknowledged (Abandon) leaves the saved cursor where it was and the batch
// is read again; the events of it already acknowledged are not sent again.
// With no saved cursor, or one saved under another boot, the read starts at
// the tail, so an old entry never opens an incident again.
func (s *JournalSource) Watch(ctx context.Context) (<-chan Event, error) {
	if s.Dir == "" {
		return nil, ErrJournalNotMounted
	}
	if s.Open == nil {
		return nil, errors.New("journal reader is not built into this agent")
	}
	if _, err := os.Stat(s.Dir); err != nil {
		return nil, err
	}
	reader, err := s.Open(s.Dir)
	if err != nil {
		return nil, err
	}
	cursor, err := s.loadCursor()
	if err != nil {
		s.warn("journal cursor unreadable; reading from the tail", "err", err)
		cursor = ""
	}
	ch := make(chan Event)
	go func() {
		defer close(ch)
		idle := s.Idle
		if idle <= 0 {
			idle = journalIdle
		}
		redeliverMax := s.RedeliverMax
		if redeliverMax <= 0 {
			redeliverMax = journalRedeliverMax
		}
		failures := 0
		// acked holds the indexes of the records, in the batch read from
		// cursor, whose events were acknowledged. A batch read again from
		// the same cursor starts with the same records.
		acked := map[int]bool{}
		wait := idle
		for {
			batch, err := reader.Next(ctx, cursor)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				failures++
				if errors.Is(err, journal.ErrInvalidJournal) || failures >= 3 {
					// The channel closing is the signal: the caller marks the
					// source unavailable and reopens it later.
					s.warn("journal read failed", "err", err)
					return
				}
				if !sleepCtx(ctx, idle) {
					return
				}
				continue
			}
			failures = 0
			delivered, ok := s.emit(ctx, ch, batch.Records, acked)
			if !ok {
				return
			}
			if !delivered {
				// Read the batch again later, more slowly each time, so an
				// event the agent cannot take does not spin the source.
				if !sleepCtx(ctx, wait) {
					return
				}
				wait = min(2*wait, redeliverMax)
				continue
			}
			wait = idle
			clear(acked)
			if batch.UnreadableEntries > 0 && s.Unreadable != nil {
				s.Unreadable(batch.UnreadableEntries)
			}
			cursor = batch.Cursor
			if err := s.saveCursor(cursor); err != nil {
				// The events are delivered; a restart may read them
				// again, and the controller deduplicates them.
				s.warn("journal cursor not saved; events may replay after a restart", "err", err)
			}
			if !batch.More {
				if !sleepCtx(ctx, idle) {
					return
				}
			}
		}
	}()
	return ch, nil
}

// emit classifies and sends one batch, skipping the records in acked, then
// waits until each event is acknowledged or abandoned and adds the
// acknowledged ones to acked. delivered is false when any was abandoned; ok
// is false when ctx ended.
func (s *JournalSource) emit(ctx context.Context, ch chan<- Event, records []journal.Record, acked map[int]bool) (delivered, ok bool) {
	type result struct {
		index int
		ok    bool
	}
	results := make(chan result, len(records))
	sent := 0
	for i, rec := range records {
		if acked[i] {
			continue
		}
		ev, matched, refusal := parseLine(rec.Message)
		if refusal != "" {
			s.logRefusal(refusal)
		}
		if !matched {
			continue
		}
		ev.Raw = ""
		var once sync.Once
		ev.acknowledge = func() error {
			once.Do(func() { results <- result{i, true} })
			return nil
		}
		ev.abandon = func() { once.Do(func() { results <- result{i, false} }) }
		select {
		case ch <- ev:
			sent++
		case <-ctx.Done():
			return false, false
		}
	}
	delivered = true
	for i := 0; i < sent; i++ {
		select {
		case r := <-results:
			if r.ok {
				acked[r.index] = true
			}
			delivered = delivered && r.ok
		case <-ctx.Done():
			return false, false
		}
	}
	return delivered, true
}

// logRefusal reports a declined classification once per reason, without
// the line.
func (s *JournalSource) logRefusal(reason string) {
	s.refusalMu.Lock()
	if s.refusalLogged == nil {
		s.refusalLogged = map[string]bool{}
	}
	already := s.refusalLogged[reason]
	s.refusalLogged[reason] = true
	s.refusalMu.Unlock()
	if !already {
		s.warn("kernel fault entry not classified", "reason", reason)
	}
}

func (s *JournalSource) warn(msg string, args ...any) {
	if s.Logger != nil {
		s.Logger.Warn(msg, append([]any{"dir", s.Dir}, args...)...)
	}
}

func (s *JournalSource) loadCursor() (journal.Cursor, error) {
	if s.CursorPath == "" {
		return "", nil
	}
	file, err := s.readCursors()
	if err != nil {
		return "", err
	}
	entry := file.Cursors[s.Dir]
	if s.BootID == "" || entry.BootID != s.BootID {
		return "", nil
	}
	return entry.Cursor, nil
}

func (s *JournalSource) readCursors() (journalCursorFile, error) {
	data, err := os.ReadFile(s.CursorPath)
	if errors.Is(err, os.ErrNotExist) {
		return journalCursorFile{Version: journalCursorVersion}, nil
	}
	if err != nil {
		return journalCursorFile{}, err
	}
	var file journalCursorFile
	if err := json.Unmarshal(data, &file); err != nil {
		return journalCursorFile{}, err
	}
	if file.Version != journalCursorVersion {
		return journalCursorFile{}, fmt.Errorf("unsupported journal cursor version %d", file.Version)
	}
	return file, nil
}

func (s *JournalSource) saveCursor(cursor journal.Cursor) error {
	if s.CursorPath == "" {
		return nil
	}
	file, err := s.readCursors()
	if err != nil {
		s.warn("journal cursor file unreadable; replacing it, the cursors of other directories are lost", "err", err)
		file = journalCursorFile{Version: journalCursorVersion}
	}
	if file.Cursors == nil {
		file.Cursors = map[string]journalCursorEntry{}
	}
	file.Cursors[s.Dir] = journalCursorEntry{Cursor: cursor, BootID: s.BootID}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	return writeDurably(s.CursorPath, ".journal-cursor-*", data)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
