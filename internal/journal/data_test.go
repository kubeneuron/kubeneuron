// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestDataLayouts(t *testing.T) {
	for _, layout := range []struct {
		name  string
		flag  byte
		start int
	}{{"regular", 0, 64}, {"compact", 16, 72}} {
		t.Run(layout.name, func(t *testing.T) {
			const field = "_TRANSPORT=kernel"
			const offset = 208
			size := layout.start + len(field)
			buf := make([]byte, offset+size)
			copy(buf, "LPKSHHRH")
			buf[12] = layout.flag
			put := binary.LittleEndian.PutUint64
			put(buf[88:], offset)
			put(buf[96:], uint64(size))
			put(buf[136:], offset)
			obj := buf[offset:]
			obj[0] = 1
			put(obj[8:], uint64(size))
			put(obj[40:], 512)
			put(obj[48:], 640)
			put(obj[56:], 3)
			copy(obj[layout.start:], field)
			r := bytes.NewReader(buf)
			h, err := readHeader(r, int64(len(buf)))
			if err != nil {
				t.Fatal(err)
			}
			data, err := h.dataAt(r, offset)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := io.ReadAll(data.payload)
			if err != nil || string(payload) != field || data.firstEntry != 512 || data.entryArray != 640 || data.entries != 3 {
				t.Fatalf("unexpected DATA metadata or payload: %+v %q %v", data, payload, err)
			}
		})
	}
}
