// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"context"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestFollowFromTail(t *testing.T) {
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
	fresh := entry(2, "MESSAGE=new")
	array := add(6, 32)
	put(buf[array+24:], uint64(fresh))
	put(buf[transport+48:], uint64(array))
	put(buf[transport+56:], 2)
	write()
	b, err = r.Next(context.Background(), b.Cursor)
	if err != nil || len(b.Records) != 1 || b.Records[0].Message != "new" {
		t.Fatalf("append %+v %v", b, err)
	}
	b, err = r.Next(context.Background(), b.Cursor)
	if err != nil || len(b.Records) != 0 {
		t.Fatalf("idle %+v %v", b, err)
	}
}
