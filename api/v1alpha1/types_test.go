package v1alpha1

import (
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sigyaml "sigs.k8s.io/yaml"
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
		// Enabled is no longer unreachable, but it is unreachable without a
		// declared set of nodes and a verbatim acknowledgement. Guard both
		// halves: dropping either would quietly re-open fleet-wide
		// destructive execution.
		"executionMode Enabled requires spec.safety.destructiveExecution",
		"message: acknowledgement text must match exactly",
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

// Checkpoint coordination is opt-in, allowlisted, and bounded. The generated
// schema is parsed rather than string-matched, because controller-gen folds
// long rules and messages across lines and doubles quote literals, and a
// formatting change must not be mistaken for a lost rule. What is asserted is
// the actual CEL, the actual defaults, and the actual structural shape.
func TestGeneratedKubeNeuronCRDCheckpointCoordinationIsOptInAllowlistedAndBounded(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/kubeneuron.io_kubeneurons.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := sigyaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("parse generated CRD: %v", err)
	}
	safety := schemaProperty(t, servedSchema(t, crd), "spec", "safety")
	if required, _ := safety["required"].([]any); slices.Contains(required, any("checkpointCoordination")) {
		t.Fatal("spec.safety must not require checkpointCoordination: an absent block is the default")
	}
	block := schemaProperty(t, safety, "checkpointCoordination")
	if block["type"] != "object" {
		t.Fatalf("checkpointCoordination type = %v, want object", block["type"])
	}
	if _, ok := block["required"]; ok {
		t.Errorf("checkpointCoordination must have no structurally required field, got required=%v", block["required"])
	}

	// Defaults: off, 5m/15m, and the device-dead classes skipped.
	for field, want := range map[string]any{
		"enabled":     false,
		"defaultWait": "5m",
		"maxWait":     "15m",
		"skipClasses": []any{"fell-off-bus", "gpu-lost"},
	} {
		if got := schemaProperty(t, block, field)["default"]; !reflect.DeepEqual(got, want) {
			t.Errorf("checkpointCoordination.%s default = %#v, want %#v", field, got, want)
		}
	}
	if _, ok := schemaProperty(t, block, "namespaces")["default"]; ok {
		t.Error("namespaces must have no default: the allowlist is a decision the operator makes explicitly")
	}

	// Both lists are sets of non-blank strings; namespaces are DNS labels.
	for _, field := range []string{"skipClasses", "namespaces"} {
		prop := schemaProperty(t, block, field)
		if prop["type"] != "array" || prop["x-kubernetes-list-type"] != "set" {
			t.Errorf("%s: type=%v listType=%v, want a string set", field, prop["type"], prop["x-kubernetes-list-type"])
		}
		items, _ := prop["items"].(map[string]any)
		if items == nil || items["type"] != "string" || !reflect.DeepEqual(items["minLength"], float64(1)) {
			t.Errorf("%s items = %v, want non-empty strings", field, prop["items"])
		}
	}
	namespaceItems := schemaProperty(t, block, "namespaces")["items"].(map[string]any)
	if namespaceItems["pattern"] != `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` || !reflect.DeepEqual(namespaceItems["maxLength"], float64(63)) {
		t.Errorf("namespaces items = %v, want DNS-label constrained", namespaceItems)
	}

	// The admission rules, by exact CEL: an enabled block without an
	// allowlist, a non-positive wait, a maxWait above the 30m ceiling, or a
	// defaultWait above maxWait must all be refused.
	rules := xValidations(t, block)
	for _, want := range []struct{ rule, message string }{
		{
			rule:    "!self.enabled || (has(self.namespaces) && self.namespaces.size() > 0)",
			message: "checkpointCoordination.enabled requires a non-empty namespaces allowlist",
		},
		{
			rule:    "!has(self.defaultWait) || duration(self.defaultWait) > duration('0s')",
			message: "defaultWait must be a positive duration",
		},
		{
			rule:    "!has(self.maxWait) || (duration(self.maxWait) > duration('0s') && duration(self.maxWait) <= duration('30m'))",
			message: "maxWait must be a positive duration of at most 30m",
		},
		{
			rule:    "!has(self.defaultWait) || !has(self.maxWait) || duration(self.defaultWait) <= duration(self.maxWait)",
			message: "defaultWait must not exceed maxWait",
		},
	} {
		message, ok := rules[want.rule]
		if !ok {
			t.Errorf("checkpointCoordination is missing x-kubernetes-validation rule %q; have %v", want.rule, slices.Sorted(maps.Keys(rules)))
			continue
		}
		if !strings.HasPrefix(message, want.message) {
			t.Errorf("rule %q has message %q, want prefix %q", want.rule, message, want.message)
		}
	}
	if len(rules) != 4 {
		t.Errorf("checkpointCoordination has %d x-kubernetes-validations, want exactly the 4 documented rules: %v", len(rules), slices.Sorted(maps.Keys(rules)))
	}
}

// servedSchema returns the openAPIV3Schema of the single served version of a
// parsed CRD document.
func servedSchema(t *testing.T, crd map[string]any) map[string]any {
	t.Helper()
	spec, _ := crd["spec"].(map[string]any)
	versions, _ := spec["versions"].([]any)
	var served []map[string]any
	for _, v := range versions {
		version, _ := v.(map[string]any)
		if version["served"] == true {
			served = append(served, version)
		}
	}
	if len(served) != 1 {
		t.Fatalf("CRD has %d served versions, want 1", len(served))
	}
	schema, _ := served[0]["schema"].(map[string]any)
	root, _ := schema["openAPIV3Schema"].(map[string]any)
	if root == nil {
		t.Fatal("served version has no openAPIV3Schema")
	}
	return root
}

// schemaProperty descends through nested object properties by name.
func schemaProperty(t *testing.T, schema map[string]any, path ...string) map[string]any {
	t.Helper()
	for _, name := range path {
		properties, _ := schema["properties"].(map[string]any)
		next, _ := properties[name].(map[string]any)
		if next == nil {
			t.Fatalf("schema has no property %q (path %v)", name, path)
		}
		schema = next
	}
	return schema
}

// xValidations returns a schema node's CEL rules keyed by rule text.
func xValidations(t *testing.T, schema map[string]any) map[string]string {
	t.Helper()
	raw, _ := schema["x-kubernetes-validations"].([]any)
	out := make(map[string]string, len(raw))
	for _, r := range raw {
		entry, _ := r.(map[string]any)
		rule, _ := entry["rule"].(string)
		message, _ := entry["message"].(string)
		if rule == "" {
			t.Fatalf("x-kubernetes-validations entry without a rule: %v", r)
		}
		out[rule] = message
	}
	return out
}
