// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"context"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/inventory"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *KubeNeuronReconciler) ensureInventorySummary(ctx context.Context, installation *api.KubeNeuron) error {
	meta := metav1.ObjectMeta{Name: installation.Name + inventory.Suffix, Namespace: installation.Spec.Namespace}
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
	return r.ensureConfigMapAccess(ctx, installation, meta, "inventory-summary", meta.Name, []string{"get", "update", "patch"})
}

// ensureRuntimeConfigAccess lets the controller read the runtime ConfigMap
// through the API. The copy the kubelet mounts can lag what was published
// by a minute or more, and steps wait until the configuration the
// controller loaded is the one published.
func (r *KubeNeuronReconciler) ensureRuntimeConfigAccess(ctx context.Context, installation *api.KubeNeuron) error {
	meta := metav1.ObjectMeta{Name: installation.Name + RuntimeConfigSuffix + "-reader", Namespace: installation.Spec.Namespace}
	return r.ensureConfigMapAccess(ctx, installation, meta, "runtime-config-reader", installation.Name+RuntimeConfigSuffix, []string{"get"})
}

func (r *KubeNeuronReconciler) ensureAutoscalerStatusAccess(ctx context.Context, installation *api.KubeNeuron) error {
	meta := metav1.ObjectMeta{Name: installation.Name + "-autoscaler-status", Namespace: metav1.NamespaceSystem}
	if installation.Spec.Cloud != nil && installation.Spec.Cloud.Provider == "aws" {
		return r.ensureConfigMapAccess(ctx, installation, meta, "autoscaler-status", "cluster-autoscaler-status", []string{"get"})
	}
	for _, obj := range []client.Object{&rbacv1.RoleBinding{}, &rbacv1.Role{}} {
		err := r.Get(ctx, client.ObjectKey{Namespace: meta.Namespace, Name: meta.Name}, obj)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if metav1.IsControlledBy(obj, installation) {
			uid := obj.GetUID()
			if err := client.IgnoreNotFound(r.Delete(ctx, obj, client.Preconditions{UID: &uid})); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *KubeNeuronReconciler) ensureConfigMapAccess(ctx context.Context, installation *api.KubeNeuron, meta metav1.ObjectMeta, component, configMap string, verbs []string) error {
	role := &rbacv1.Role{ObjectMeta: meta}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		if err := ensureControlledBy(installation, role, "Role"); err != nil {
			return err
		}
		role.Labels = resourceLabels(installation, component)
		role.Rules = []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{configMap}, Verbs: verbs}}
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
		binding.Labels = resourceLabels(installation, component)
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: meta.Name}
		binding.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: installation.Name + "-controller", Namespace: installation.Spec.Namespace}}
		return setOwner(r.Scheme, installation, binding)
	})
	return err
}

func (r *KubeNeuronReconciler) setInventoryConditions(ctx context.Context, root *api.KubeNeuron, nodeActionsEnabled bool) {
	if !nodeActionsEnabled {
		apimeta.RemoveStatusCondition(&root.Status.Conditions, "ResetAvailable")
	}
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: root.Spec.Namespace, Name: root.Name + inventory.Suffix}
	err := r.Get(ctx, key, &cm)
	now := time.Now()
	record := func(kind string, status metav1.ConditionStatus, reason, message string) {
		if kind == "ResetAvailable" && !nodeActionsEnabled {
			return
		}
		apimeta.SetStatusCondition(&root.Status.Conditions, metav1.Condition{
			Type: kind, Status: status, Reason: reason, Message: message + " See docs/quickstart.md#check-gpu-visibility.",
			ObservedGeneration: root.Generation,
		})
	}
	unknown := func(reason, message string) {
		record("GPUVisibility", metav1.ConditionUnknown, reason, message)
		record("ResetAvailable", metav1.ConditionUnknown, reason, message)
	}
	const poolWarning = "EKSFixedSizeUndeclared"
	if err != nil || !metav1.IsControlledBy(&cm, root) || cm.Data["schemaVersion"] == "" {
		apimeta.RemoveStatusCondition(&root.Status.Conditions, poolWarning)
	}
	switch {
	case apierrors.IsNotFound(err):
		unknown("AwaitingController", "Controller has not reported yet.")
		return
	case err != nil || !metav1.IsControlledBy(&cm, root):
		unknown("SummaryUnavailable", "Could not read an installation-owned inventory summary.")
		return
	case cm.Data["schemaVersion"] == "":
		// Empty, or holding only keys outside the inventory schema such as
		// the first accepted alert.
		unknown("AwaitingController", "Controller has not reported yet.")
		return
	}
	s, err := inventory.Parse(cm.Data, now)
	if err == nil && now.Sub(s.ObservedAt) <= 3*inventory.Interval && s.UndeclaredEKSPool != "" {
		apimeta.SetStatusCondition(&root.Status.Conditions, metav1.Condition{
			Type: poolWarning, Status: metav1.ConditionTrue, Reason: "FixedSizeDeclarationMissing",
			ObservedGeneration: root.Generation, Message: fmt.Sprintf("EKS node group %s has no fixed-size declaration. Set kubeneuron.io/fixed-size=true only if nothing else changes its size, or choose RemoveFirst. Run kubeneuronctl pools to see all affected groups.", s.UndeclaredEKSPool),
		})
	} else {
		apimeta.RemoveStatusCondition(&root.Status.Conditions, poolWarning)
	}
	switch {
	case err != nil:
		unknown("InvalidSummary", "Controller inventory summary is invalid.")
	case now.Sub(s.ObservedAt) > 3*inventory.Interval:
		unknown("StaleSummary", "Controller inventory summary is stale.")
	case s.ReportingNodes == 0 && s.Diagnostic != "":
		unknown(s.Diagnostic, inventory.DiagnosticText(s))
	case s.ReportingNodes == 0 && s.AdvertisedNodes > 0:
		unknown("AwaitingGPUAgents", "Kubernetes advertises accelerator resources, but no agent has a fresh report.")
	case s.ReportingNodes == 0:
		unknown("NoFreshAgentReport", "No fresh agent inventory report is available.")
	default:
		switch {
		case inventory.AgentRuntimeClassDiagnostic(s.Diagnostic):
			// Some agents report, others cannot run: what they report is
			// not the whole fleet.
			record("GPUVisibility", metav1.ConditionFalse, s.Diagnostic, inventory.DiagnosticText(s))
		case s.GPUs > 0:
			record("GPUVisibility", metav1.ConditionTrue, "InventoryReported", fmt.Sprintf("Agents on %d nodes report %d GPUs.", s.ReportingNodes, s.GPUs))
		case s.AdvertisedNodes == 0:
			record("GPUVisibility", metav1.ConditionFalse, "NoGPUResource", "Agents report zero GPUs and no Kubernetes node advertises an accelerator resource.")
		case s.Diagnostic != "":
			record("GPUVisibility", metav1.ConditionFalse, s.Diagnostic, inventory.DiagnosticText(s))
		default:
			record("GPUVisibility", metav1.ConditionFalse, "NoGPUsReported", "Agents report zero GPUs. Check the driver and nvidia-smi on the selected nodes.")
		}
		if s.ResetConfirmed {
			record("ResetAvailable", metav1.ConditionTrue, "ResetAttested", "At least one NVIDIA GPU has a fresh reset attestation.")
		} else {
			record("ResetAvailable", metav1.ConditionFalse, "ResetNotAttested", "No NVIDIA GPU has a fresh reset attestation. Check the DCGM client and runtime profile.")
		}
	}
}
