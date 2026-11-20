// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"reflect"
	"testing"
	"time"

	api "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/inventory"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"

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

func TestInventoryConditionsRejectStaleMalformedAndMissingEvidence(t *testing.T) {
	root := testKubeNeuron()
	root.UID = "installation-a"
	scheme := newOperatorScheme(t)
	r := &KubeNeuronReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}
	ctx := t.Context()
	if err := r.ensureInventorySummary(ctx, root); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: root.Spec.Namespace, Name: root.Name + inventory.Suffix}
	check := func(data map[string]string, reason string) {
		t.Helper()
		var cm corev1.ConfigMap
		if err := r.Get(ctx, key, &cm); err != nil {
			t.Fatal(err)
		}
		cm.Data = data
		if err := r.Update(ctx, &cm); err != nil {
			t.Fatal(err)
		}
		r.setInventoryConditions(ctx, root, true)
		for _, kind := range []string{"GPUVisibility", "ResetAvailable"} {
			c := meta.FindStatusCondition(root.Status.Conditions, kind)
			if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != reason {
				t.Fatalf("%s: %+v, want Unknown/%s", kind, c, reason)
			}
		}
	}
	check(inventory.Summary{ObservedAt: time.Now(), UndeclaredEKSPool: "train"}.Data(), "NoFreshAgentReport")
	if c := meta.FindStatusCondition(root.Status.Conditions, "EKSFixedSizeUndeclared"); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "FixedSizeDeclarationMissing" {
		t.Fatalf("missing EKS pool warning: %+v", c)
	}
	check(inventory.Summary{ObservedAt: time.Now()}.Data(), "NoFreshAgentReport")
	if meta.FindStatusCondition(root.Status.Conditions, "EKSFixedSizeUndeclared") != nil {
		t.Fatal("resolved declaration warning persisted")
	}
	check(nil, "AwaitingController")
	check(map[string]string{inventory.FirstAlertAcceptedAt: time.Now().UTC().Format(time.RFC3339Nano)}, "AwaitingController")
	check(map[string]string{"schemaVersion": "99"}, "InvalidSummary")
	check(inventory.Summary{ObservedAt: time.Now().Add(-4 * inventory.Interval)}.Data(), "StaleSummary")
	check(inventory.Summary{ObservedAt: time.Now(), AdvertisedNodes: 1}.Data(), "AwaitingGPUAgents")
	check(inventory.Summary{ObservedAt: time.Now(), Diagnostic: "SMIMissing"}.Data(), "SMIMissing")
	r.setInventoryConditions(ctx, root, false)
	if meta.FindStatusCondition(root.Status.Conditions, "ResetAvailable") != nil || meta.FindStatusCondition(root.Status.Conditions, "GPUVisibility") == nil {
		t.Fatal("disabling node actions should remove only ResetAvailable")
	}
}

func TestAutoscalerStatusAccess(t *testing.T) {
	ctx := t.Context()
	root := testKubeNeuron()
	root.UID = "installation-a"
	root.Spec.Cloud = &api.CloudSpec{Provider: "aws"}
	scheme := newOperatorScheme(t)
	r := &KubeNeuronReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}
	if err := r.ensureAutoscalerStatusAccess(ctx, root); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "kube-system", Name: root.Name + "-autoscaler-status"}
	var role rbacv1.Role
	want := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"cluster-autoscaler-status"}, Verbs: []string{"get"}}}
	if err := r.Get(ctx, key, &role); err != nil || !reflect.DeepEqual(role.Rules, want) {
		t.Fatalf("role %v: %+v", err, role.Rules)
	}
	var binding rbacv1.RoleBinding
	subjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: root.Name + "-controller", Namespace: root.Spec.Namespace}}
	if err := r.Get(ctx, key, &binding); err != nil || binding.RoleRef.Name != role.Name || !reflect.DeepEqual(binding.Subjects, subjects) {
		t.Fatalf("binding %v: %+v", err, binding)
	}
	root.Spec.Cloud = nil
	if err := r.ensureAutoscalerStatusAccess(ctx, root); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []client.Object{&role, &binding} {
		if err := r.Get(ctx, key, obj); !apierrors.IsNotFound(err) {
			t.Fatalf("expected removal of %T: %v", obj, err)
		}
	}
}
