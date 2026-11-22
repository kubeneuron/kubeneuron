// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"context"

	api "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// nodeEventsNamespace is where Kubernetes records events about nodes.
const nodeEventsNamespace = "default"

// nodeEventsRules lets the controller list node events, for a cloud provider
// that judges its own repair from them (AKS auto-repair). Only there, and only
// in the namespace node events live in.
func nodeEventsRules(installation *api.KubeNeuron) []rbacv1.PolicyRule {
	if azureCloud(installation) == nil {
		return nil
	}
	return []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"list"}}}
}

// ensureNodeEventsAccess grants the event read a provider needs and empties
// the grant once no provider needs it. Nothing is created for a provider that
// reads no events.
func (r *KubeNeuronReconciler) ensureNodeEventsAccess(ctx context.Context, installation *api.KubeNeuron) error {
	meta := metav1.ObjectMeta{Name: installation.Name + "-controller-node-events", Namespace: nodeEventsNamespace}
	rules := nodeEventsRules(installation)
	if rules == nil {
		var existing rbacv1.Role
		err := r.Get(ctx, client.ObjectKey{Namespace: meta.Namespace, Name: meta.Name}, &existing)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	role := &rbacv1.Role{ObjectMeta: meta}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		if err := ensureControlledBy(installation, role, "Role"); err != nil {
			return err
		}
		role.Labels = resourceLabels(installation, "controller")
		role.Rules = rules
		return setOwner(r.Scheme, installation, role)
	}); err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: meta}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		if err := ensureControlledBy(installation, binding, "RoleBinding"); err != nil {
			return err
		}
		binding.Labels = resourceLabels(installation, "controller")
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: meta.Name}
		binding.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: installation.Name + "-controller", Namespace: installation.Spec.Namespace}}
		return setOwner(r.Scheme, installation, binding)
	})
	return err
}
