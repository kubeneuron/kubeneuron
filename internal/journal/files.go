// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// systemFiles enumerates system journals directly below the mount or in
// journald's machine-ID directory. User journals and symlinks are skipped.
func systemFiles(root string) ([]string, error) {
	var archived, active []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			if !d.IsDir() {
				return &os.PathError{Op: "open journal directory", Path: path, Err: fs.ErrInvalid}
			}
			return nil
		}
		if d.IsDir() {
			if filepath.Dir(path) != root {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !d.Type().IsRegular() || !(name == "system.journal" || strings.HasPrefix(name, "system@") && strings.HasSuffix(name, ".journal")) {
			return nil
		}
		if name == "system.journal" {
			active = append(active, path)
		} else {
			archived = append(archived, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(archived)+len(active) == 0 {
		return nil, &os.PathError{Op: "find system journal", Path: root, Err: fs.ErrNotExist}
	}
	return append(archived, active...), nil
}
