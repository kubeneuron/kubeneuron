// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"encoding/binary"
	"fmt"
	"io"
)

type entryArray struct {
	next  uint64
	items *io.SectionReader
	width int64
}

// arrayAt validates the link before a caller follows it. Array objects are
// appended to the journal, so a link to this or an earlier object is invalid.
func (h fileHeader) arrayAt(r io.ReaderAt, offset uint64) (entryArray, error) {
	kind, flags, body, err := h.objectAt(r, offset)
	if err != nil {
		return entryArray{}, err
	}
	width := int64(8)
	if h.compact {
		width = 4
	}
	if kind != 6 || flags != 0 || body.Size() <= 8 || (body.Size()-8)%width != 0 {
		return entryArray{}, fmt.Errorf("%w: invalid journal ENTRY_ARRAY layout", ErrInvalidJournal)
	}
	var raw [8]byte
	if _, err := body.ReadAt(raw[:], 0); err != nil {
		return entryArray{}, err
	}
	next := binary.LittleEndian.Uint64(raw[:])
	if next != 0 && (next < offset+16+uint64(body.Size()) || next%8 != 0 || next > h.tail) {
		return entryArray{}, fmt.Errorf("%w: invalid journal ENTRY_ARRAY link", ErrInvalidJournal)
	}
	return entryArray{next: next, items: io.NewSectionReader(body, 8, body.Size()-8), width: width}, nil
}

func (a entryArray) entryOffset(index int64) (uint64, error) {
	if index < 0 || index >= a.items.Size()/a.width {
		return 0, io.EOF
	}
	var raw [8]byte
	if _, err := a.items.ReadAt(raw[:a.width], index*a.width); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(raw[:]), nil
}
