// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"encoding/binary"
	"fmt"
	"io"
)

type entryObject struct {
	sequence uint64
	items    *io.SectionReader
	stride   int64
}

func (h fileHeader) entryAt(r io.ReaderAt, offset uint64) (entryObject, error) {
	kind, flags, body, err := h.objectAt(r, offset)
	if err != nil {
		return entryObject{}, err
	}
	const fixed = 48
	stride := int64(16)
	if h.compact {
		stride = 4
	}
	if kind != 3 || flags != 0 || body.Size() <= fixed || (body.Size()-fixed)%stride != 0 {
		return entryObject{}, fmt.Errorf("%w: invalid journal ENTRY layout", ErrInvalidJournal)
	}
	var sequence [8]byte
	if _, err := body.ReadAt(sequence[:], 0); err != nil {
		return entryObject{}, err
	}
	seq := binary.LittleEndian.Uint64(sequence[:])
	if seq == 0 {
		return entryObject{}, fmt.Errorf("%w: invalid zero journal sequence", ErrInvalidJournal)
	}
	return entryObject{seq, io.NewSectionReader(body, fixed, body.Size()-fixed), stride}, nil
}

// dataOffset reads only the reference. Regular items also carry a hash.
func (e entryObject) dataOffset(index int64) (uint64, error) {
	if index < 0 || index >= e.items.Size()/e.stride {
		return 0, io.EOF
	}
	var raw [8]byte
	width := min(e.stride, int64(len(raw)))
	if _, err := e.items.ReadAt(raw[:width], index*e.stride); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(raw[:]), nil
}
