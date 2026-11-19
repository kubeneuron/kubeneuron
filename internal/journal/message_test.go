// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestPlainKernelMessage(t *testing.T) {
	for _, compact := range []bool{false, true} {
		const transport, message, entry = 208, 312, 432
		buf := make([]byte, 528)
		stride, base := 16, 64
		if compact {
			stride, base = 4, 72
		}
		put := binary.LittleEndian.PutUint64
		for off, field := range map[int]string{transport: "_TRANSPORT=kernel", message: "MESSAGE=test marker"} {
			buf[off] = 1
			put(buf[off+8:], uint64(base+len(field)))
			copy(buf[off+base:], field)
		}
		buf[entry] = 3
		put(buf[entry+8:], uint64(64+2*stride))
		put(buf[entry+16:], 1)
		binary.LittleEndian.PutUint32(buf[entry+64:], transport)
		binary.LittleEndian.PutUint32(buf[entry+64+stride:], message)
		h := fileHeader{start: 208, end: uint64(len(buf)), tail: entry, compact: compact}
		got, ok, err := h.plainKernelMessage(bytes.NewReader(buf), entry, transport)
		if err != nil || !ok || got != "test marker" {
			t.Fatalf("compact %t: %q %v %v", compact, got, ok, err)
		}
		_, _, err = h.plainKernelMessage(bytes.NewReader(buf), entry, 216)
		if !errors.Is(err, ErrInvalidJournal) {
			t.Fatalf("missing backlink: %v", err)
		}
	}
}
