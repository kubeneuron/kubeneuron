// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"encoding/binary"
	"errors"
	"io"
)

// objectAt returns a reader bounded to the declared object body. The arena can
// include unused space after the tail, even in a complete file.
func (h fileHeader) objectAt(r io.ReaderAt, offset uint64) (kind, flags byte, body *io.SectionReader, err error) {
	var raw [16]byte
	if offset < h.start || offset%8 != 0 || offset > h.tail || offset > h.end || h.end-offset < 16 {
		return 0, 0, nil, errors.New("invalid journal object offset")
	}
	if _, err := r.ReadAt(raw[:], int64(offset)); err != nil {
		return 0, 0, nil, err
	}
	size := binary.LittleEndian.Uint64(raw[8:])
	limit := h.end
	if offset < h.tail {
		limit = h.tail
	}
	if size < 16 || size > limit-offset {
		return 0, 0, nil, errors.New("invalid journal object size")
	}
	return raw[0], raw[1], io.NewSectionReader(r, int64(offset+16), int64(size-16)), nil
}
