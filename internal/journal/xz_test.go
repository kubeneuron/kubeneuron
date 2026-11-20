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
