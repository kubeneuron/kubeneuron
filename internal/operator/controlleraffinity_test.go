package operator

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

// A controller that can be scheduled onto a GPU node can be rebooted by its own
// playbook. That happened on hardware: the reboot succeeded, the boot_id guard
// stopped a second one, and the playbook was cut off mid-step leaving the node
// cordoned.
func TestControllerIsKeptOffGPUNodesByDefault(t *testing.T) {
	affinity := controllerAffinity(&kubeneuronv1alpha1.KubeNeuron{})
	if affinity == nil || affinity.NodeAffinity == nil {
		t.Fatal("want node affinity that excludes GPU nodes")
	}
	required := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil || len(required.NodeSelectorTerms) != 1 {
		t.Fatalf("affinity = %+v, want one required term", affinity.NodeAffinity)
	}
	expressions := required.NodeSelectorTerms[0].MatchExpressions
	if len(expressions) != 1 {
		t.Fatalf("expressions = %+v", expressions)
	}
	got := expressions[0]
	if got.Key != gpuPresentLabel || got.Operator != corev1.NodeSelectorOpNotIn {
		t.Fatalf("expression = %+v, want %s NotIn true", got, gpuPresentLabel)
	}
	// Required rather than preferred: a cluster where every node has a GPU
	// should make that choice explicitly instead of inheriting the failure.
	if len(got.Values) != 1 || got.Values[0] != "true" {
		t.Fatalf("values = %v", got.Values)
	}
}

func TestControllerMayOptIntoGPUNodes(t *testing.T) {
	installation := &kubeneuronv1alpha1.KubeNeuron{}
	installation.Spec.Controller.AllowGPUNodes = true
	if affinity := controllerAffinity(installation); affinity != nil {
		t.Fatalf("affinity = %+v, want none when the operator opts in", affinity)
	}
}
