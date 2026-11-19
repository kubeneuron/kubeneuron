// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/pierrec/lz4/v4"
)

func TestJournalLZ4Block(t *testing.T) {
	plain := append([]byte("MESSAGE="), bytes.Repeat([]byte("synthetic "), 200)...)
	buf := make([]byte, 8+lz4.CompressBlockBound(len(plain)))
	n, err := lz4.CompressBlock(plain, buf[8:], nil)
	if err != nil || n == 0 {
		t.Fatalf("compress: %d %v", n, err)
	}
	buf = buf[:8+n]
	decode := func(size uint64) ([]byte, error) {
		binary.LittleEndian.PutUint64(buf, size)
		return readLZ4(io.NewSectionReader(bytes.NewReader(buf), 0, int64(len(buf))))
	}
	got, err := decode(uint64(len(plain)))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("decode: %v", err)
	}
	for _, bad := range []uint64{0, uint64(len(plain) + 1), MaxFieldBytes + 1, ^uint64(0)} {
		if _, err := decode(bad); !errors.Is(err, ErrInvalidJournal) {
			t.Fatalf("size %d: %v", bad, err)
		}
	}
}
