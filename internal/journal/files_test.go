// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestSystemFilesRequiresSystemJournal(t *testing.T) {
	root := t.TempDir()
	if _, err := systemFiles(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty root: %v", err)
	}
	dir := filepath.Join(root, "machine")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "user-1000.journal"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := systemFiles(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("only user journal: %v", err)
	}
	file := filepath.Join(dir, "system.journal")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	files, err := systemFiles(root)
	if err != nil || len(files) != 1 || files[0] != file {
		t.Fatalf("files %v: %v", files, err)
	}
}
