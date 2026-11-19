// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestReadHeader(t *testing.T) {
	var data [224]byte
	copy(data[:], "LPKSHHRH")
	binary.LittleEndian.PutUint64(data[88:], 208)
	binary.LittleEndian.PutUint64(data[96:], 16)
	binary.LittleEndian.PutUint64(data[136:], 208)
	if h, err := readHeader(bytes.NewReader(data[:]), 224); h.tail != 208 || err != nil {
		t.Fatalf("tail %d, error %v", h.tail, err)
	}
	for _, index := range []int{0, 12, 16, 88, 96, 136} {
		bad := data
		bad[index] = 255
		if _, err := readHeader(bytes.NewReader(bad[:]), 224); err == nil {
			t.Fatalf("accepted corrupt header at byte %d", index)
		}
	}
}

func FuzzReadHeader(f *testing.F) {
	header := make([]byte, 208)
	copy(header, "LPKSHHRH")
	binary.LittleEndian.PutUint64(header[88:], uint64(len(header)))
	f.Add(header, int64(len(header)))
	f.Add([]byte("LPKSHHRH"), int64(8))
	f.Fuzz(func(t *testing.T, data []byte, fileSize int64) {
		h, err := readHeader(bytes.NewReader(data), fileSize)
		if err != nil {
			return
		}
		if len(data) < 208 || fileSize < 208 {
			t.Fatal("accepted a truncated fixed header")
		}
		tail := h.tail
		if tail != 0 && (tail%8 != 0 || tail < 208 || tail > uint64(fileSize)-16) {
			t.Fatalf("tail offset %d cannot address an object in %d bytes", tail, fileSize)
		}
	})
}
