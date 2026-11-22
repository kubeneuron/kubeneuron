// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func FuzzEntryArrayLink(f *testing.F) {
	for _, compact := range []bool{false, true} {
		for _, next := range []uint64{0, 208, 224, 240, 248, 249, 272, 280} {
			f.Add(compact, next)
		}
	}
	f.Fuzz(func(t *testing.T, compact bool, next uint64) {
		const start = 208
		var buf [312]byte
		width := 8
		if compact {
			width = 4
		}
		size := 24 + 2*width
		for _, off := range []int{start, 272} {
			obj := buf[off:]
			obj[0] = 6
			binary.LittleEndian.PutUint64(obj[8:], uint64(size))
			binary.LittleEndian.PutUint32(obj[24+width:], 192)
		}
		binary.LittleEndian.PutUint64(buf[start+16:], next)
		r := bytes.NewReader(buf[:])
		h := fileHeader{start: start, end: 312, tail: 272, compact: compact}
		a, err := h.arrayAt(r, start)
		valid := next == 0 || next >= start+uint64(size) && next%8 == 0 && next <= 272
		if (err == nil) != valid {
			t.Fatalf("link %d compact %t: %v", next, compact, err)
		}
		if err != nil {
			return
		}
		if a.next == 272 {
			a, err = h.arrayAt(r, a.next)
			if err != nil || a.next != 0 {
				t.Fatalf("tail: %+v %v", a, err)
			}
		}
		off, err := a.entryOffset(1)
		if err != nil || off != 192 {
			t.Fatalf("entry: %d %v", off, err)
		}
	})
}
