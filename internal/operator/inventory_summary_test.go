// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestInventorySummaryPreservesReportAndScopesWriter(t *testing.T) {
	ctx := t.Context()
	scheme := newOperatorScheme(t)
	root := testKubeNeuron()
	root.UID = "installation-a"
	r := &KubeNeuronReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}
	if err := r.ensureInventorySummary(ctx, root); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Name: root.Name + "-inventory-summary", Namespace: root.Spec.Namespace}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, key, &cm); err != nil || !metav1.IsControlledBy(&cm, root) || len(cm.Data) != 0 {
		t.Fatalf("empty owned summary: err=%v owner=%v data=%v", err, cm.OwnerReferences, cm.Data)
	}
	cm.Data = map[string]string{"schemaVersion": "1"}
	if err := r.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureInventorySummary(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, &cm); err != nil || cm.Data["schemaVersion"] != "1" {
		t.Fatalf("reconcile lost controller report: %v %v", err, cm.Data)
	}
	var role rbacv1.Role
	want := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{key.Name}, Verbs: []string{"get", "update", "patch"}}}
	if err := r.Get(ctx, key, &role); err != nil || !reflect.DeepEqual(role.Rules, want) {
		t.Fatalf("unexpected summary writer permissions: %v %+v", err, role.Rules)
	}
	var binding rbacv1.RoleBinding
	if err := r.Get(ctx, key, &binding); err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != key.Name || !reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: key.Namespace, Name: root.Name + "-controller"}}) {
		t.Fatalf("unexpected summary role binding: %+v %+v", binding.RoleRef, binding.Subjects)
	}
}
