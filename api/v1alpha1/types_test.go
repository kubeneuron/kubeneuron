package v1alpha1

import (
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeepCopyPreservesCollectionShapeAndOverwritesDestination(t *testing.T) {
	input := &KubeNeuron{
		Spec: KubeNeuronSpec{
			Agent: AgentSpec{
				NodeSelector: map[string]string{},
				Tolerations:  []corev1.Toleration{},
			},
		},
		Status: KubeNeuronStatus{Conditions: []metav1.Condition{}},
	}
	destination := KubeNeuron{
		Spec: KubeNeuronSpec{
			Agent: AgentSpec{
				NodeSelector: map[string]string{"stale": "value"},
			},
		},
	}

	input.DeepCopyInto(&destination)
	if destination.Spec.Agent.NodeSelector == nil || len(destination.Spec.Agent.NodeSelector) != 0 {
		t.Fatalf("DeepCopyInto() node selector = %#v, want non-nil empty map", destination.Spec.Agent.NodeSelector)
	}
	if destination.Status.Conditions == nil || len(destination.Status.Conditions) != 0 {
		t.Fatalf("DeepCopyInto() conditions = %#v, want non-nil empty slice", destination.Status.Conditions)
	}

	copied := input.DeepCopy()
	copied.Spec.Agent.NodeSelector["new"] = "value"
	if len(input.Spec.Agent.NodeSelector) != 0 {
		t.Fatalf("DeepCopy() shares node selector with input: %#v", input.Spec.Agent.NodeSelector)
	}
}

func TestAcceleratorRuntimeProfileDeepCopyPreservesNestedPolicy(t *testing.T) {
	input := &AcceleratorRuntimeProfile{
		Spec: AcceleratorRuntimeProfileSpec{
			KubeNeuronRef: "platform",
			NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				"accelerator": "nvidia-h100",
			}},
			Vendor:        AcceleratorRuntimeVendorNVIDIA,
			ProfileDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			MaxReportAge:  metav1.Duration{Duration: 10 * time.Minute},
			AllowedActions: []AcceleratorRuntimeActionPolicy{{
				Action: AcceleratorRuntimeActionResetDevice,
				Scopes: []AcceleratorRuntimeScope{
					AcceleratorRuntimeScopePhysicalDevice,
				},
				RequireVerifiedUnpartitionedTopology: true,
			}},
		},
		Status: AcceleratorRuntimeProfileStatus{Conditions: []metav1.Condition{}},
	}

	copied := input.DeepCopy()
	copied.Spec.NodeSelector.MatchLabels["pool"] = "a"
	copied.Spec.AllowedActions[0].Scopes[0] = AcceleratorRuntimeScopeNode
	if _, ok := input.Spec.NodeSelector.MatchLabels["pool"]; ok {
		t.Fatalf("DeepCopy() shares node selector: %#v", input.Spec.NodeSelector.MatchLabels)
	}
	if got := input.Spec.AllowedActions[0].Scopes[0]; got != AcceleratorRuntimeScopePhysicalDevice {
		t.Fatalf("DeepCopy() shares policy scopes: %q", got)
	}
	if copied.Status.Conditions == nil {
		t.Fatal("DeepCopy() lost non-nil empty conditions")
	}
}

func TestGeneratedAcceleratorRuntimeProfileCRDIsFailClosed(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/kubeneuron.io_acceleratorruntimeprofiles.yaml")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	for _, want := range []string{
		"kind: AcceleratorRuntimeProfile",
		"plural: acceleratorruntimeprofiles",
		"- nvidia",
		"pattern: ^sha256:[a-f0-9]{64}$",
		"format: duration",
		"message: maxReportAge must be a positive duration",
		"message: nodeSelector.matchLabels is required and cannot select every",
		"message: nodeSelector supports matchLabels only",
		"message: reset-device only supports physical-device scope and",
		"requires requireVerifiedUnpartitionedTopology=true",
		"message: requireVerifiedUnpartitionedTopology is only valid for",
		"x-kubernetes-list-map-keys:",
		"- action",
		"- physical-device",
		"- partition",
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("generated AcceleratorRuntimeProfile CRD is missing %q", want)
		}
	}
	for _, field := range []string{"driverVersion", "kubeNeuronRef", "maxReportAge", "nodeSelector", "profileDigest", "vendor"} {
		if !strings.Contains(schema, "            - "+field) {
			t.Errorf("generated AcceleratorRuntimeProfile CRD does not require %q", field)
		}
	}
	if strings.Contains(schema, "executionMode:") {
		t.Fatal("AcceleratorRuntimeProfile must not introduce an executionMode field")
	}
}

func TestGeneratedKubeNeuronCRDContainsFailClosedTransitionRules(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/kubeneuron.io_kubeneurons.yaml")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	for _, want := range []string{
		"message: sqlite settings are required for SQLite",
		"message: size must be a positive Kubernetes quantity",
		"quantity(self).compareTo(quantity('0'))",
		"message: size cannot decrease",
		"quantity(self.size).compareTo(quantity(oldSelf.size))",
		"message: workflow store type is immutable",
		"message: storageClassName is immutable",
		"message: victoriaMetrics currently supports only External mode",
		"message: alertmanager currently supports only External mode",
		"message: clickHouse currently supports only Disabled mode",
		"message: Disabled dependencies cannot set endpoint or secretRef",
		"message: serverSecretRef, clientCASecretRef, clientSecretRef, and",
		"serverCASecretRef are required",
		"message: TLS key-pair Secret references cannot select one key",
		"message: TLS Secret references must omit namespace and use spec.namespace",
		"message: executionMode Enabled is disabled until",
		"notifications.webhookToken is required",
		"notifications.operatorAPIToken is required for Paused mode",
		"operatorAPIToken",
		"webhookToken",
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("generated KubeNeuron CRD is missing %q", want)
		}
	}
	if !strings.Contains(schema, "required:\n                    - size") {
		t.Error("generated KubeNeuron CRD does not require the defaulted SQLite size")
	}
	if !strings.Contains(schema, "            - tls") {
		t.Error("generated KubeNeuron CRD does not require TLS configuration")
	}
	for _, field := range []string{"clientCASecretRef", "clientSecretRef", "serverCASecretRef", "serverSecretRef"} {
		if !strings.Contains(schema, "                - "+field) {
			t.Errorf("generated KubeNeuron CRD does not structurally require TLS field %q", field)
		}
	}
	if got := strings.Count(schema, "message: TLS Secret references must omit namespace and use spec.namespace"); got != 1 {
		t.Errorf("generated KubeNeuron CRD has %d TLS namespace rules, want 1", got)
	}
}
