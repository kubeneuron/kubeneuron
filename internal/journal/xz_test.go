// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

// The synthetic XZ streams contain MESSAGE= followed only by x characters.
// The dictionary case declares a 16 MiB dictionary for a 64-byte field.
func TestJournalXZLimits(t *testing.T) {
	var decoders fieldDecoders
	for _, name := range []string{"limit", "oversize", "dictionary"} {
		t.Run(name, func(t *testing.T) {
			block, err := os.ReadFile("testdata/field-" + name + ".xz")
			if err != nil {
				t.Fatal(err)
			}
			got, err := decoders.decodeField(1, io.NewSectionReader(bytes.NewReader(block), 0, int64(len(block))))
			if name == "limit" {
				want := append([]byte("MESSAGE="), bytes.Repeat([]byte("x"), MaxFieldBytes-8)...)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("decoded %d bytes: %v", len(got), err)
				}
			} else if !errors.Is(err, ErrInvalidJournal) {
				t.Fatalf("expected invalid field, got %v", err)
			}
		})
	}
}

func TestXZInterruptedFieldThenCompleteField(t *testing.T) {
	var decoders fieldDecoders
	block, err := os.ReadFile("testdata/new.xz")
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 1, len(block) / 2, len(block) - 1} {
		got, err := decoders.decodeField(1, io.NewSectionReader(bytes.NewReader(block[:size]), 0, int64(size)))
		if !errors.Is(err, ErrInvalidJournal) || len(got) != 0 {
			t.Errorf("interrupted field of %d bytes: got %d decoded bytes, error %v", size, len(got), err)
		}
	}
	got, err := decoders.decodeField(1, io.NewSectionReader(bytes.NewReader(block), 0, int64(len(block))))
	if err != nil || string(got) != "MESSAGE=new" {
		t.Fatalf("complete field: got %q, error %v", got, err)
	}
}

// FuzzXZFieldSequence checks that reusing the dictionary across valid fields
// preserves the exact bytes of each message, irrespective of their order.
func FuzzXZFieldSequence(f *testing.F) {
	names := []string{"old", "new", "after rotation", "dict8"}
	messages := []string{"MESSAGE=old", "MESSAGE=new", "MESSAGE=after rotation", "MESSAGE=synthetic dictionary check"}
	blocks := make([][]byte, len(names))
	for i, name := range names {
		var err error
		blocks[i], err = os.ReadFile("testdata/" + name + ".xz")
		if err != nil {
			f.Fatal(err)
		}
	}
	f.Add([]byte{0, 1, 2, 3, 0})
	f.Fuzz(func(t *testing.T, order []byte) {
		var decoders fieldDecoders
		for _, selector := range order[:min(len(order), MaxEntriesPerTick)] {
			i := int(selector) % len(blocks)
			block := blocks[i]
			got, err := decoders.decodeField(1, io.NewSectionReader(bytes.NewReader(block), 0, int64(len(block))))
			if err != nil || string(got) != messages[i] {
				t.Fatalf("field %s: got %q, error %v", names[i], got, err)
			}
		}
	})
}

// FuzzXZShortRead varies where an otherwise valid field ends. A short
// read must not produce a partial message or affect the next complete field.
func FuzzXZShortRead(f *testing.F) {
	block, err := os.ReadFile("testdata/new.xz")
	if err != nil {
		f.Fatal(err)
	}
	for n := range len(block) {
		f.Add(uint16(n))
	}
	f.Fuzz(func(t *testing.T, position uint16) {
		var decoders fieldDecoders
		n := int64(int(position) % len(block))
		got, err := decoders.decodeField(1, io.NewSectionReader(bytes.NewReader(block), 0, n))
		if !errors.Is(err, ErrInvalidJournal) || len(got) != 0 {
			t.Fatalf("short field of %d bytes: decoded %d bytes, error %v", n, len(got), err)
		}
		got, err = decoders.decodeField(1, io.NewSectionReader(bytes.NewReader(block), 0, int64(len(block))))
		if err != nil || string(got) != "MESSAGE=new" {
			t.Fatalf("complete field: got %q, error %v", got, err)
		}
	})
}

// The synthetic stream contains one CRC64-protected block at offset 12,
// with total block size 36. Its last eight bytes are the block check.
func TestXZBlockChecksumCorruption(t *testing.T) {
	block, err := os.ReadFile("testdata/new.xz")
	if err != nil {
		t.Fatal(err)
	}
	for bit := range 64 {
		var decoders fieldDecoders
		damaged := bytes.Clone(block)
		damaged[40+bit/8] ^= 1 << (bit % 8)
		got, err := decoders.decodeField(1, io.NewSectionReader(bytes.NewReader(damaged), 0, int64(len(damaged))))
		if !errors.Is(err, ErrInvalidJournal) || len(got) != 0 {
			t.Fatalf("checksum bit %d: decoded %d bytes, error %v", bit, len(got), err)
		}
		got, err = decoders.decodeField(1, io.NewSectionReader(bytes.NewReader(block), 0, int64(len(block))))
		if err != nil || string(got) != "MESSAGE=new" {
			t.Fatalf("complete field after checksum bit %d: got %q, error %v", bit, got, err)
		}
	}
}
