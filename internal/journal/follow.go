// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"context"
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

func openHeader(path string) (*os.File, fileHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fileHeader{}, err
	}
	info, err := f.Stat()
	if err == nil {
		var h fileHeader
		h, err = readHeader(f, info.Size())
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
	f, _, err := openHeader(files[0])
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	return directoryReader{directory, decodeField}, nil
}

func (r directoryReader) Next(ctx context.Context, after Cursor) (Batch, error) {
	positions := map[string]filePosition{}
	if after != "" {
		if err := json.Unmarshal([]byte(after), &positions); err != nil || positions == nil {
			return Batch{}, fmt.Errorf("%w: invalid cursor", ErrInvalidJournal)
		}
	}
	files, err := systemFiles(r.path)
	if err != nil {
		return Batch{}, err
	}
	var batch Batch
	remaining := MaxEntriesPerTick
	readBytes, outputBytes := MaxBytesPerTick, MaxBytesPerTick
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return Batch{}, err
		}
		f, h, err := openHeader(path)
		if err != nil {
			return Batch{}, err
		}
		key := hex.EncodeToString(h.fileID[:])
		p := positions[key]
		if after == "" {
			p.After = h.tailSequence
		} else if h.tailSequence > p.After {
			err = h.readNew(ctx, tickReader{f, &readBytes}, &p, &remaining, &outputBytes, &batch, r.decode)
		}
		_ = f.Close()
		if errors.Is(err, errTickBudget) {
			positions[key] = p
			batch.More = true
			break
		}
		if err != nil {
			return Batch{}, err
		}
		positions[key] = p
	}
	encoded, err := json.Marshal(positions)
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
			if errors.Is(err, errDecoderPanic) {
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
