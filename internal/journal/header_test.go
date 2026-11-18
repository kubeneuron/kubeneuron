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
	if tail, err := readHeader(bytes.NewReader(data[:]), 224); tail != 208 || err != nil {
		t.Fatalf("tail %d, error %v", tail, err)
	}
	for _, index := range []int{0, 12, 16, 88, 96, 136} {
		bad := data
		bad[index] = 255
		if _, err := readHeader(bytes.NewReader(bad[:]), 224); err == nil {
			t.Fatalf("accepted corrupt header at byte %d", index)
		}
	}
}
