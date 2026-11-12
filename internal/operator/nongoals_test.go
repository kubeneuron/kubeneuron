// SPDX-License-Identifier: LicenseRef-KubeNeuron

package operator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	sigsyaml "sigs.k8s.io/yaml"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

// v0.8 non-goals, pinned on every role this repository ships or generates:
// no network or process path from the controller into a workload. That
// means no pods/exec, pods/attach, pods/portforward or
// pods/ephemeralcontainers in any rule, under any combination of the v0.8
// opt-ins, and no wildcard that would quietly include them.
var forbiddenPodSubresources = []string{"pods/exec", "pods/attach", "pods/portforward", "pods/ephemeralcontainers", "pods/proxy"}

func assertNoWorkloadPath(t *testing.T, where string, rules []rbacv1.PolicyRule) {
	t.Helper()
	for _, rule := range rules {
		for _, res := range rule.Resources {
			if res == "*" {
				t.Errorf("%s: wildcard resource rule %#v would include pod subresources", where, rule)
			}
			for _, bad := range forbiddenPodSubresources {
				if res == bad {
					t.Errorf("%s: rule grants %s: %#v", where, bad, rule)
				}
			}
		}
		for _, verb := range rule.Verbs {
			if verb == "*" {
				t.Errorf("%s: wildcard verb rule %#v", where, rule)
			}
		}
	}
}

// everyOptIn is a KubeNeuron with every v0.8 block switched on at once, the
// widest ClusterRole the operator can generate.
func everyOptIn() *kubeneuronv1alpha1.KubeNeuron {
	kn := testKubeNeuron()
	kn.Spec.Safety.TopologyClassifier = draEnabled()
	kn.Spec.Safety.CheckpointCoordination = &kubeneuronv1alpha1.CheckpointCoordinationSpec{
		Enabled:    true,
		Namespaces: []string{"ml-training"},
		Advisory:   &kubeneuronv1alpha1.CheckpointAdvisorySpec{Enabled: true},
		Groups:     &kubeneuronv1alpha1.CheckpointGroupsSpec{Enabled: true, CrossNode: true},
	}
	kn.Spec.Safety.ImpactPreview = &kubeneuronv1alpha1.ImpactPreviewSpec{
		Enabled:            true,
		RequireForApproval: true,
		WorkloadContext:    &kubeneuronv1alpha1.ImpactWorkloadContextSpec{BatchJobs: true, Kueue: true, KubeRay: true},
	}
	return kn
}

func TestNoRoleOpensAPathIntoAWorkload(t *testing.T) {
	assertNoWorkloadPath(t, "controllerClusterRole(default)", controllerClusterRole(testKubeNeuron()).Rules)
	assertNoWorkloadPath(t, "controllerClusterRole(every opt-in)", controllerClusterRole(everyOptIn()).Rules)

	files, err := filepath.Glob("../../config/rbac/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob config/rbac: %v (%d files)", err, len(files))
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
			assertNoWorkloadPath(t, file, role.Rules)
		}
	}
	// The chart is a template; the subresource names are literal text in
	// any rule that would grant them, so a text search is exact here.
	helm, err := os.ReadFile("../../deploy/helm/kubeneuron/templates/rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range forbiddenPodSubresources {
		if strings.Contains(string(helm), bad) {
			t.Errorf("deploy/helm/kubeneuron/templates/rbac.yaml grants %s", bad)
		}
	}
}

// The widest generated role writes exactly one resource: pods (annotations,
// through the guarded patch), and only while checkpoint coordination is on.
// Everything v0.8 adds is read-only, so every other rule carries read verbs
// alone.
func TestWidestControllerRoleWritesOnlyPods(t *testing.T) {
	readOnly := map[string]bool{"get": true, "list": true, "watch": true}
	for _, rule := range controllerClusterRole(everyOptIn()).Rules {
		podsOnly := len(rule.APIGroups) == 1 && rule.APIGroups[0] == "" && len(rule.Resources) == 1 && rule.Resources[0] == "pods"
		for _, verb := range rule.Verbs {
			if readOnly[verb] {
				continue
			}
			if podsOnly && verb == "patch" {
				continue
			}
			// Pre-v0.8 write verbs (node cordon/drain, evictions, events,
			// leases, the operator-managed objects) are allowed on their own
			// resources; what this test forbids is a write verb on any of the
			// v0.8 groups.
			for _, group := range rule.APIGroups {
				switch group {
				case "resource.k8s.io", "batch", "kueue.x-k8s.io", "ray.io":
					t.Errorf("v0.8 group %s carries write verb %q: %#v", group, verb, rule)
				}
			}
		}
	}
	// And without coordination, pods is read-only again.
	kn := everyOptIn()
	kn.Spec.Safety.CheckpointCoordination = nil
	for _, rule := range controllerClusterRole(kn).Rules {
		if len(rule.Resources) == 1 && rule.Resources[0] == "pods" && len(rule.APIGroups) == 1 && rule.APIGroups[0] == "" {
			for _, verb := range rule.Verbs {
				if verb == "patch" {
					t.Errorf("pods: patch granted without checkpoint coordination: %#v", rule)
				}
			}
		}
	}
}
