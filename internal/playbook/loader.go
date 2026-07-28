package playbook

import (
	"fmt"
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
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
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
	// Resolve escalation references.
	for _, p := range books {
		if to := p.OnFailure.EscalateTo; to != "" {
			if _, ok := books[to]; !ok {
				return nil, fmt.Errorf("playbook %q escalates to unknown playbook %q", p.Name, to)
			}
		}
	}
	return books, nil
}
