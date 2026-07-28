// Package spool is a bounded, file-backed retry queue for agent events. When
// the controller is unreachable, events are appended (fsynced) and later
// replayed in FIFO batches. Replay is at-least-once; the controller
// deduplicates on the event's capture-time EventID.
package spool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
}

// Open creates or opens a spool file, creating its directory if needed.
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
	events, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	s.count = len(events)
	return s, nil
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
		return err
	}
	if err := f.Sync(); err != nil {
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
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event types.AgentEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue // skip corrupt lines rather than wedging the spool
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
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
