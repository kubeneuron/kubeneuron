// Command gpucatalog renders the canonical GPU metric catalog
// (internal/gpucatalog) into its checked-in artifacts, or verifies that the
// checked-in artifacts are current.
//
//	go run ./hack/gpucatalog            # write artifacts under the repo root
//	go run ./hack/gpucatalog -check     # exit 1 if any artifact is stale
//	go run ./hack/gpucatalog -root DIR  # operate on another checkout
//
// Output is a pure function of the Go tables, so a clean `-check` after a
// write proves there is no drift.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kubeneuron/kubeneuron/internal/gpucatalog"
)

func main() {
	check := flag.Bool("check", false, "verify artifacts instead of writing them")
	root := flag.String("root", "", "repository root (default: nearest go.mod above the working directory)")
	flag.Parse()

	if *root == "" {
		r, err := findRoot()
		if err != nil {
			fatal(err)
		}
		*root = r
	}
	rendered, err := gpucatalog.Render(gpucatalog.Default())
	if err != nil {
		fatal(fmt.Errorf("catalog invalid: %w", err))
	}

	stale := 0
	for _, rel := range gpucatalog.Paths() {
		path := filepath.Join(*root, rel)
		want := rendered[rel]
		if *check {
			got, err := os.ReadFile(path)
			switch {
			case err != nil:
				fmt.Fprintf(os.Stderr, "MISSING %s: %v\n", rel, err)
				stale++
			case !bytes.Equal(got, want):
				fmt.Fprintf(os.Stderr, "STALE   %s\n", rel)
				stale++
			default:
				fmt.Printf("ok      %s\n", rel)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote   %s\n", rel)
	}
	if stale > 0 {
		fmt.Fprintf(os.Stderr, "%d artifact(s) out of date; run `make generate-gpucatalog`\n", stale)
		os.Exit(1)
	}
}

func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gpucatalog:", err)
	os.Exit(2)
}
