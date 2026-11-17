// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

const (
	collectorReadyCondition = "ObservabilityCollectorReady"
	storeReadyCondition     = "ObservabilityStoreReady"
)

// observabilityCondition is one observability status condition to write;
// a nil condition removes the type, for a block that is Disabled.
type observabilityCondition struct {
	conditionType string
	condition     *metav1.Condition
}

func applyObservabilityConditions(installation *kubeneuronv1alpha1.KubeNeuron, conditions []observabilityCondition) {
	for _, c := range conditions {
		if c.condition == nil {
			meta.RemoveStatusCondition(&installation.Status.Conditions, c.conditionType)
			continue
		}
		condition := *c.condition
		condition.Type = c.conditionType
		condition.ObservedGeneration = installation.Generation
		condition.LastTransitionTime = metav1.Now()
		meta.SetStatusCondition(&installation.Status.Conditions, condition)
	}
}

func (r *KubeNeuronReconciler) collectorImage() string {
	if r.CollectorImage != "" {
		return r.CollectorImage
	}
	return DefaultCollectorImage
}

func (r *KubeNeuronReconciler) metricsStoreImage() string {
	if r.MetricsStoreImage != "" {
		return r.MetricsStoreImage
	}
	return DefaultMetricsStoreImage
}

// reconcileObservability runs or removes the managed collector and store and
// reports their readiness. Neither affects the installation's Ready
// condition: remediation does not depend on metrics.
func (r *KubeNeuronReconciler) reconcileObservability(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) ([]observabilityCondition, error) {
	var conditions []observabilityCondition
	// The store first, so a new collector's first export finds it.
	switch storeMode(installation) {
	case kubeneuronv1alpha1.IntegrationModeManaged:
		if err := r.ensureStore(ctx, installation); err != nil {
			return nil, err
		}
		condition, err := r.storeReady(ctx, installation)
		if err != nil {
			return nil, err
		}
		conditions = append(conditions, observabilityCondition{storeReadyCondition, condition})
	default:
		if err := r.removeStore(ctx, installation); err != nil {
			return nil, err
		}
		condition := externalOrDisabled(storeMode(installation), "metrics store")
		if condition == nil && installation.Spec.Observability.VictoriaMetrics != nil {
			condition = notReady("DeprecatedVictoriaMetrics",
				"victoriaMetrics is deprecated and is not used as the metrics destination; set spec.observability.store")
		}
		conditions = append(conditions, observabilityCondition{storeReadyCondition, condition})
	}
	switch collectorMode(installation) {
	case kubeneuronv1alpha1.IntegrationModeManaged:
		dcgm, err := r.ensureCollector(ctx, installation)
		if err != nil {
			return nil, err
		}
		condition, err := r.deploymentCondition(ctx, installation, collectorName(installation), "collector")
		if err != nil {
			return nil, err
		}
		conditions = append(conditions, observabilityCondition{collectorReadyCondition, condition})
		var gpu *metav1.Condition
		if dcgmScrapeEnabled(installation) {
			gpu = gpuMetricsStatus(installation, dcgm)
		}
		conditions = append(conditions, observabilityCondition{gpuMetricsCondition, gpu})
	default:
		if err := r.removeCollector(ctx, installation); err != nil {
			return nil, err
		}
		conditions = append(conditions,
			observabilityCondition{collectorReadyCondition, externalOrDisabled(collectorMode(installation), "collector")},
			observabilityCondition{gpuMetricsCondition, nil})
	}
	return conditions, nil
}

// externalOrDisabled is the condition of a block the operator does not run:
// an external endpoint is the user's to keep available, and a disabled
// block has no condition at all.
func externalOrDisabled(mode kubeneuronv1alpha1.IntegrationMode, what string) *metav1.Condition {
	if mode != kubeneuronv1alpha1.IntegrationModeExternal {
		return nil
	}
	return &metav1.Condition{
		Status:  metav1.ConditionTrue,
		Reason:  "External",
		Message: "the " + what + " is external; the operator does not check its availability",
	}
}

// ensureCollector runs the managed collector and returns the dcgm-exporter
// it was set to scrape, nil when scraping is off.
func (r *KubeNeuronReconciler) ensureCollector(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) (*dcgmDiscovery, error) {
	var dcgm *dcgmDiscovery
	if dcgmScrapeEnabled(installation) {
		var err error
		if dcgm, err = r.discoverDCGMExporter(ctx, installation); err != nil {
			return nil, err
		}
		targets, err := dcgmTargetsConfigMap(installation, dcgm)
		if err != nil {
			return nil, err
		}
		if err := r.ensureConfigMap(ctx, installation, targets); err != nil {
			return nil, fmt.Errorf("reconcile collector targets ConfigMap: %w", err)
		}
	} else if err := r.removeOwned(ctx, installation, &corev1.ConfigMap{}, dcgmTargetsName(installation)); err != nil {
		return nil, fmt.Errorf("remove collector targets ConfigMap: %w", err)
	}
	configMap, err := collectorConfigMap(installation)
	if err != nil {
		return nil, err
	}
	if err := r.ensureConfigMap(ctx, installation, configMap); err != nil {
		return nil, fmt.Errorf("reconcile collector ConfigMap: %w", err)
	}
	if err := r.ensureServiceAccount(ctx, installation, observabilityServiceAccount(installation, collectorName(installation), collectorComponent)); err != nil {
		return nil, fmt.Errorf("reconcile collector ServiceAccount: %w", err)
	}
	policy, err := collectorNetworkPolicy(installation, r.OperatorNamespace, dcgm)
	if err != nil {
		return nil, err
	}
	if err := r.ensureNetworkPolicy(ctx, installation, policy); err != nil {
		return nil, fmt.Errorf("reconcile collector NetworkPolicy: %w", err)
	}
	if err := r.ensureService(ctx, installation, collectorService(installation)); err != nil {
		return nil, fmt.Errorf("reconcile collector Service: %w", err)
	}
	// The digest rolls the collector when its configuration changes.
	digest := sha256Hex([]byte(configMap.Data["config.yaml"]))
	if err := r.ensureDeployment(ctx, installation, collectorDeployment(installation, r.collectorImage(), digest)); err != nil {
		return nil, fmt.Errorf("reconcile collector Deployment: %w", err)
	}
	return dcgm, nil
}

func (r *KubeNeuronReconciler) removeCollector(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) error {
	name := collectorName(installation)
	for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &networkingv1.NetworkPolicy{}, &corev1.ServiceAccount{}, &corev1.ConfigMap{}} {
		if err := r.removeOwned(ctx, installation, object, name); err != nil {
			return fmt.Errorf("remove collector: %w", err)
		}
	}
	if err := r.removeOwned(ctx, installation, &corev1.ConfigMap{}, dcgmTargetsName(installation)); err != nil {
		return fmt.Errorf("remove collector: %w", err)
	}
	return nil
}

func (r *KubeNeuronReconciler) ensureStore(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) error {
	if err := r.ensureServiceAccount(ctx, installation, observabilityServiceAccount(installation, storeName(installation), storeComponent)); err != nil {
		return fmt.Errorf("reconcile metrics store ServiceAccount: %w", err)
	}
	if !storeEphemeral(effectiveStore(installation)) {
		claim, err := storePVC(installation)
		if err != nil {
			return err
		}
		if err := r.ensureStoreClaim(ctx, installation, claim); err != nil {
			return fmt.Errorf("reconcile metrics store PersistentVolumeClaim: %w", err)
		}
	}
	if err := r.ensureNetworkPolicy(ctx, installation, storeNetworkPolicy(installation)); err != nil {
		return fmt.Errorf("reconcile metrics store NetworkPolicy: %w", err)
	}
	if err := r.ensureService(ctx, installation, storeService(installation)); err != nil {
		return fmt.Errorf("reconcile metrics store Service: %w", err)
	}
	if err := r.ensureDeployment(ctx, installation, storeDeployment(installation, r.metricsStoreImage())); err != nil {
		return fmt.Errorf("reconcile metrics store Deployment: %w", err)
	}
	return nil
}

// removeStore removes the store's workload when it is no longer Managed.
// Its PersistentVolumeClaim stays: the operator never deletes it.
func (r *KubeNeuronReconciler) removeStore(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) error {
	name := storeName(installation)
	for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &networkingv1.NetworkPolicy{}, &corev1.ServiceAccount{}} {
		if err := r.removeOwned(ctx, installation, object, name); err != nil {
			return fmt.Errorf("remove metrics store: %w", err)
		}
	}
	return nil
}

// removeOwned deletes the named object in the installation namespace when
// this installation controls it. An object someone else owns is left alone
// and reported; absence is success.
func (r *KubeNeuronReconciler) removeOwned(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, object client.Object, name string) error {
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: installation.Spec.Namespace}, object)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(object, installation) {
		return fmt.Errorf("%T %s exists but is not controlled by this installation; refusing to delete it", object, name)
	}
	return client.IgnoreNotFound(r.Delete(ctx, object))
}

func (r *KubeNeuronReconciler) ensureNetworkPolicy(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, wanted *networkingv1.NetworkPolicy) error {
	actual := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name, Namespace: wanted.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if err := ensureControlledBy(installation, actual, "NetworkPolicy"); err != nil {
			return err
		}
		actual.Labels = copyStringMap(wanted.Labels)
		actual.Spec = *wanted.Spec.DeepCopy()
		return setOwner(r.Scheme, installation, actual)
	})
	return err
}

// ensureStoreClaim creates the store's claim and afterwards only grows it:
// access mode, volume mode and storage class are fixed at creation. The
// claim has no owner reference, so neither garbage collection nor the
// operator ever deletes the metrics: it outlives the installation and is
// removed by hand. It is recognised as this installation's by its labels.
func (r *KubeNeuronReconciler) ensureStoreClaim(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, wanted *corev1.PersistentVolumeClaim) error {
	actual := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name, Namespace: wanted.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if actual.DeletionTimestamp != nil {
			return fmt.Errorf("PersistentVolumeClaim %s is being deleted", actual.Name)
		}
		if actual.ResourceVersion == "" {
			actual.Labels = copyStringMap(wanted.Labels)
			actual.Spec = *wanted.Spec.DeepCopy()
			return nil
		}
		for key, value := range wanted.Labels {
			if actual.Labels[key] != value {
				return fmt.Errorf("PersistentVolumeClaim %s/%s already exists and is not this installation's metrics store claim (label %s)",
					actual.Namespace, actual.Name, key)
			}
		}
		// Claims made before the store stopped being owned carry this
		// installation's owner reference; drop it so deleting the
		// installation does not delete the metrics. Any other owner is
		// someone else's claim.
		var owners []metav1.OwnerReference
		for _, ref := range actual.OwnerReferences {
			if ref.UID == installation.UID {
				continue
			}
			owners = append(owners, ref)
		}
		if len(owners) != 0 {
			return fmt.Errorf("PersistentVolumeClaim %s/%s has owners other than this installation; refusing to adopt it", actual.Namespace, actual.Name)
		}
		actual.OwnerReferences = nil
		if wanted.Spec.StorageClassName != nil &&
			(actual.Spec.StorageClassName == nil || *actual.Spec.StorageClassName != *wanted.Spec.StorageClassName) {
			return fmt.Errorf("spec.observability.store.managed.storage.storageClassName cannot change on the existing claim %s", actual.Name)
		}
		current := actual.Spec.Resources.Requests[corev1.ResourceStorage]
		size := wanted.Spec.Resources.Requests[corev1.ResourceStorage]
		switch size.Cmp(current) {
		case -1:
			return fmt.Errorf("spec.observability.store.managed.storage.size cannot shrink the existing claim %s from %s to %s",
				actual.Name, current.String(), size.String())
		case 1:
			if actual.Spec.Resources.Requests == nil {
				actual.Spec.Resources.Requests = corev1.ResourceList{}
			}
			actual.Spec.Resources.Requests[corev1.ResourceStorage] = size
		}
		return nil
	})
	return err
}

func (r *KubeNeuronReconciler) storeReady(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) (*metav1.Condition, error) {
	if !storeEphemeral(effectiveStore(installation)) {
		var claim corev1.PersistentVolumeClaim
		err := r.Get(ctx, types.NamespacedName{Name: storeName(installation), Namespace: installation.Spec.Namespace}, &claim)
		if apierrors.IsNotFound(err) {
			return notReady("StoragePending", "the metrics store's persistent volume claim was not found"), nil
		}
		if err != nil {
			return nil, err
		}
		if claim.Status.Phase != corev1.ClaimBound {
			phase := claim.Status.Phase
			if phase == "" {
				phase = corev1.ClaimPending
			}
			return notReady("StoragePending", fmt.Sprintf(
				"the metrics store's persistent volume claim %s is %s, not Bound; check that a default StorageClass exists or set spec.observability.store.managed.storage.storageClassName",
				claim.Name, phase)), nil
		}
	}
	return r.deploymentCondition(ctx, installation, storeName(installation), "metrics store")
}

func notReady(reason, message string) *metav1.Condition {
	return &metav1.Condition{Status: metav1.ConditionFalse, Reason: reason, Message: message}
}

// deploymentCondition reports whether a managed single-replica Deployment is
// available at its current generation.
func (r *KubeNeuronReconciler) deploymentCondition(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, name, what string) (*metav1.Condition, error) {
	var deployment appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: installation.Spec.Namespace}, &deployment)
	if apierrors.IsNotFound(err) {
		return notReady("NotReady", "the "+what+" deployment was not found"), nil
	}
	if err != nil {
		return nil, err
	}
	if ready, message := managedObjectReady(installation, &deployment, "the "+what+" deployment"); !ready {
		return notReady("NotReady", message), nil
	}
	if deployment.Status.ObservedGeneration < deployment.Generation ||
		deployment.Status.UpdatedReplicas < 1 || deployment.Status.AvailableReplicas < 1 {
		return notReady("NotReady", "the "+what+" deployment is not available at its current generation"), nil
	}
	return &metav1.Condition{Status: metav1.ConditionTrue, Reason: "Available", Message: "the " + what + " is available"}, nil
}
