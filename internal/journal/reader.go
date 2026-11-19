// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"context"
	"errors"
)

// Limits apply to one Next call, including traversal of non-matching objects.
const (
	MaxEntriesPerTick = 256
	MaxBytesPerTick   = 8 << 20
	MaxFieldBytes     = 1 << 20
)

// ErrInvalidJournal indicates an invalid layout or unsupported on-disk feature.
// Filesystem and context errors retain their original errors.Is semantics.
var ErrInvalidJournal = errors.New("invalid or unsupported journal")

// Cursor is an opaque checkpoint. Its zero value starts at the current tail.
// Persist it as a JSON string in the agent state directory.
type Cursor string

// Record contains only the MESSAGE from a kernel entry.
type Record struct{ Message string }

// Batch carries a resumable checkpoint even when no records matched.
// More indicates another tick is needed to catch up.
type Batch struct {
	Records []Record
	Cursor  Cursor
	More    bool
}

// Reader follows system journal files under a single journal directory,
// including rotation. Calls are sequential. Each call releases its file handles.
type Reader interface {
	// Next consumes at most one tick. On success, persist Cursor only after all
	// returned records have been processed and resulting events durably spooled
	// or delivered. On error retain the input cursor. An idle tail returns an
	// empty successful batch with More false.
	Next(ctx context.Context, after Cursor) (Batch, error)
}

// OpenReader is the signature of the Open constructor and the injection point
// for integration tests. The directory is the read-only journal mount root.
type OpenReader func(directory string) (Reader, error)
