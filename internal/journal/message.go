// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"fmt"
	"io"
)

// plainKernelMessage checks both ends of the DATA-to-ENTRY link before
// returning a message. transport is the previously matched kernel DATA offset.
func (h fileHeader) plainKernelMessage(r io.ReaderAt, entry, transport uint64) (string, bool, error) {
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
		if d.flags == 2 {
			field, err := readLZ4(d.payload)
			if err != nil {
				return "", false, err
			}
			d.payload = io.NewSectionReader(bytes.NewReader(field), 0, int64(len(field)))
		} else if d.flags != 0 {
			return "", false, fmt.Errorf("%w: compressed DATA requires decoder", ErrInvalidJournal)
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
