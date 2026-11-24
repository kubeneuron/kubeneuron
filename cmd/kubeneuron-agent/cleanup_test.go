// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupLeavesExternalSymlinkTarget(t *testing.T) {
	mount := t.TempDir()
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	bundles := filepath.Join(mount, "bundles")
	if err := os.Mkdir(bundles, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(bundles, "link")); err != nil {
		t.Fatal(err)
	}
	if err := emptyAgentData(mount); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(mount); err != nil || len(entries) != 0 {
		t.Fatalf("remaining entries %v, error %v", entries, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
}
