// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestFollowFromTail(t *testing.T) {
	t.Run("plain", func(t *testing.T) { testFollowFromTail(t, 0, 0, nil, false) })
	t.Run("zstandard", func(t *testing.T) {
		enc, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer enc.Close()
		testFollowFromTail(t, 1<<3, 4, func(field []byte) []byte { return enc.EncodeAll(field, nil) }, false)
		t.Run("decoder panic", func(t *testing.T) {
			testFollowFromTail(t, 1<<3, 4, func(field []byte) []byte { return enc.EncodeAll(field, nil) }, true)
		})
	})
	t.Run("xz", func(t *testing.T) {
		testFollowFromTail(t, 1, 1, func(field []byte) []byte {
			name := strings.TrimPrefix(string(field), "MESSAGE=")
			block, err := os.ReadFile(filepath.Join("testdata", name+".xz"))
			if err != nil {
				t.Fatal(err)
			}
			return block
		}, false)
	})
}

func testFollowFromTail(t *testing.T, headerFlag uint32, dataFlag byte, encode func([]byte) []byte, panicOnce bool) {
	dir := t.TempDir()
	if _, err := Open(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
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
		var msg int
		if encode == nil {
			msg = data(field)
		} else {
			binary.LittleEndian.PutUint32(buf[12:], headerFlag)
			block := encode([]byte(field))
			msg = add(1, 64+len(block))
			buf[msg+1] = dataFlag
			copy(buf[msg+64:], block)
		}
		off := add(3, 96)
		put(buf[off+16:], seq)
		put(buf[off+64:], uint64(transport))
		put(buf[off+80:], uint64(msg))
		put(buf[160:], seq)
		return off
	}
	old := entry(1, "MESSAGE=old")
	put(buf[transport+40:], uint64(old))
	put(buf[transport+56:], 1)
	write := func() {
		if err := os.WriteFile(path, buf, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Next(context.Background(), "")
	if err != nil || b.Cursor == "" || len(b.Records) != 0 {
		t.Fatalf("tail %+v %v", b, err)
	}
	if encode == nil {
		t.Run("vacuum during catch-up", func(t *testing.T) {
			archive := filepath.Join(dir, "system@vacuum.journal")
			header := append([]byte(nil), buf...)
			header[24] = 2
			if err := os.WriteFile(archive, header, 0600); err != nil {
				t.Fatal(err)
			}
			partial, err := r.(directoryReader).next(context.Background(), b.Cursor, 0)
			if err != nil || !partial.More {
				t.Fatalf("partial: %+v %v", partial, err)
			}
			if err := os.Remove(archive); err != nil {
				t.Fatal(err)
			}
			original := append([]byte(nil), buf...)
			defer func() { buf = original; write() }()
			newEntry := entry(2, "MESSAGE=survived")
			array := add(6, 32)
			put(buf[array+24:], uint64(newEntry))
			put(buf[transport+48:], uint64(array))
			put(buf[transport+56:], 2)
			write()
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			got, err := reopened.Next(context.Background(), partial.Cursor)
			if err != nil || got.More || got.UnreadableEntries != 1 || len(got.Records) != 1 || got.Records[0].Message != "survived" {
				t.Fatalf("vacuum: %+v %v", got, err)
			}
			idle, err := reopened.Next(context.Background(), got.Cursor)
			if err != nil || idle.More || idle.UnreadableEntries != 0 || len(idle.Records) != 0 {
				t.Fatalf("idle: %+v %v", idle, err)
			}
		})
	}
	fresh := entry(2, "MESSAGE=new")
	array := add(6, 32)
	put(buf[array+24:], uint64(fresh))
	put(buf[transport+48:], uint64(array))
	put(buf[transport+56:], 2)
	write()
	// Rotate before the reader sees the last append. A fresh reader must
	// finish the renamed file before it delivers the new active file.
	if err := os.Rename(path, filepath.Join(dir, "system@archive.journal")); err != nil {
		t.Fatal(err)
	}
	buf = make([]byte, 208)
	copy(buf, "LPKSHHRH")
	buf[24] = 1
	put(buf[88:], 208)
	transport = data("_TRANSPORT=kernel")
	first := entry(3, "MESSAGE=after rotation")
	put(buf[transport+40:], uint64(first))
	put(buf[transport+56:], 1)
	write()
	r, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if encode == nil {
		t.Run("broken archive header", func(t *testing.T) {
			path := filepath.Join(dir, "system@0.journal")
			if err := os.WriteFile(path, []byte("truncated"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Remove(path) })
			reader, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			got, err := reader.Next(context.Background(), b.Cursor)
			if err != nil || got.UnreadableEntries != 1 || len(got.Records) != 2 || got.Records[1].Message != "after rotation" {
				t.Fatalf("after bad header: %+v %v", got, err)
			}
			got, err = reader.Next(context.Background(), got.Cursor)
			if err != nil || got.UnreadableEntries != 0 || len(got.Records) != 0 || got.More {
				t.Fatalf("idle: %+v %v", got, err)
			}
		})
	}
	if panicOnce {
		reader := r.(directoryReader)
		first := true
		reader.decode = func(flags byte, payload *io.SectionReader) ([]byte, error) {
			if first {
				first = false
				panic("synthetic decoder failure")
			}
			return new(fieldDecoders).decodeField(flags, payload)
		}
		r = reader
	}
	b, err = r.Next(context.Background(), b.Cursor)
	if panicOnce {
		if err != nil || b.UnreadableEntries != 1 || len(b.Records) != 1 || b.Records[0].Message != "after rotation" {
			t.Fatalf("after panic %+v %v", b, err)
		}
	} else if err != nil || len(b.Records) != 2 || b.Records[0].Message != "new" || b.Records[1].Message != "after rotation" {
		t.Fatalf("append %+v %v", b, err)
	}
	b, err = r.Next(context.Background(), b.Cursor)
	if err != nil || len(b.Records) != 0 {
		t.Fatalf("idle %+v %v", b, err)
	}
}

func TestResumeLargeUncompressedBacklog(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
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
		off := add(3, 96)
		put(buf[off+16:], seq)
		put(buf[off+64:], uint64(transport))
		put(buf[off+80:], uint64(msg))
		put(buf[160:], seq)
		return off
	}
	old := entry(1, "MESSAGE=old")
	put(buf[transport+40:], uint64(old))
	put(buf[transport+56:], 1)
	write := func() {
		if err := os.WriteFile(path, buf, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Next(context.Background(), "")
	if err != nil || b.Cursor == "" || len(b.Records) != 0 {
		t.Fatalf("tail %+v %v", b, err)
	}
	const count = 18
	var offsets []int
	var want []string
	padding := strings.Repeat("k", 512<<10)
	for i := range count {
		message := fmt.Sprintf("%02d %s", i, padding)
		want = append(want, message)
		offsets = append(offsets, entry(uint64(i+2), "MESSAGE="+message))
	}
	array := add(6, 24+8*count)
	for i, off := range offsets {
		put(buf[array+24+i*8:], uint64(off))
	}
	put(buf[transport+48:], uint64(array))
	put(buf[transport+56:], count+1)
	write()
	seen := 0
	for tick := 0; tick < 64; tick++ {
		r, err = Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		b, err = r.Next(context.Background(), b.Cursor)
		if err != nil {
			t.Fatal(err)
		}
		bytes := 0
		for _, record := range b.Records {
			if seen >= len(want) || record.Message != want[seen] {
				t.Fatalf("unexpected message at index %d", seen)
			}
			seen++
			bytes += len(record.Message)
		}
		if bytes > MaxBytesPerTick {
			t.Errorf("tick %d delivered %d uncompressed message bytes (limit %d)", tick, bytes, MaxBytesPerTick)
		}
		if tick == 0 {
			if !b.More || seen == 0 || seen >= count {
				t.Fatalf("expected a partial batch before rotation, received %d records, More=%v", seen, b.More)
			}
			if err := os.Rename(path, filepath.Join(dir, "system@archive.journal")); err != nil {
				t.Fatal(err)
			}
			buf = make([]byte, 208)
			copy(buf, "LPKSHHRH")
			buf[24] = 1
			put(buf[88:], 208)
			transport = data("_TRANSPORT=kernel")
			const marker = "first message after rotation"
			off := entry(count+2, "MESSAGE="+marker)
			put(buf[transport+40:], uint64(off))
			put(buf[transport+56:], 1)
			write()
			want = append(want, marker)
		}
		if !b.More && seen == len(want) && len(b.Records) == 0 {
			return
		}
	}
	t.Fatalf("did not reach idle tail after delivering %d/%d records", seen, len(want))
}

func TestHeaderBudgetResumesInitialTail(t *testing.T) {
	for _, names := range [][]string{{"system@a.journal", "system@b.journal", "system.journal"}, {"a/system.journal", "b/system.journal", "c/system.journal"}} {
		t.Run(names[0], func(t *testing.T) { testHeaderBudget(t, names) })
	}
}

func testHeaderBudget(t *testing.T, names []string) {
	dir := t.TempDir()
	for i, name := range names {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 208)
		copy(buf, "LPKSHHRH")
		buf[24] = byte(i + 1)
		binary.LittleEndian.PutUint64(buf[88:], 208)
		binary.LittleEndian.PutUint64(buf[160:], uint64(i+1))
		if err := os.WriteFile(filepath.Join(dir, name), buf, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.(directoryReader).next(context.Background(), "", 2*208)
	if err != nil || !b.More || len(b.Records) != 0 {
		t.Fatalf("first tick: %+v %v", b, err)
	}
	r, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err = r.(directoryReader).next(context.Background(), b.Cursor, 2*208)
	if err != nil || b.More || len(b.Records) != 0 {
		t.Fatalf("second tick: %+v %v", b, err)
	}
	var state scanCursor
	if err = json.Unmarshal([]byte(b.Cursor), &state); err != nil || state.Starting || len(state.Positions) != 3 {
		t.Fatalf("tail positions: %+v %v", state, err)
	}
}

func TestUnknownCursorStartsAtTail(t *testing.T) {
	dir := t.TempDir()
	buf := make([]byte, 208)
	copy(buf, "LPKSHHRH")
	binary.LittleEndian.PutUint64(buf[88:], 208)
	binary.LittleEndian.PutUint64(buf[160:], 42)
	if err := os.WriteFile(filepath.Join(dir, "system.journal"), buf, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	many := scanCursor{FileList: strings.Repeat("0", 64), Positions: map[string]filePosition{}}
	for i := 0; i < 129; i++ {
		many.Positions[fmt.Sprintf("%032x", i)] = filePosition{After: 99}
	}
	raw, _ := json.Marshal(many)
	for _, after := range []Cursor{Cursor(raw), Cursor(strings.Repeat(" ", 65536) + string(raw)), `{"00000000000000000000000000000000":{"After":1}}`, "broken"} {
		b, err := r.Next(context.Background(), after)
		var s scanCursor
		if err != nil || b.UnreadableEntries != 1 || len(b.Records) != 0 || b.More || json.Unmarshal([]byte(b.Cursor), &s) != nil || s.Positions[strings.Repeat("0", 32)].After != 42 {
			t.Fatalf("tail: %+v %v", b, err)
		}
	}
}

func TestLimitedJournalSet(t *testing.T) {
	dir := t.TempDir()
	write := func(i int, name string) {
		b := make([]byte, 208)
		copy(b, "LPKSHHRH")
		b[24] = byte(i)
		binary.LittleEndian.PutUint64(b[88:], 208)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(i+1), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	var cursor Cursor
	tick := func(n, skip int) scanCursor {
		r, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		b, err := r.Next(context.Background(), cursor)
		if err != nil {
			t.Fatal(err)
		}
		var state scanCursor
		if err := json.Unmarshal([]byte(b.Cursor), &state); err != nil {
			t.Fatal(err)
		}
		if b.More || len(state.Positions) != n || b.UnreadableEntries != skip || len(b.Cursor) > 65536 {
			t.Fatalf("positions=%d skip=%d more=%t", len(state.Positions), b.UnreadableEntries, b.More)
		}
		cursor = b.Cursor
		return state
	}
	for i := 0; i < 4; i++ {
		write(i, "system.journal")
		tick(1, 0)
	}
	for i := 4; i <= 131; i++ {
		write(i, fmt.Sprintf("system@%03d.journal", i))
	}
	state := tick(128, 1)
	if _, ok := state.Positions["03000000000000000000000000000000"]; ok {
		t.Fatal("oldest file retained")
	}
	tick(128, 0)
}
