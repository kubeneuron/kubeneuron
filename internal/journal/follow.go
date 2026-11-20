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
)

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

// Open checks that the mount contains a readable system journal.
func Open(directory string) (Reader, error) {
	files, err := systemFiles(directory)
	if err != nil {
		return nil, err
	}
	budget := MaxBytesPerTick
	f, _, err := openHeader(files[0], &budget)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
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
	Starting  bool
}

func (r directoryReader) next(ctx context.Context, after Cursor, readBytes int) (Batch, error) {
	freshTail := func() scanCursor {
		return scanCursor{Starting: true, Positions: make(map[string]filePosition)}
	}
	state := freshTail()
	if after != "" {
		var saved scanCursor
		if err := json.Unmarshal([]byte(after), &saved); err == nil && saved.Positions != nil && len(saved.FileList) == sha256.Size*2 {
			state = saved
		}
	}
	files, err := systemFiles(r.path)
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
			state = freshTail()
		}
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
		if err != nil {
			return Batch{}, err
		}
		key := hex.EncodeToString(h.fileID[:])
		p, seen := state.Positions[key]
		if state.Starting && !seen {
			p.After = h.tailSequence
		} else if h.tailSequence > p.After {
			err = h.readNew(ctx, tickReader{f, &readBytes}, &p, &remaining, &outputBytes, &batch, r.decode)
		}
		_ = f.Close()
		if errors.Is(err, errTickBudget) {
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
		if err != nil {
			return err
		}
		if e.sequence > p.After {
			msg, ok, err := h.plainKernelMessage(f, off, p.Transport, decode)
			if errors.Is(err, errUnreadableField) {
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
