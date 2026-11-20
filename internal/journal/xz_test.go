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
	for _, name := range []string{"limit", "oversize", "dictionary"} {
		t.Run(name, func(t *testing.T) {
			block, err := os.ReadFile("testdata/field-" + name + ".xz")
			if err != nil {
				t.Fatal(err)
			}
			got, err := readXZ(io.NewSectionReader(bytes.NewReader(block), 0, int64(len(block))))
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
	block, err := os.ReadFile("testdata/new.xz")
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 1, len(block) / 2, len(block) - 1} {
		got, err := readXZ(io.NewSectionReader(bytes.NewReader(block[:size]), 0, int64(size)))
		if !errors.Is(err, ErrInvalidJournal) || len(got) != 0 {
			t.Errorf("interrupted field of %d bytes: got %d decoded bytes, error %v", size, len(got), err)
		}
	}
	got, err := readXZ(io.NewSectionReader(bytes.NewReader(block), 0, int64(len(block))))
	if err != nil || string(got) != "MESSAGE=new" {
		t.Fatalf("complete field: got %q, error %v", got, err)
	}
}
