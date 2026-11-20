// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"fmt"
	"io"
)

// plainKernelMessage checks both ends of the DATA-to-ENTRY link before
// returning a message. transport is the previously matched kernel DATA offset.
func (h fileHeader) plainKernelMessage(r io.ReaderAt, entry, transport uint64, decode fieldDecoder) (string, bool, error) {
	e, err := h.entryAt(r, entry)
	if err != nil {
		return "", false, err
	}
	var matched bool
	for i := int64(0); i < e.items.Size()/e.stride; i++ {
		off, err := e.dataOffset(i)
		if err != nil {
			return "", false, err
		}
		if off == transport {
			matched = true
			break
		}
	}
	if !matched {
		return "", false, fmt.Errorf("%w: ENTRY missing matched transport", ErrInvalidJournal)
	}
	var message string
	var found bool
	for i := int64(0); i < e.items.Size()/e.stride; i++ {
		off, err := e.dataOffset(i)
		if err != nil {
			return "", false, err
		}
		if off == transport {
			continue
		}
		d, err := h.dataAt(r, off)
		if err != nil {
			return "", false, err
		}
		if d.flags != 0 {
			field, err := guardedDecode(decode, d.flags, d.payload)
			if err != nil {
				return "", false, err
			}
			d.payload = io.NewSectionReader(bytes.NewReader(field), 0, int64(len(field)))
		}
		const prefix = "MESSAGE="
		if d.payload.Size() < int64(len(prefix)) {
			continue
		}
		var name [len(prefix)]byte
		if _, err := d.payload.ReadAt(name[:], 0); err != nil {
			return "", false, err
		}
		if !bytes.Equal(name[:], []byte(prefix)) {
			continue
		}
		if found || d.payload.Size() > MaxFieldBytes {
			return "", false, fmt.Errorf("%w: duplicate or oversized MESSAGE", ErrInvalidJournal)
		}
		value := make([]byte, d.payload.Size()-int64(len(prefix)))
		if len(value) > 0 {
			if _, err := d.payload.ReadAt(value, int64(len(prefix))); err != nil {
				return "", false, err
			}
		}
		message = string(value)
		found = true
	}
	return message, found, nil
}

type fieldDecoder func(byte, *io.SectionReader) ([]byte, error)

var errDecoderPanic = fmt.Errorf("%w: field decoder panicked", ErrInvalidJournal)

func guardedDecode(decode fieldDecoder, flags byte, payload *io.SectionReader) (field []byte, err error) {
	defer func() {
		if recover() != nil {
			field, err = nil, errDecoderPanic
		}
	}()
	return decode(flags, payload)
}

func decodeField(flags byte, payload *io.SectionReader) ([]byte, error) {
	switch flags {
	case 1:
		return readXZ(payload)
	case 2:
		return readLZ4(payload)
	case 4:
		return readZstd(payload)
	default:
		return nil, fmt.Errorf("%w: unsupported DATA compression", ErrInvalidJournal)
	}
}
