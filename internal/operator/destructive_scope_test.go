package operator

import (
	"testing"

	"gopkg.in/yaml.v3"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/config"
)

// R1: the destructiveExecution.nodeSelector must reach the controller through
// the runtime config snapshot so the controller can confine its own
// destructive platform steps to the declared blast radius — not only the agent
// DaemonSet. An Enabled install compiles the selector; a dry-run install
// compiles none.
func TestDestructiveNodeSelectorCompiledIntoSnapshot(t *testing.T) {
	policies, playbooks := testPolicyFixture()

	enabled := testKubeNeuron()
	enabled.Spec.Safety.ExecutionMode = kubeneuronv1alpha1.ExecutionModeEnabled
	enabled.Spec.Safety.DestructiveExecution = &kubeneuronv1alpha1.DestructiveExecutionSpec{
		NodeSelector:    map[string]string{"kubeneuron.io/destructive": "true", "pool": "canary"},
		Acknowledgement: "I understand these nodes may be reset, rebooted, or destroyed",
	}
	snapshot, err := CompileSnapshot(enabled, policies, playbooks, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(snapshot.PoliciesYAML, &cfg); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"kubeneuron.io/destructive": "true", "pool": "canary"}
	if len(cfg.Safety.DestructiveExecutionNodeSelector) != len(want) {
		t.Fatalf("compiled selector = %v, want %v", cfg.Safety.DestructiveExecutionNodeSelector, want)
	}
	for k, v := range want {
		if cfg.Safety.DestructiveExecutionNodeSelector[k] != v {
			t.Fatalf("compiled selector = %v, want %v", cfg.Safety.DestructiveExecutionNodeSelector, want)
		}
	}

	// A dry-run install carries no selector: nothing executes, so there is
	// nothing to confine.
	dryRun := testKubeNeuron()
	snap2, err := CompileSnapshot(dryRun, policies, playbooks, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var cfg2 config.Config
	if err := yaml.Unmarshal(snap2.PoliciesYAML, &cfg2); err != nil {
		t.Fatal(err)
	}
	if len(cfg2.Safety.DestructiveExecutionNodeSelector) != 0 {
		t.Fatalf("dry-run install compiled a selector: %v", cfg2.Safety.DestructiveExecutionNodeSelector)
	}
}
