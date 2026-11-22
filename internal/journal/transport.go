// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"fmt"
	"io"
)

// findKernelData walks at most limit object headers without reading unrelated
// payloads. A nonzero resume points to the first object still to inspect.
func (h fileHeader) findKernelData(r io.ReaderAt, start uint64, limit int) (match, resume uint64, err error) {
	const field = "_TRANSPORT=kernel"
	if h.tail == 0 {
		return 0, 0, nil
	}
	if start == 0 {
		start = h.start
	}
	for offset := start; ; {
		if limit <= 0 {
			return 0, offset, nil
		}
		limit--
		kind, flags, body, err := h.objectAt(r, offset)
		if err != nil {
			return 0, 0, err
		}
		if kind == 1 && flags == 0 {
			d, err := h.dataAt(r, offset)
			if err != nil {
				return 0, 0, err
			}
			if d.payload.Size() == int64(len(field)) {
				var raw [len(field)]byte
				if _, err := d.payload.ReadAt(raw[:], 0); err != nil {
					return 0, 0, err
				}
				if string(raw[:]) == field {
					return offset, 0, nil
				}
			}
		}
		if offset == h.tail {
			return 0, 0, nil
		}
		next := (offset + 16 + uint64(body.Size()) + 7) &^ uint64(7)
		if next > h.tail {
			return 0, 0, fmt.Errorf("%w: object alignment crosses tail", ErrInvalidJournal)
		}
		offset = next
	}
}
