// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
		name := strings.TrimSuffix(d.Name(), "~")
		systemJournal := name == "system.journal" || strings.HasPrefix(name, "system@") && strings.HasSuffix(name, ".journal")
		if !d.Type().IsRegular() || !systemJournal {
			return nil
		}
		if d.Name() == "system.journal" {
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

const maxJournalFiles = 128

// Pick the most recently modified journals, retaining the normal traversal
// order among those selected. The original order breaks modification-time ties.
func selectedFiles(root string) ([]string, bool, error) {
	paths, err := systemFiles(root)
	if err != nil || len(paths) <= maxJournalFiles {
		return paths, false, err
	}
	times := make(map[string]time.Time, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, false, err
		}
		times[path] = info.ModTime()
	}
	ranked := append([]string(nil), paths...)
	sort.SliceStable(ranked, func(i, j int) bool { return times[ranked[i]].After(times[ranked[j]]) })
	for _, path := range ranked[maxJournalFiles:] {
		delete(times, path)
	}
	kept := paths[:0]
	for _, path := range paths {
		if _, ok := times[path]; ok {
			kept = append(kept, path)
		}
	}
	return kept, true, nil
}
