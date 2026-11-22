// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func FuzzObjectAt(f *testing.F) {
	for _, pair := range [][2]uint64{{208, 19}, {208, 15}, {208, 33}, {240, 16}, {248, 16}, {209, 16}, {240, ^uint64(0)}} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, off, size uint64) {
		var data [272]byte
		h := fileHeader{start: 208, end: 272, tail: 240}
		if off <= 256 {
			data[off] = 1
			binary.LittleEndian.PutUint64(data[off+8:], size)
		}
		kind, _, body, err := h.objectAt(bytes.NewReader(data[:]), off)
		valid := off >= 208 && off <= 240 && off%8 == 0 && size >= 16 && size <= 272-off && (off == 240 || size <= 240-off)
		if valid != (err == nil) {
			t.Fatalf("offset %d size %d: %v", off, size, err)
		}
		if err != nil {
			return
		}
		b, err := io.ReadAll(body)
		if err != nil || kind != 1 || uint64(len(b)) != size-16 {
			t.Fatalf("kind %d body %d: %v", kind, len(b), err)
		}
	})
}
