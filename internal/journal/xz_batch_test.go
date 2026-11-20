// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
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
		var decoders fieldDecoders
		for range MaxEntriesPerTick {
			got, err := decoders.decodeField(1, io.NewSectionReader(bytes.NewReader(block), 0, int64(len(block))))
			if err != nil || string(got) != "MESSAGE=synthetic dictionary check" {
				b.Fatalf("unexpected field %q, error %v", got, err)
			}
		}
	}
}

// Setup checkpoints an empty system journal. Each iteration opens a fresh reader
// and consumes the same 256 records so the first dictionary allocation is included.
func BenchmarkXZOpenNextBatch(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "system.journal")
	buf := make([]byte, 208)
	copy(buf, "LPKSHHRH")
	put := binary.LittleEndian.PutUint64
	put(buf[88:], 208)
	add := func(kind byte, size int) int {
		for len(buf)%8 != 0 {
			buf = append(buf, 0)
		}
		off := len(buf)
		buf = append(buf, make([]byte, size)...)
		buf[off] = kind
		put(buf[off+8:], uint64(size))
		put(buf[136:], uint64(off))
		put(buf[96:], uint64(len(buf)-208))
		return off
	}
	data := func(field string) int { off := add(1, 64+len(field)); copy(buf[off+64:], field); return off }
	transport := data("_TRANSPORT=kernel")
	entry := func(seq uint64, field string) int {
		msg := data(field)
		buf[msg+1] = 1
		off := add(3, 96)
		put(buf[off+16:], seq)
		put(buf[off+64:], uint64(transport))
		put(buf[off+80:], uint64(msg))
		put(buf[160:], seq)
		return off
	}

	binary.LittleEndian.PutUint32(buf[12:], 1)
	write := func() {
		if err := os.WriteFile(path, buf, 0600); err != nil {
			b.Fatal(err)
		}
	}
	write()
	r, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	tail, err := r.Next(context.Background(), "")
	if err != nil || tail.Cursor == "" {
		b.Fatalf("checkpoint: %v", err)
	}
	block, err := os.ReadFile("testdata/dict8.xz")
	if err != nil {
		b.Fatal(err)
	}
	var offsets []int
	for i := range MaxEntriesPerTick {
		offsets = append(offsets, entry(uint64(i+1), string(block)))
	}
	array := add(6, 24+8*(len(offsets)-1))
	for i, off := range offsets[1:] {
		put(buf[array+24+i*8:], uint64(off))
	}
	put(buf[transport+40:], uint64(offsets[0]))
	put(buf[transport+48:], uint64(array))
	put(buf[transport+56:], uint64(len(offsets)))
	write()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		r, err := Open(dir)
		if err != nil {
			b.Fatal(err)
		}
		batch, err := r.Next(context.Background(), tail.Cursor)
		if err != nil || len(batch.Records) != MaxEntriesPerTick || batch.UnreadableEntries != 0 {
			b.Fatalf("read %d records, %d unreadable: %v", len(batch.Records), batch.UnreadableEntries, err)
		}
		for _, record := range batch.Records {
			if record.Message != "synthetic dictionary check" {
				b.Fatalf("unexpected message %q", record.Message)
			}
		}
	}
}
