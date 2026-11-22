// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestFindKernelDataResumes(t *testing.T) {
	for _, compact := range []bool{false, true} {
		buf := make([]byte, 208)
		payload := 64
		if compact {
			payload = 72
		}
		var tail uint64
		for _, field := range []string{"_TRANSPORT=syslog", "MESSAGE=_TRANSPORT=kernel", "_TRANSPORT=kernel"} {
			for len(buf)%8 != 0 {
				buf = append(buf, 0)
			}
			tail = uint64(len(buf))
			obj := make([]byte, payload+len(field))
			obj[0] = 1
			binary.LittleEndian.PutUint64(obj[8:], uint64(len(obj)))
			copy(obj[payload:], field)
			buf = append(buf, obj...)
		}
		h := fileHeader{start: 208, tail: tail, end: uint64(len(buf)), compact: compact}
		r := bytes.NewReader(buf)
		match, resume, err := h.findKernelData(r, 0, 2)
		if err != nil || match != 0 || resume != tail {
			t.Fatalf("first pass: %d %d %v", match, resume, err)
		}
		match, resume, err = h.findKernelData(r, resume, 2)
		if err != nil || match != tail || resume != 0 {
			t.Fatalf("resume: %d %d %v", match, resume, err)
		}
	}
}
