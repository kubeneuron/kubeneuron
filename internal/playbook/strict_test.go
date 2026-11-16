// SPDX-License-Identifier: Apache-2.0

package playbook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A misspelt key must be an error, not a silently dropped field: `approve:
// required` used to load as a step with no approval at all (#43).
func TestLoadDirRejectsUnknownKeysAndExtraDocuments(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"misspelt step key": {"name: a\ntarget: node\nsteps:\n  - name: drain\n    action: platform.drain\n    approve: required\n", "field approve not found"},
		"unknown top key":   {"name: a\ntarget: node\nretries: 3\nsteps:\n  - name: observe\n    action: notify.observe\n", "retries"},
		"second document":   {"name: a\ntarget: node\nsteps:\n  - name: observe\n    action: notify.observe\n---\nname: b\n", "second YAML document"},
		"empty file":        {"", "empty playbook file"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadDir(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadDir() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// Every playbook the project ships must load under strict decoding.
func TestShippedPlaybooksLoadStrictly(t *testing.T) {
	books, err := LoadDir(filepath.Join("..", "..", "configs", "playbooks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(books) == 0 {
		t.Fatal("no shipped playbooks found")
	}
}
