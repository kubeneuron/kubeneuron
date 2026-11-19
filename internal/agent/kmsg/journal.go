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
// tail, failed, or was not delivered.
const journalIdle = 2 * time.Second

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
	// Idle overrides journalIdle; tests set it.
	Idle time.Duration

	refusalMu     sync.Mutex
	refusalLogged map[string]bool
}

// journalCursorFile keeps one cursor per directory, so switching a node
// between /var/log/journal and /run/log/journal neither applies one
// journal's cursor to the other nor loses the place in either.
type journalCursorFile struct {
	Version int                       `json:"version"`
	Cursors map[string]journal.Cursor `json:"cursors"`
}

// Watch opens the journal and streams classified kernel fault events until
// ctx ends or the reader cannot continue. Each batch's cursor is saved only
// after every event of the batch is acknowledged; an event that is not
// acknowledged (Abandon) leaves the saved cursor where it was and the batch
// is read again. With no saved cursor the read starts at the tail, so an
// old entry never opens an incident again.
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
		failures := 0
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
			delivered, ok := s.emit(ctx, ch, batch.Records)
			if !ok {
				return
			}
			if delivered {
				cursor = batch.Cursor
				if err := s.saveCursor(cursor); err != nil {
					// The events are delivered; a restart may read them
					// again, and the controller deduplicates them.
					s.warn("journal cursor not saved; events may replay after a restart", "err", err)
				}
			}
			if !delivered || !batch.More {
				if !sleepCtx(ctx, idle) {
					return
				}
			}
		}
	}()
	return ch, nil
}

// emit classifies and sends one batch, then waits until each event is
// acknowledged or abandoned. delivered is false when any was abandoned; ok
// is false when ctx ended.
func (s *JournalSource) emit(ctx context.Context, ch chan<- Event, records []journal.Record) (delivered, ok bool) {
	results := make(chan bool, len(records))
	sent := 0
	for _, rec := range records {
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
			once.Do(func() { results <- true })
			return nil
		}
		ev.abandon = func() { once.Do(func() { results <- false }) }
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
			delivered = delivered && r
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
	return file.Cursors[s.Dir], nil
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
		file = journalCursorFile{Version: journalCursorVersion}
	}
	if file.Cursors == nil {
		file.Cursors = map[string]journal.Cursor{}
	}
	file.Cursors[s.Dir] = cursor
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
