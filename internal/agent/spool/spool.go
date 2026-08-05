// Package spool is a bounded, file-backed retry queue for agent events. When
// the controller is unreachable, events are appended (fsynced) and later
// replayed in FIFO batches. Replay is at-least-once; the controller
// deduplicates on the event's capture-time EventID.
package spool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// maxEvents bounds the spool; oldest events are dropped past this (the
// metrics path still carries their counters).
const maxEvents = 10000

// Spool is a file-backed FIFO of agent events.
type Spool struct {
	mu   sync.Mutex
	path string
	// count tracks the parseable events on disk so Append does not have to
	// re-read the file to enforce the bound.
	count int
	// dirty marks that an in-process Append failed after a partial write, so the
	// tail may lack its committing newline. The next Append/ReplayBatch repairs
	// it before touching the file. Open handles the cross-process crash case;
	// this handles a write/sync failure while the same process keeps running —
	// the exact ENOSPC/EIO regime the spool exists to survive.
	dirty bool
	// Logger, when set, records spool recovery events: a repaired torn tail and
	// any corrupt lines skipped during replay. Recovery still proceeds when nil.
	Logger *slog.Logger
}

// Open creates or opens a spool file, creating its directory if needed. A torn
// final line left by a crash mid-Append is repaired before the file is read,
// so the next Append cannot glue its JSON onto a partial record.
func Open(path string) (*Spool, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	s := &Spool{path: path}
	if err := s.repairTornTailLocked(); err != nil {
		return nil, err
	}
	events, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	s.count = len(events)
	return s, nil
}

// repairTornTailLocked truncates a partial final line left by a power loss or
// crash mid-Append. Append's durability contract is that it fsyncs the record
// AND its trailing newline in one write before returning success, so a file
// that does not end in '\n' has an uncommitted tail that was never reported
// durable. Left in place, the next Append would O_APPEND its JSON onto the
// partial line, producing one unparseable record that replay silently drops —
// losing both the torn event and the fresh one. Mirrors the action journal's
// torn-tail handling. Called before the file is read; safe on an empty file.
func (s *Spool) repairTornTailLocked() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return nil
	}
	// Keep everything through the last committed newline; drop the rest. When
	// there is no newline at all the whole file is one uncommitted record.
	keep := int64(bytes.LastIndexByte(data, '\n') + 1)
	f, err := os.OpenFile(s.path, os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(keep); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if s.Logger != nil {
		s.Logger.Warn("repaired torn spool tail from an interrupted append",
			"path", s.path, "dropped_bytes", int64(len(data))-keep)
	}
	return nil
}

// healTornTailLocked repairs a torn tail left by an in-process Append that
// failed after a partial write, then reconciles count with what is actually
// durable on disk. A no-op unless a prior Append/ReplayBatch marked the spool
// dirty, so the happy path pays no extra I/O. Called with mu held.
func (s *Spool) healTornTailLocked() error {
	if !s.dirty {
		return nil
	}
	if err := s.repairTornTailLocked(); err != nil {
		return err
	}
	// A failed Append never incremented count, but recompute it from disk so any
	// drift (e.g. a sync that failed after the record was durable) is corrected
	// to the truncated, parseable state rather than trusted blindly.
	events, err := s.readLocked()
	if err != nil {
		return err
	}
	s.count = len(events)
	s.dirty = false
	return nil
}

// Len reports how many events are queued.
func (s *Spool) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// Append adds an event to the spool and fsyncs it: an event accepted here
// survives an agent crash or node reboot.
func (s *Spool) Append(ev types.AgentEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.healTornTailLocked(); err != nil {
		return err
	}

	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		// A partial write leaves the tail without its committing newline. Mark
		// the spool dirty so the next Append repairs it instead of gluing its
		// JSON onto the fragment (which would make one unparseable record that
		// replay drops, silently losing this event AND the next one).
		s.dirty = true
		return err
	}
	if err := f.Sync(); err != nil {
		// The write may or may not have reached disk intact; repair before the
		// next append and recompute count from what is actually durable.
		s.dirty = true
		return err
	}
	s.count++
	if s.count <= maxEvents {
		return nil
	}
	return s.truncateLocked()
}

// ReplayBatch sends at most limit events from the head of the spool. Events
// remain on disk until send succeeds. If sending fails or ctx is canceled,
// the failed event and every event after it remain in their original FIFO
// order. Successfully sent events are removed with one atomic file replace.
//
// The spool lock intentionally spans send calls: Append must not insert events
// between the batch snapshot and its commit.
func (s *Spool) ReplayBatch(
	ctx context.Context,
	limit int,
	send func(context.Context, types.AgentEvent) error,
) (int, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("replay batch limit must be positive")
	}
	if send == nil {
		return 0, fmt.Errorf("replay send function must not be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.healTornTailLocked(); err != nil {
		return 0, err
	}

	events, err := s.readLocked()
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}
	if limit > len(events) {
		limit = len(events)
	}

	sent := 0
	var replayErr error
	for sent < limit {
		if err := ctx.Err(); err != nil {
			replayErr = err
			break
		}
		if err := send(ctx, events[sent]); err != nil {
			replayErr = err
			break
		}
		sent++
	}
	if sent == 0 {
		return 0, replayErr
	}
	if err := s.rewriteLocked(events[sent:]); err != nil {
		return sent, fmt.Errorf("commit replayed spool events: %w", err)
	}
	s.count = len(events) - sent
	return sent, replayErr
}

func (s *Spool) readLocked() ([]types.AgentEvent, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var events []types.AgentEvent
	skipped := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event types.AgentEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			// Skip corrupt lines rather than wedging the spool, but never
			// silently: a corrupt line means an event was lost, which the
			// operator must be able to see.
			skipped++
			continue
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if skipped > 0 && s.Logger != nil {
		s.Logger.Error("skipped corrupt spool lines during replay; events were lost",
			"path", s.path, "skipped", skipped)
	}
	return events, nil
}

func (s *Spool) rewriteLocked(events []types.AgentEvent) (retErr error) {
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) && retErr == nil {
			retErr = err
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}

	writer := bufio.NewWriter(tmp)
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			_ = tmp.Close()
			return err
		}
		if _, err := writer.Write(append(line, '\n')); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	// Sync the directory so the rename itself survives a crash; without it
	// the atomic replace is not durable and recovery can see the old file.
	dirFile, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	if err := dirFile.Sync(); err != nil {
		_ = dirFile.Close()
		return err
	}
	return dirFile.Close()
}

// truncateLocked drops oldest events past maxEvents through the same
// fsynced atomic-replace path as replay commits. Called with mu held.
func (s *Spool) truncateLocked() error {
	events, err := s.readLocked()
	if err != nil {
		return err
	}
	if len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	if err := s.rewriteLocked(events); err != nil {
		return err
	}
	s.count = len(events)
	return nil
}
