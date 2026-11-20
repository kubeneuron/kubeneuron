// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// One iteration measures sequential decoding of one tick of small fields.
// The synthetic stream declares an 8 MiB LZMA2 dictionary.
func BenchmarkXZDictionaryBatch(b *testing.B) {
	block, err := os.ReadFile("testdata/dict8.xz")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for range MaxEntriesPerTick {
			got, err := readXZ(io.NewSectionReader(bytes.NewReader(block), 0, int64(len(block))))
			if err != nil || string(got) != "MESSAGE=synthetic dictionary check" {
				b.Fatalf("unexpected field %q, error %v", got, err)
			}
		}
	}
}
