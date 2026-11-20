// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestJournalZstdFieldLimit(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	for _, size := range []int{1024, MaxFieldBytes, MaxFieldBytes + 1} {
		field := bytes.Repeat([]byte("x"), size)
		block := encoder.EncodeAll(field, nil)
		got, err := readZstd(io.NewSectionReader(bytes.NewReader(block), 0, int64(len(block))))
		if size > MaxFieldBytes {
			if !errors.Is(err, ErrInvalidJournal) {
				t.Fatalf("size %d: %v", size, err)
			}
		} else if err != nil || !bytes.Equal(got, field) {
			t.Fatalf("size %d: %v", size, err)
		}
	}
}
