// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"strings"
	"testing"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func azureInstallation() *kubeneuronv1alpha1.KubeNeuron {
	installation := testKubeNeuron()
	installation.UID = "installation-a"
	installation.Spec.Cloud = &kubeneuronv1alpha1.CloudSpec{Provider: "azure", Azure: &kubeneuronv1alpha1.AzureCloudSpec{
		SubscriptionID: "11111111-2222-3333-4444-555555555555", ResourceGroup: "ml", ClusterName: "training",
		ClientID: "66666666-7777-8888-9999-000000000000",
	}}
	return installation
}

func TestControllerAzureWiring(t *testing.T) {
	installation := azureInstallation()
	deployment, err := controllerDeployment(installation, &Snapshot{Digest: "d"})
	if err != nil {
		t.Fatal(err)
	}
	pod := deployment.Spec.Template
	if pod.Labels["azure.workload.identity/use"] != "true" {
		t.Fatal("controller pod is not marked for Azure Workload Identity")
	}
	if _, inSelector := deployment.Spec.Selector.MatchLabels["azure.workload.identity/use"]; inSelector {
		t.Fatal("the workload identity label must not enter the immutable selector")
	}
	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		"KUBENEURON_AKS_SUBSCRIPTION_ID": "11111111-2222-3333-4444-555555555555",
		"KUBENEURON_AKS_RESOURCE_GROUP":  "ml",
		"KUBENEURON_AKS_CLUSTER":         "training",
	} {
		if env[name] != want {
			t.Fatalf("%s = %q, want %q", name, env[name], want)
		}
	}
	if !containsString(pod.Spec.Containers[0].Args, "--cloud-provider=azure") {
		t.Fatalf("controller args %v miss the provider", pod.Spec.Containers[0].Args)
	}
	sa := controllerServiceAccount(installation)
	if sa.Annotations["azure.workload.identity/client-id"] != "66666666-7777-8888-9999-000000000000" || sa.Annotations["azure.workload.identity/tenant-id"] != "" {
		t.Fatalf("SA annotations = %v", sa.Annotations)
	}
	condition := cloudRecoveryCondition(installation)
	if condition.Reason != "NotReadyOnlyRepair" || condition.Status != metav1.ConditionUnknown || !strings.Contains(condition.Message, "experimental, not validated on accelerator hardware; start in DryRun") {
		t.Fatalf("condition = %+v", condition)
	}
}

// The controller may list events only in default, only for Azure, and the
// grant is emptied, never left behind, when the provider changes.
func TestNodeEventsAccessOnlyForAzure(t *testing.T) {
	ctx := t.Context()
	scheme := newOperatorScheme(t)
	r := &KubeNeuronReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}
	key := client.ObjectKey{Namespace: "default", Name: "kube-neuron-controller-node-events"}

	plain := testKubeNeuron()
	plain.UID = "installation-a"
	key.Name = plain.Name + "-controller-node-events"
	if err := r.ensureNodeEventsAccess(ctx, plain); err != nil {
		t.Fatal(err)
	}
	var role rbacv1.Role
	if err := r.Get(ctx, key, &role); !apierrors.IsNotFound(err) {
		t.Fatalf("a provider without event needs got a Role: %v", err)
	}

	installation := azureInstallation()
	if err := r.ensureNodeEventsAccess(ctx, installation); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, &role); err != nil || !metav1.IsControlledBy(&role, installation) {
		t.Fatalf("role: %v owners=%v", err, role.OwnerReferences)
	}
	want := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"list"}}
	if len(role.Rules) != 1 || !rulesCover(role.Rules, want) || len(role.Rules[0].Verbs) != 1 {
		t.Fatalf("rules = %+v", role.Rules)
	}
	var binding rbacv1.RoleBinding
	if err := r.Get(ctx, key, &binding); err != nil || len(binding.Subjects) != 1 || binding.Subjects[0].Name != installation.Name+"-controller" || binding.Subjects[0].Namespace != installation.Spec.Namespace {
		t.Fatalf("binding: %v %+v", err, binding.Subjects)
	}

	installation.Spec.Cloud = nil
	if err := r.ensureNodeEventsAccess(ctx, installation); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, &role); err != nil || len(role.Rules) != 0 {
		t.Fatalf("grant left after leaving Azure: %v %+v", err, role.Rules)
	}
}
