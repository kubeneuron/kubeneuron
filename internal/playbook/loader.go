// SPDX-License-Identifier: Apache-2.0

package playbook

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LoadFile reads and validates a single playbook YAML file.
func LoadFile(path string) (*Playbook, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Playbook
	if err := decodeStrict(data, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

// decodeStrict decodes exactly one YAML document and rejects unknown keys, so
// a misspelt key such as `approve: required` is an error instead of silently
// meaning "no approval".
func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("empty playbook file")
		}
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected second YAML document")
	}
	return nil
}

// LoadDir reads every *.yaml/*.yml playbook in a directory and returns them
// keyed by name. Escalation references (on_failure.escalate_to) are resolved
// against the loaded set and must exist.
func LoadDir(dir string) (map[string]*Playbook, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	books := map[string]*Playbook{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		p, err := LoadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if _, dup := books[p.Name]; dup {
			return nil, fmt.Errorf("duplicate playbook name %q", p.Name)
		}
		books[p.Name] = p
	}
	// Resolve escalation references and reject any cycle or self-reference.
	if err := ValidateEscalationGraph(books); err != nil {
		return nil, err
	}
	return books, nil
}

// ValidateEscalationGraph checks the on_failure.escalate_to graph over a set of
// playbooks: every target must exist, and the graph must be acyclic (a
// self-reference is the smallest cycle). A cycle such as A->B->A compiles
// cleanly on a per-playbook existence check yet, under a persistent fault, lets
// the ladder repeat its destructive rungs forever without ever reaching a
// human. Rejecting it here — at load and at compile — is the first of the three
// guards against that (the others are the per-incident escalation cap and the
// cooldown honored by escalated rungs).
func ValidateEscalationGraph(books map[string]*Playbook) error {
	for start, book := range books {
		if book.OnFailure.EscalateTo == "" {
			continue
		}
		visited := map[string]bool{start: true}
		cur := start
		for {
			next := books[cur].OnFailure.EscalateTo
			if next == "" {
				break
			}
			if next == cur {
				return fmt.Errorf("playbook %q escalates to itself (on_failure.escalate_to)", cur)
			}
			if _, ok := books[next]; !ok {
				return fmt.Errorf("playbook %q escalates to unknown playbook %q", cur, next)
			}
			if visited[next] {
				return fmt.Errorf("playbook escalation cycle detected: %q eventually escalates back to %q", start, next)
			}
			visited[next] = true
			cur = next
		}
	}
	return nil
}
