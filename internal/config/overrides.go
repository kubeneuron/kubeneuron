package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// SignalOverride redefines how one detection source maps to a problem
// class. Overrides come from GPUSignalMapping CRs (compiled by the operator)
// and take precedence over the built-in catalog in internal/detect.
type SignalOverride struct {
	Name string `yaml:"name"`
	// Exactly one of XIDCodes or AlertName is set.
	XIDCodes  []int              `yaml:"xid_codes,omitempty"`
	AlertName string             `yaml:"alert_name,omitempty"`
	Class     types.ProblemClass `yaml:"class"`
	Severity  types.Severity     `yaml:"severity"`
}

// Validate checks structural invariants.
func (o SignalOverride) Validate() error {
	if o.Name == "" {
		return fmt.Errorf("signal override: name is required")
	}
	if (len(o.XIDCodes) == 0) == (o.AlertName == "") {
		return fmt.Errorf("signal override %q: exactly one of xid_codes or alert_name must be set", o.Name)
	}
	if o.Class == "" {
		return fmt.Errorf("signal override %q: class is required", o.Name)
	}
	switch o.Severity {
	case types.SeverityInfo, types.SeverityWarning, types.SeverityCritical:
	default:
		return fmt.Errorf("signal override %q: severity must be info, warning, or critical", o.Name)
	}
	for _, code := range o.XIDCodes {
		if code <= 0 {
			return fmt.Errorf("signal override %q: XID codes must be positive", o.Name)
		}
	}
	return nil
}

// NodeConfig is per-node desired state consumed by the controller.
type NodeConfig struct {
	NodeName string `yaml:"node_name"`
	Paused   bool   `yaml:"paused"`
}

// Validate checks structural invariants.
func (n NodeConfig) Validate() error {
	if n.NodeName == "" {
		return fmt.Errorf("node config: node_name is required")
	}
	return nil
}

type overridesFile struct {
	Overrides []SignalOverride `yaml:"overrides"`
}

type nodeConfigsFile struct {
	Nodes []NodeConfig `yaml:"nodes"`
}

// LoadSignalOverrides reads signal overrides from a YAML file; a missing
// file is an empty set.
func LoadSignalOverrides(path string) ([]SignalOverride, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	overrides, err := LoadSignalOverridesFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return overrides, nil
}

// LoadSignalOverridesFromBytes parses and validates an overrides document.
func LoadSignalOverridesFromBytes(data []byte) ([]SignalOverride, error) {
	var f overridesFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	for _, o := range f.Overrides {
		if err := o.Validate(); err != nil {
			return nil, err
		}
	}
	return f.Overrides, nil
}

// LoadNodeConfigs reads per-node configuration from a YAML file; a missing
// file is an empty set.
func LoadNodeConfigs(path string) ([]NodeConfig, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	nodes, err := LoadNodeConfigsFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return nodes, nil
}

// LoadNodeConfigsFromBytes parses and validates a node-configs document.
func LoadNodeConfigsFromBytes(data []byte) ([]NodeConfig, error) {
	var f nodeConfigsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	for _, n := range f.Nodes {
		if err := n.Validate(); err != nil {
			return nil, err
		}
	}
	return f.Nodes, nil
}
