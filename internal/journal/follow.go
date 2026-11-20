// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var errEntryBudget = fmt.Errorf("journal entry exceeds read budget: %w", errTickBudget)

var errTickBudget = errors.New("journal tick budget exhausted")

type tickReader struct {
	source    io.ReaderAt
	remaining *int
}

func (r tickReader) ReadAt(p []byte, offset int64) (int, error) {
	if len(p) > *r.remaining {
		return 0, errTickBudget
	}
	n, err := r.source.ReadAt(p, offset)
	*r.remaining -= n
	return n, err
}

type directoryReader struct {
	path   string
	decode fieldDecoder
}
type filePosition struct {
	After, Search, Transport, Consumed, Array, Slot uint64
	Seen                                            bool `json:",omitempty"`
}

func openHeader(path string, remaining *int) (*os.File, fileHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fileHeader{}, err
	}
	info, err := f.Stat()
	if err == nil {
		var h fileHeader
		h, err = readHeader(tickReader{f, remaining}, info.Size())
		if err == nil {
			return f, h, nil
		}
	}
	_ = f.Close()
	return nil, fileHeader{}, err
}

func archivePath(path string) bool { return filepath.Base(path) != "system.journal" }

// A malformed archived header may have no usable file ID. Track its path
// in the same bounded position map across polls.
func headerPosition(path string, h fileHeader, err error) (string, bool) {
	if err != nil && archivePath(path) && errors.Is(err, ErrInvalidJournal) {
		digest := sha256.Sum256([]byte(path))
		return hex.EncodeToString(digest[:16]), true
	}
	return hex.EncodeToString(h.fileID[:]), false
}

// Open checks that the mount contains a readable system journal.
func Open(directory string) (Reader, error) {
	files, _, err := selectedFiles(directory)
	if err != nil {
		return nil, err
	}
	budget := MaxBytesPerTick
	for _, path := range files {
		f, h, err := openHeader(path, &budget)
		if _, skip := headerPosition(path, h, err); skip {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
		break
	}
	return directoryReader{directory, new(fieldDecoders).decodeField}, nil
}

func (r directoryReader) Next(ctx context.Context, after Cursor) (Batch, error) {
	return r.next(ctx, after, MaxBytesPerTick)
}

type scanCursor struct {
	FileList  string `json:",omitempty"`
	Positions map[string]filePosition
	Resume    string
	Overflow  string `json:",omitempty"`
	Starting  bool
}

func (r directoryReader) next(ctx context.Context, after Cursor, readBytes int) (Batch, error) {
	freshTail := func() scanCursor {
		return scanCursor{Starting: true, Positions: make(map[string]filePosition)}
	}
	state := freshTail()
	if after != "" && len(after) <= 64<<10 {
		var saved scanCursor
		if err := json.Unmarshal([]byte(after), &saved); err == nil && saved.Positions != nil && len(saved.Positions) <= maxJournalFiles && len(saved.FileList) == sha256.Size*2 {
			valid := true
			for id := range saved.Positions {
				if len(id) != 32 {
					valid = false
					break
				}
			}
			if valid {
				state = saved
			}
		}
	}
	files, omitted, err := selectedFiles(r.path)
	if err != nil {
		return Batch{}, err
	}
	var batch Batch

	remaining := MaxEntriesPerTick
	outputBytes := MaxBytesPerTick
	// Rotation can insert an archive before the saved path. Rescan in that
	// case, using per-file positions to suppress already delivered entries.
	hash := sha256.New()
	for _, path := range files {
		io.WriteString(hash, path)
		hash.Write([]byte{0})
	}
	listID := hex.EncodeToString(hash.Sum(nil))
	// A full cursor may contain expired IDs after rotation. Reconcile its
	// keys before inserting new positions so partial checkpoints stay bounded.
	if len(state.Positions)+len(files) > maxJournalFiles {
		live := make(map[string]bool, len(files))
		for _, path := range files {
			f, h, err := openHeader(path, &readBytes)
			key, skip := headerPosition(path, h, err)
			if !skip {
				if err != nil {
					return Batch{}, err
				}
				f.Close()
			}
			live[key] = true
		}
		for id := range state.Positions {
			if !live[id] {
				delete(state.Positions, id)
			}
		}
	}
	if state.Resume == "" || state.FileList != listID {
		for id, p := range state.Positions {
			p.Seen = false
			state.Positions[id] = p
		}
	}
	if state.Resume != "" {
		found := false
		for i, path := range files {
			if path == state.Resume {
				found = true
				if state.FileList == listID {
					files = files[i:]
				}
				break
			}
		}
		if !found {
			// Vacuum can remove the resume file while other files still have unread entries.
			batch.UnreadableEntries++
		}
	}
	if after != "" && state.FileList == "" {
		batch.UnreadableEntries++
	}
	if omitted {
		if state.Overflow != listID {
			batch.UnreadableEntries++
		}
		state.Overflow = listID
	} else {
		state.Overflow = ""
	}
	retryPath := state.Resume
	if state.FileList != listID {
		retryPath = ""
	}
	state.FileList = listID
	state.Resume = ""
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return Batch{}, err
		}
		f, h, err := openHeader(path, &readBytes)
		if errors.Is(err, errTickBudget) {
			state.Resume = path
			batch.More = true
			break
		}
		key, skipHeader := headerPosition(path, h, err)
		if err != nil && !skipHeader {
			return Batch{}, err
		}
		p, seen := state.Positions[key]
		if !seen && len(state.Positions) >= maxJournalFiles {
			if f != nil {
				f.Close()
			}
			return Batch{}, errors.New("journal files changed during scan")
		}
		p.Seen = true
		if skipHeader {
			if !seen {
				batch.UnreadableEntries++
			}
			state.Positions[key] = p
			continue
		}
		before := p
		if state.Starting && !seen {
			p.After = h.tailSequence
		} else if h.tailSequence > p.After {
			err = h.readNew(ctx, tickReader{f, &readBytes}, &p, &remaining, &outputBytes, &batch, r.decode)
		}
		_ = f.Close()
		if archivePath(path) && (!h.online || strings.HasSuffix(path, ".journal~")) && errors.Is(err, ErrInvalidJournal) {
			p.After = h.tailSequence
			batch.UnreadableEntries++
			err = nil
		}
		if errors.Is(err, errTickBudget) {
			// The saved entry was the first work on a fresh tick and still
			// exhausted its budget. Advance past it so the next entry can run.
			if retryPath == path && p == before && errors.Is(err, errEntryBudget) {
				if p.Consumed > 0 {
					p.Slot++
				}
				p.Consumed++
				batch.UnreadableEntries++
			}
			state.Positions[key] = p
			state.Resume = path
			batch.More = true
			break
		}
		if err != nil {
			return Batch{}, err
		}
		state.Positions[key] = p
	}
	if !batch.More {
		state.Starting = false
		for id, p := range state.Positions {
			if !p.Seen {
				delete(state.Positions, id)
			}
		}
	}
	encoded, err := json.Marshal(state)
	batch.Cursor = Cursor(encoded)
	return batch, err
}

func (h fileHeader) readNew(ctx context.Context, f io.ReaderAt, p *filePosition, remaining, outputBytes *int, batch *Batch, decode fieldDecoder) error {
	if p.Transport == 0 {
		match, resume, err := h.findKernelData(f, p.Search, 256)
		if err != nil {
			return err
		}
		p.Search = resume
		if match == 0 {
			if resume != 0 {
				batch.More = true
			} else {
				p.Search = h.tail
				p.After = h.tailSequence
			}
			return nil
		}
		p.Transport = match
	}
	d, err := h.dataAt(f, p.Transport)
	if err != nil {
		return err
	}
	for p.Consumed < d.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if *remaining == 0 {
			batch.More = true
			return nil
		}
		off := d.firstEntry
		if p.Consumed > 0 {
			if p.Array == 0 {
				p.Array = d.entryArray
			}
			a, err := h.arrayAt(f, p.Array)
			if err != nil {
				return err
			}
			if p.Slot == uint64(a.items.Size()/a.width) {
				if a.next == 0 {
					return fmt.Errorf("%w: missing ENTRY_ARRAY link", ErrInvalidJournal)
				}
				p.Array = a.next
				p.Slot = 0
				continue
			}
			off, err = a.entryOffset(int64(p.Slot))
			if err != nil {
				return err
			}
		}
		e, err := h.entryAt(f, off)
		if errors.Is(err, ErrInvalidJournal) {
			batch.UnreadableEntries++
		} else if err != nil {
			return err
		} else if e.sequence > p.After {
			msg, ok, err := h.plainKernelMessage(f, off, p.Transport, decode)
			if errors.Is(err, errTickBudget) {
				return errEntryBudget
			}
			if errors.Is(err, errUnreadableField) || errors.Is(err, ErrInvalidJournal) {
				batch.UnreadableEntries++
			} else if err != nil {
				return err
			}
			if ok {
				if len(msg) > *outputBytes {
					return errTickBudget
				}
				*outputBytes -= len(msg)
				batch.Records = append(batch.Records, Record{Message: msg})
			}
			p.After = e.sequence
		}
		if p.Consumed > 0 {
			p.Slot++
		}
		p.Consumed++
		*remaining--
	}
	p.After = h.tailSequence
	return nil
}
