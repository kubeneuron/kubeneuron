// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"context"

	api "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *KubeNeuronReconciler) ensureInventorySummary(ctx context.Context, installation *api.KubeNeuron) error {
	meta := metav1.ObjectMeta{Name: installation.Name + "-inventory-summary", Namespace: installation.Spec.Namespace}
	cm := &corev1.ConfigMap{ObjectMeta: meta}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if err := ensureControlledBy(installation, cm, "ConfigMap"); err != nil {
			return err
		}
		// Only the controller writes Data. Reconcile metadata without clearing its report.
		cm.Labels = resourceLabels(installation, "inventory-summary")
		return setOwner(r.Scheme, installation, cm)
	})
	if err != nil {
		return err
	}
	role := &rbacv1.Role{ObjectMeta: meta}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		if err := ensureControlledBy(installation, role, "Role"); err != nil {
			return err
		}
		role.Labels = resourceLabels(installation, "inventory-summary")
		role.Rules = []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{meta.Name}, Verbs: []string{"get", "update", "patch"}}}
		return setOwner(r.Scheme, installation, role)
	})
	if err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: meta}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		if err := ensureControlledBy(installation, binding, "RoleBinding"); err != nil {
			return err
		}
		binding.Labels = resourceLabels(installation, "inventory-summary")
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: meta.Name}
		binding.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: installation.Name + "-controller", Namespace: meta.Namespace}}
		return setOwner(r.Scheme, installation, binding)
	})
	return err
}
