// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"os"
	"path/filepath"
	"testing"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/config"
)

// The controller decodes its configuration strictly, so every key the
// operator renders must be one the controller declares. This loads the
// operator's output through the controller's own loaders, with every optional
// safety block switched on.
func TestCompiledSnapshotLoadsUnderStrictDecoding(t *testing.T) {
	policies, playbooks := testPolicyFixture()

	enabled := testKubeNeuron()
	enabled.Spec.Safety.ExecutionMode = kubeneuronv1alpha1.ExecutionModeEnabled
	enabled.Spec.Safety.DestructiveExecution = &kubeneuronv1alpha1.DestructiveExecutionSpec{
		NodeSelector:    map[string]string{"kubeneuron.io/destructive": "true"},
		Acknowledgement: "I understand these nodes may be reset, rebooted, or destroyed",
	}
	enabled.Spec.Safety.TaintDegradedNodes = &kubeneuronv1alpha1.TaintDegradedNodesSpec{Enabled: true}
	enabled.Spec.Safety.CheckpointCoordination = &kubeneuronv1alpha1.CheckpointCoordinationSpec{
		Enabled: true, Namespaces: []string{"training"},
	}
	enabled.Spec.Safety.ImpactPreview = &kubeneuronv1alpha1.ImpactPreviewSpec{Enabled: true}
	enabled.Spec.Safety.TopologyClassifier = &kubeneuronv1alpha1.TopologyClassifierSpec{
		DRA: &kubeneuronv1alpha1.TopologyClassifierDRASpec{Enabled: true},
	}

	for name, installation := range map[string]*kubeneuronv1alpha1.KubeNeuron{
		"dry-run": testKubeNeuron(),
		"enabled": enabled,
	} {
		t.Run(name, func(t *testing.T) {
			snapshot, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "policies.yaml")
			if err := os.WriteFile(path, snapshot.PoliciesYAML, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("controller rejects the operator's policies:\n%s\nerror: %v", snapshot.PoliciesYAML, err)
			}
			if wantDryRun := name == "dry-run"; cfg.Safety.DryRun != wantDryRun {
				t.Fatalf("DryRun = %v, want %v", cfg.Safety.DryRun, wantDryRun)
			}
			if _, err := config.LoadWindowsFromBytes(snapshot.WindowsYAML); err != nil {
				t.Fatalf("windows: %v", err)
			}
			if _, err := config.LoadSignalOverridesFromBytes(snapshot.MappingsYAML); err != nil {
				t.Fatalf("mappings: %v", err)
			}
			if _, err := config.LoadNodeConfigsFromBytes(snapshot.NodeConfigsYAML); err != nil {
				t.Fatalf("node configs: %v", err)
			}
		})
	}
}
