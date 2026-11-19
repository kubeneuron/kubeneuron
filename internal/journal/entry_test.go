// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestEntryReferences(t *testing.T) {
	for _, stride := range []int{4, 16} {
		const offset = 208
		buf := make([]byte, offset+64+2*stride)
		obj := buf[offset:]
		obj[0] = 3
		put := binary.LittleEndian.PutUint64
		put(obj[8:], uint64(len(obj)))
		put(obj[16:], 17)
		binary.LittleEndian.PutUint32(obj[64:], 224)
		binary.LittleEndian.PutUint32(obj[64+stride:], 320)
		if stride == 16 {
			put(obj[72:], 0xdeadbeef)
		}
		h := fileHeader{start: offset, tail: offset, end: uint64(len(buf)), compact: stride == 4}
		e, err := h.entryAt(bytes.NewReader(buf), offset)
		if err != nil || e.sequence != 17 {
			t.Fatalf("stride %d: seq %d, err %v", stride, e.sequence, err)
		}
		for i, want := range []uint64{224, 320} {
			got, err := e.dataOffset(int64(i))
			if err != nil || got != want {
				t.Fatalf("stride %d item %d: got %d, err %v", stride, i, got, err)
			}
		}
		if _, err := e.dataOffset(2); err != io.EOF {
			t.Fatalf("end: %v", err)
		}
		put(obj[8:], uint64(len(obj)-1))
		if _, err := h.entryAt(bytes.NewReader(buf), offset); err == nil {
			t.Fatal("accepted partial item")
		}
	}
}
