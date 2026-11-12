// SPDX-License-Identifier: LicenseRef-KubeNeuron

package operator

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	rbacv1 "k8s.io/api/rbac/v1"
	sigsyaml "sigs.k8s.io/yaml"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/config"
)

func draEnabled() *kubeneuronv1alpha1.TopologyClassifierSpec {
	return &kubeneuronv1alpha1.TopologyClassifierSpec{DRA: &kubeneuronv1alpha1.TopologyClassifierDRASpec{Enabled: true}}
}

// The topology classifier has no switch; only its DRA read is opt-in. An
// omitted block, an empty block and an explicit dra.enabled=false must all
// compile to nothing — and to the SAME digest as an installation that never
// heard of the field — so no controller starts reading resource.k8s.io as a
// side effect of an upgrade.
func TestCompileSnapshotOmitsTopologyClassifierUnlessDRAEnabled(t *testing.T) {
	compile := func(t *testing.T, spec *kubeneuronv1alpha1.TopologyClassifierSpec) (*config.TopologyClassifier, string, string) {
		t.Helper()
		installation := testKubeNeuron()
		installation.Spec.Safety.TopologyClassifier = spec
		policies, playbooks := testPolicyFixture()
		snapshot, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
		if err != nil {
			t.Fatalf("CompileSnapshot() error = %v", err)
		}
		var compiled config.Config
		if err := yaml.Unmarshal(snapshot.PoliciesYAML, &compiled); err != nil {
			t.Fatalf("unmarshal compiled policies: %v", err)
		}
		return compiled.Safety.TopologyClassifier, string(snapshot.PoliciesYAML), snapshot.Digest
	}

	tc, yamlText, baseline := compile(t, nil)
	if tc != nil || strings.Contains(yamlText, "topology_classifier") {
		t.Fatalf("an omitted block must leave no trace in the snapshot:\n%s", yamlText)
	}
	for name, spec := range map[string]*kubeneuronv1alpha1.TopologyClassifierSpec{
		"empty block":  {},
		"dra omitted":  {DRA: nil},
		"dra disabled": {DRA: &kubeneuronv1alpha1.TopologyClassifierDRASpec{Enabled: false}},
	} {
		tc, yamlText, digest := compile(t, spec)
		if tc != nil || strings.Contains(yamlText, "topology_classifier") || strings.Contains(yamlText, "dra") {
			t.Fatalf("%s must leave no trace in the snapshot:\n%s", name, yamlText)
		}
		if digest != baseline {
			t.Fatalf("%s must compile to the same digest as an absent block", name)
		}
	}

	tc, yamlText, enabledDigest := compile(t, draEnabled())
	if tc == nil || !tc.DRA.Enabled {
		t.Fatalf("compiled topology classifier = %+v, want DRA enabled", tc)
	}
	if !strings.Contains(yamlText, "topology_classifier:") || !strings.Contains(yamlText, "enabled: true") {
		t.Fatalf("enabled DRA discovery left no key in the snapshot:\n%s", yamlText)
	}
	if enabledDigest == baseline {
		t.Fatal("enabling DRA discovery must change the digest so the controller rolls")
	}
	if topologyDRADiscoveryEnabled(testKubeNeuron()) {
		t.Fatal("the RBAC predicate must be off by default")
	}
	enabled := testKubeNeuron()
	enabled.Spec.Safety.TopologyClassifier = draEnabled()
	if !topologyDRADiscoveryEnabled(enabled) {
		t.Fatal("the RBAC predicate must follow dra.enabled")
	}
}

var draReadOnlyRule = rbacv1.PolicyRule{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceslices"}, Verbs: []string{"get", "list", "watch"}}

func draRules(rules []rbacv1.PolicyRule) []rbacv1.PolicyRule {
	var out []rbacv1.PolicyRule
	for _, rule := range rules {
		if containsString(rule.APIGroups, "resource.k8s.io") || containsString(rule.APIGroups, "*") {
			out = append(out, rule)
		}
	}
	return out
}

// TestControllerRBACDRARuleFollowsTopologyClassifierPolicy pins the one
// privilege DRA discovery costs: a read-only resourceslices rule on the
// managed controller ClusterRole, held ONLY by an installation that
// explicitly enabled spec.safety.topologyClassifier.dra. Every other shape
// holds no rule on the group at all, and nothing else about the role changes.
func TestControllerRBACDRARuleFollowsTopologyClassifierPolicy(t *testing.T) {
	baseline := controllerClusterRole(testKubeNeuron())
	if rules := draRules(baseline.Rules); len(rules) != 0 {
		t.Fatalf("default controller ClusterRole holds a resource.k8s.io rule: %#v", rules)
	}
	for name, spec := range map[string]*kubeneuronv1alpha1.TopologyClassifierSpec{
		"omitted":      nil,
		"empty":        {},
		"dra disabled": {DRA: &kubeneuronv1alpha1.TopologyClassifierDRASpec{Enabled: false}},
	} {
		installation := testKubeNeuron()
		installation.Spec.Safety.TopologyClassifier = spec
		role := controllerClusterRole(installation)
		if !reflect.DeepEqual(role.Rules, baseline.Rules) {
			t.Fatalf("%s: controller ClusterRole differs from the default:\n got %#v\nwant %#v", name, role.Rules, baseline.Rules)
		}
	}

	installation := testKubeNeuron()
	installation.Spec.Safety.TopologyClassifier = draEnabled()
	role := controllerClusterRole(installation)
	if len(role.Rules) != len(baseline.Rules)+1 {
		t.Fatalf("enabled ClusterRole has %d rules, want %d: exactly one DRA rule is added", len(role.Rules), len(baseline.Rules)+1)
	}
	for i, rule := range baseline.Rules {
		if !reflect.DeepEqual(role.Rules[i], rule) {
			t.Fatalf("rule %d changed under DRA discovery: %#v, want %#v", i, role.Rules[i], rule)
		}
	}
	got := draRules(role.Rules)
	if len(got) != 1 || !reflect.DeepEqual(got[0], draReadOnlyRule) {
		t.Fatalf("DRA rules = %#v, want exactly %#v", got, draReadOnlyRule)
	}
	// The DRA rule is the last rule and the only thing the policy adds, so the
	// checkpoint pod-verb policy and this one compose without touching each
	// other.
	installation.Spec.Safety.CheckpointCoordination = &kubeneuronv1alpha1.CheckpointCoordinationSpec{Enabled: true, Namespaces: []string{"ml"}}
	both := controllerClusterRole(installation)
	if !reflect.DeepEqual(draRules(both.Rules), got) || len(both.Rules) != len(role.Rules) {
		t.Fatalf("checkpoint policy changed the DRA grant: %#v", both.Rules)
	}
}

// TestDRAGrantsAreReadOnlyEverywhere is the least-privilege pin for Dynamic
// Resource Allocation across the whole deliverable: the static operator role
// holds the read-only resourceslices rule ONLY because RBAC escalation
// prevention requires the operator to hold what it delegates to the managed
// controller role; no role, chart or generated rule anywhere grants a write
// verb, a wildcard, or any other resource.k8s.io resource.
func TestDRAGrantsAreReadOnlyEverywhere(t *testing.T) {
	readOnly := map[string]bool{"get": true, "list": true, "watch": true}
	assertReadOnly := func(t *testing.T, where string, rules []rbacv1.PolicyRule) {
		t.Helper()
		for _, rule := range rules {
			if containsString(rule.APIGroups, "*") {
				t.Errorf("%s: wildcard apiGroup rule %#v", where, rule)
			}
			if !containsString(rule.APIGroups, "resource.k8s.io") {
				continue
			}
			if !reflect.DeepEqual(rule.APIGroups, []string{"resource.k8s.io"}) || !reflect.DeepEqual(rule.Resources, []string{"resourceslices"}) {
				t.Errorf("%s: resource.k8s.io rule must name only resourceslices: %#v", where, rule)
			}
			for _, verb := range rule.Verbs {
				if !readOnly[verb] {
					t.Errorf("%s: resource.k8s.io rule grants non-read verb %q: %#v", where, verb, rule)
				}
			}
			if len(rule.ResourceNames) != 0 || len(rule.NonResourceURLs) != 0 {
				t.Errorf("%s: resource.k8s.io rule has unexpected selectors: %#v", where, rule)
			}
		}
	}

	// The static operator role: holds exactly the delegation rule, read-only.
	static, err := os.ReadFile("../../config/rbac/operator_role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var operatorRole rbacv1.ClusterRole
	if err := sigsyaml.Unmarshal(static, &operatorRole); err != nil {
		t.Fatal(err)
	}
	assertReadOnly(t, "config/rbac/operator_role.yaml", operatorRole.Rules)
	if got := draRules(operatorRole.Rules); len(got) != 1 || !reflect.DeepEqual(got[0], draReadOnlyRule) {
		t.Fatalf("operator ClusterRole DRA rules = %#v, want exactly the delegation rule %#v", got, draReadOnlyRule)
	}
	// ...and that rule is necessary: it is exactly what the managed
	// controller role contains when discovery is enabled, which the operator
	// could not grant without holding it.
	enabled := testKubeNeuron()
	enabled.Spec.Safety.TopologyClassifier = draEnabled()
	for _, wanted := range draRules(controllerClusterRole(enabled).Rules) {
		if !rulesCover(operatorRole.Rules, wanted) {
			t.Errorf("operator ClusterRole cannot delegate %#v", wanted)
		}
	}
	assertReadOnly(t, "controllerClusterRole(enabled)", controllerClusterRole(enabled).Rules)
	assertReadOnly(t, "controllerClusterRole(default)", controllerClusterRole(testKubeNeuron()).Rules)

	// Every static RBAC document in the repo, and the Helm operator role.
	files, err := filepath.Glob("../../config/rbac/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob config/rbac: %v (%d files)", err, len(files))
	}
	helm, err := os.ReadFile("../../deploy/helm/kubeneuron/templates/rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range strings.Split(string(data), "\n---\n") {
			var role struct {
				Rules []rbacv1.PolicyRule `json:"rules"`
			}
			if err := sigsyaml.Unmarshal([]byte(doc), &role); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			assertReadOnly(t, file, role.Rules)
		}
	}
	// The chart's rules blocks are plain YAML by construction (see
	// TestHelmOperatorRoleMirrorsStaticRBAC); parse every one of them.
	for _, doc := range strings.Split(string(helm), "\n---\n") {
		idx := strings.Index(doc, "\nrules:\n")
		if idx < 0 {
			continue
		}
		block := doc[idx+1:]
		if strings.Contains(block, "{{") {
			continue
		}
		var role struct {
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		if err := sigsyaml.UnmarshalStrict([]byte(block), &role); err != nil {
			t.Fatalf("helm rbac.yaml rules block: %v", err)
		}
		assertReadOnly(t, "deploy/helm/kubeneuron/templates/rbac.yaml", role.Rules)
	}

	// Grep-style: wherever the group is spelled in deployable material or in
	// the operator's generator, no write verb, wildcard or other DRA resource
	// is spelled within reach of it.
	forbidden := []string{`"create"`, `"update"`, `"patch"`, `"delete"`, `"deletecollection"`, `"escalate"`, `"bind"`, `"impersonate"`, `"*"`,
		"- create", "- update", "- patch", "- delete", "- escalate", "- bind", "- impersonate", `- "*"`, "resourceclaims", "deviceclasses", "resourceclaimtemplates"}
	sources := append([]string{"../../deploy/helm/kubeneuron/templates/rbac.yaml"}, files...)
	generator, err := filepath.Glob("../../internal/operator/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range generator {
		if !strings.HasSuffix(file, "_test.go") {
			sources = append(sources, file)
		}
	}
	for _, file := range sources {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if !strings.Contains(line, "resource.k8s.io") {
				continue
			}
			lo, hi := max(0, i-3), min(len(lines), i+4)
			window := strings.Join(lines[lo:hi], "\n")
			for _, word := range forbidden {
				if strings.Contains(window, word) {
					t.Errorf("%s:%d: %q within reach of resource.k8s.io:\n%s", file, i+1, word, window)
				}
			}
		}
	}
}

// TestDRADiscoveryDetectsClaimsThroughTheExistingPodRead pins the other half
// of the least-privilege design: ResourceClaim use is detected from the
// spec.resourceClaims of the Pods on a node, which the core pods
// get/list/watch rule already covers, so enabling discovery adds no rule
// naming resourceclaims, resourceclaimtemplates or deviceclasses in any
// group, and does not change the pod verbs at all.
func TestDRADiscoveryDetectsClaimsThroughTheExistingPodRead(t *testing.T) {
	claimResources := map[string]bool{"resourceclaims": true, "resourceclaimtemplates": true, "deviceclasses": true, "resourceclaims/status": true}
	podsRule := func(rules []rbacv1.PolicyRule) *rbacv1.PolicyRule {
		for i := range rules {
			if containsString(rules[i].APIGroups, "") && containsString(rules[i].Resources, "pods") {
				return &rules[i]
			}
		}
		return nil
	}
	enabled := testKubeNeuron()
	enabled.Spec.Safety.TopologyClassifier = draEnabled()
	for name, installation := range map[string]*kubeneuronv1alpha1.KubeNeuron{"default": testKubeNeuron(), "dra enabled": enabled} {
		role := controllerClusterRole(installation)
		for _, rule := range role.Rules {
			for _, resource := range rule.Resources {
				if claimResources[strings.ToLower(resource)] || resource == "*" {
					t.Errorf("%s: controller ClusterRole grants %q: %#v", name, resource, rule)
				}
			}
		}
		pods := podsRule(role.Rules)
		if pods == nil {
			t.Fatalf("%s: controller ClusterRole has no core pods rule", name)
		}
		for _, verb := range []string{"get", "list", "watch"} {
			if !containsString(pods.Verbs, verb) {
				t.Errorf("%s: core pods rule lacks %q, which claim detection relies on: %#v", name, verb, pods)
			}
		}
	}
	if !reflect.DeepEqual(podsRule(controllerClusterRole(enabled).Rules), podsRule(controllerClusterRole(testKubeNeuron()).Rules)) {
		t.Fatal("DRA discovery changed the core pods rule")
	}
}

var contextReadOnlyRules = []rbacv1.PolicyRule{
	{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get", "list", "watch"}},
	{APIGroups: []string{"kueue.x-k8s.io"}, Resources: []string{"workloads", "localqueues", "clusterqueues"}, Verbs: []string{"get", "list", "watch"}},
	{APIGroups: []string{"ray.io"}, Resources: []string{"rayclusters", "rayjobs"}, Verbs: []string{"get", "list", "watch"}},
}

func contextRules(rules []rbacv1.PolicyRule) []rbacv1.PolicyRule {
	var out []rbacv1.PolicyRule
	for _, rule := range rules {
		if containsString(rule.APIGroups, "batch") || containsString(rule.APIGroups, "kueue.x-k8s.io") || containsString(rule.APIGroups, "ray.io") {
			out = append(out, rule)
		}
	}
	return out
}

// Each resolver's read-only grant is present exactly while the preview is
// enabled and that resolver selected; nothing else in the role moves.
func TestControllerRBACContextRulesFollowImpactPreviewPolicy(t *testing.T) {
	baseline := controllerClusterRole(testKubeNeuron())
	if got := contextRules(baseline.Rules); len(got) != 0 {
		t.Fatalf("default controller ClusterRole holds a context rule: %#v", got)
	}
	for name, spec := range map[string]*kubeneuronv1alpha1.ImpactPreviewSpec{
		"omitted":                  nil,
		"enabled, no context":      {Enabled: true},
		"disabled with context":    {Enabled: false, WorkloadContext: &kubeneuronv1alpha1.ImpactWorkloadContextSpec{BatchJobs: true, Kueue: true, KubeRay: true}},
		"enabled, context all off": {Enabled: true, WorkloadContext: &kubeneuronv1alpha1.ImpactWorkloadContextSpec{}},
	} {
		installation := testKubeNeuron()
		installation.Spec.Safety.ImpactPreview = spec
		if role := controllerClusterRole(installation); !reflect.DeepEqual(role.Rules, baseline.Rules) {
			t.Fatalf("%s: controller ClusterRole differs from the default:\n got %#v\nwant %#v", name, role.Rules, baseline.Rules)
		}
	}
	for i, selection := range []kubeneuronv1alpha1.ImpactWorkloadContextSpec{{BatchJobs: true}, {Kueue: true}, {KubeRay: true}} {
		installation := testKubeNeuron()
		installation.Spec.Safety.ImpactPreview = &kubeneuronv1alpha1.ImpactPreviewSpec{Enabled: true, WorkloadContext: &selection}
		role := controllerClusterRole(installation)
		got := contextRules(role.Rules)
		if len(got) != 1 || !reflect.DeepEqual(got[0], contextReadOnlyRules[i]) {
			t.Fatalf("selection %+v: context rules = %#v, want exactly %#v", selection, got, contextReadOnlyRules[i])
		}
		if len(role.Rules) != len(baseline.Rules)+1 {
			t.Fatalf("selection %+v: %d rules, want %d", selection, len(role.Rules), len(baseline.Rules)+1)
		}
		for j, rule := range baseline.Rules {
			if !reflect.DeepEqual(role.Rules[j], rule) {
				t.Fatalf("rule %d changed under a context selection: %#v", j, role.Rules[j])
			}
		}
	}
	installation := testKubeNeuron()
	installation.Spec.Safety.ImpactPreview = &kubeneuronv1alpha1.ImpactPreviewSpec{Enabled: true, WorkloadContext: &kubeneuronv1alpha1.ImpactWorkloadContextSpec{BatchJobs: true, Kueue: true, KubeRay: true}}
	all := controllerClusterRole(installation)
	if got := contextRules(all.Rules); !reflect.DeepEqual(got, contextReadOnlyRules) {
		t.Fatalf("all selected: %#v", got)
	}
}

// No write verb on batch, Kueue or Ray objects exists anywhere: the managed
// role under every selection, the static operator role (which must hold the
// rules to delegate them) and the Helm mirror.
func TestContextGrantsAreReadOnlyEverywhere(t *testing.T) {
	readOnly := map[string]bool{"get": true, "list": true, "watch": true}
	assertReadOnly := func(t *testing.T, where string, rules []rbacv1.PolicyRule) {
		t.Helper()
		for _, rule := range contextRules(rules) {
			for _, verb := range rule.Verbs {
				if !readOnly[verb] {
					t.Errorf("%s: rule grants non-read verb %q: %#v", where, verb, rule)
				}
			}
			if containsString(rule.Resources, "*") || containsString(rule.APIGroups, "*") {
				t.Errorf("%s: wildcard in context rule %#v", where, rule)
			}
		}
	}
	static, err := os.ReadFile("../../config/rbac/operator_role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var operatorRole rbacv1.ClusterRole
	if err := sigsyaml.Unmarshal(static, &operatorRole); err != nil {
		t.Fatal(err)
	}
	assertReadOnly(t, "config/rbac/operator_role.yaml", operatorRole.Rules)
	installation := testKubeNeuron()
	installation.Spec.Safety.ImpactPreview = &kubeneuronv1alpha1.ImpactPreviewSpec{Enabled: true, WorkloadContext: &kubeneuronv1alpha1.ImpactWorkloadContextSpec{BatchJobs: true, Kueue: true, KubeRay: true}}
	enabled := controllerClusterRole(installation)
	assertReadOnly(t, "controllerClusterRole(all)", enabled.Rules)
	for _, wanted := range contextRules(enabled.Rules) {
		if !rulesCover(operatorRole.Rules, wanted) {
			t.Errorf("operator ClusterRole cannot delegate %#v", wanted)
		}
	}
	helm, err := os.ReadFile("../../deploy/helm/kubeneuron/templates/rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{`apiGroups: ["batch"]`, `apiGroups: ["kueue.x-k8s.io"]`, `apiGroups: ["ray.io"]`} {
		if !strings.Contains(string(helm), group) {
			t.Errorf("helm rbac.yaml lacks the delegation rule for %s", group)
		}
	}
	for _, forbidden := range []string{`apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get", "list", "watch", "patch"]`, `apiGroups: ["kueue.x-k8s.io"]
    resources: ["workloads", "localqueues", "clusterqueues"]
    verbs: ["get", "list", "watch", "patch"]`} {
		if strings.Contains(string(helm), forbidden) {
			t.Errorf("helm rbac.yaml grants a write verb on a context group")
		}
	}
}
