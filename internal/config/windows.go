package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// MaintenanceWindow pauses automation for matching nodes during a bounded
// time range. Windows come from GPUMaintenanceWindow CRs (compiled by the
// operator into windows.yaml) or a local file on bare metal.
type MaintenanceWindow struct {
	Name     string    `yaml:"name"`
	StartsAt time.Time `yaml:"starts_at"`
	EndsAt   time.Time `yaml:"ends_at"`
	// MatchLabels selects nodes; every key/value must be present on the
	// node. An empty selector matches every node.
	MatchLabels map[string]string `yaml:"match_labels,omitempty"`
}

// ActiveAt reports whether the window covers the given moment.
func (w MaintenanceWindow) ActiveAt(t time.Time) bool {
	return !t.Before(w.StartsAt) && t.Before(w.EndsAt)
}

// MatchesLabels reports whether the node labels satisfy the selector.
func (w MaintenanceWindow) MatchesLabels(labels map[string]string) bool {
	for k, v := range w.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// Validate checks structural invariants.
func (w MaintenanceWindow) Validate() error {
	if w.Name == "" {
		return fmt.Errorf("maintenance window: name is required")
	}
	if w.StartsAt.IsZero() || w.EndsAt.IsZero() {
		return fmt.Errorf("maintenance window %q: starts_at and ends_at are required", w.Name)
	}
	if !w.StartsAt.Before(w.EndsAt) {
		return fmt.Errorf("maintenance window %q: starts_at must be before ends_at", w.Name)
	}
	return nil
}

type windowsFile struct {
	Windows []MaintenanceWindow `yaml:"windows"`
}

// LoadWindows reads maintenance windows from a YAML file. A missing file is
// an empty set, not an error, so installations without windows need no file.
func LoadWindows(path string) ([]MaintenanceWindow, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	windows, err := LoadWindowsFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return windows, nil
}

// LoadWindowsFromBytes parses and validates a windows document.
func LoadWindowsFromBytes(data []byte) ([]MaintenanceWindow, error) {
	var f windowsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	for _, w := range f.Windows {
		if err := w.Validate(); err != nil {
			return nil, err
		}
	}
	return f.Windows, nil
}
