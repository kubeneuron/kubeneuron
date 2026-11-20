// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// Vary the layout, codec and number of appended entries while retaining valid
// object references. Starting at an empty tail exercises delivery on every seed.
func FuzzFollowKernelEntries(f *testing.F) {
	for codec := uint8(0); codec < 3; codec++ {
		for _, compact := range []bool{false, true} {
			for _, n := range []uint16{0, 255, 256, 319} {
				f.Add(codec, compact, n)
			}
		}
	}
	f.Fuzz(func(t *testing.T, codec uint8, compact bool, n uint16) {
		count := 1 + int(n%320)
		field := []byte("MESSAGE=new")
		var block []byte
		var header uint32
		var flag byte
		switch codec % 3 {
		case 0:
			var err error
			block, err = os.ReadFile("testdata/new.xz")
			if err != nil {
				t.Fatal(err)
			}
			header, flag = 1, 1
		case 1:
			block = make([]byte, 8+lz4.CompressBlockBound(len(field)))
			size, err := lz4.CompressBlock(field, block[8:], nil)
			if err != nil || size == 0 {
				t.Fatalf("compress: %d %v", size, err)
			}
			binary.LittleEndian.PutUint64(block, uint64(len(field)))
			block = block[:8+size]
			header, flag = 2, 2
		case 2:
			enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
			if err != nil {
				t.Fatal(err)
			}
			block = enc.EncodeAll(field, nil)
			enc.Close()
			header, flag = 8, 4
		}
		base, stride, width := 64, 16, 8
		if compact {
			base, stride, width = 72, 4, 4
			header |= 16
		}
		path := filepath.Join(t.TempDir(), "system.journal")
		buf := make([]byte, 208)
		copy(buf, "LPKSHHRH")
		put := binary.LittleEndian.PutUint64
		put(buf[88:], 208)
		binary.LittleEndian.PutUint32(buf[12:], header)
		write := func() {
			if err := os.WriteFile(path, buf, 0600); err != nil {
				t.Fatal(err)
			}
		}
		write()
		reader, err := Open(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		tail, err := reader.Next(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		add := func(kind byte, size int) int {
			for len(buf)%8 != 0 {
				buf = append(buf, 0)
			}
			pos := len(buf)
			buf = append(buf, make([]byte, size)...)
			buf[pos] = kind
			put(buf[pos+8:], uint64(size))
			put(buf[136:], uint64(pos))
			put(buf[96:], uint64(len(buf)-208))
			return pos
		}
		transport := add(1, base+len("_TRANSPORT=kernel"))
		copy(buf[transport+base:], "_TRANSPORT=kernel")
		msg := add(1, base+len(block))
		copy(buf[msg+base:], block)
		buf[msg+1] = flag
		offsets := make([]int, count)
		for i := range offsets {
			pos := add(3, 64+2*stride)
			offsets[i] = pos
			put(buf[pos+16:], uint64(i+1))
			binary.LittleEndian.PutUint32(buf[pos+64:], uint32(transport))
			binary.LittleEndian.PutUint32(buf[pos+64+stride:], uint32(msg))
		}
		put(buf[transport+40:], uint64(offsets[0]))
		if count > 1 {
			array := add(6, 24+(count-1)*width)
			for i, pos := range offsets[1:] {
				binary.LittleEndian.PutUint32(buf[array+24+i*width:], uint32(pos))
			}
			put(buf[transport+48:], uint64(array))
		}
		put(buf[transport+56:], uint64(count))
		put(buf[160:], uint64(count))
		write()
		cursor := tail.Cursor
		seen := 0
		for range 12 {
			b, err := reader.Next(context.Background(), cursor)
			if err != nil || b.UnreadableEntries != 0 || len(b.Records) > MaxEntriesPerTick {
				t.Fatalf("batch: records=%d skips=%d err=%v", len(b.Records), b.UnreadableEntries, err)
			}
			size := 0
			for _, rec := range b.Records {
				if rec.Message != "new" {
					t.Fatal("unexpected record")
				}
				size += len(rec.Message)
			}
			if size > MaxBytesPerTick {
				t.Fatal("message budget exceeded")
			}
			seen += len(b.Records)
			if !b.More {
				if seen != count {
					t.Fatalf("got %d records, want %d", seen, count)
				}
				return
			}
			if cursor == b.Cursor {
				t.Fatal("cursor did not advance")
			}
			cursor = b.Cursor
		}
		t.Fatal("backlog did not drain")
	})
}
