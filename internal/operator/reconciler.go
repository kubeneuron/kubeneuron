package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
)

const (
	reconcileRetry = 30 * time.Second
	// pkiCheckInterval is how often a settled installation re-examines its TLS
	// material. Certificates are renewed with a third of their life remaining —
	// 30 days on a 90-day certificate — so checking twice a day is frequent
	// enough to be irrelevant to the margin and rare enough to cost nothing.
	pkiCheckInterval                 = 12 * time.Hour
	deploymentSystemAnnotationPrefix = "deployment.kubernetes.io/"
	daemonSetSystemAnnotationPrefix  = "deprecated.daemonset."
)

// KubeNeuronReconciler owns the managed runtime objects for a root
// KubeNeuron resource. It intentionally does not create VictoriaMetrics,
// ClickHouse, or PostgreSQL deployments: their dedicated upstream operators
// own lifecycle, storage, backups, and upgrades.
type KubeNeuronReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	// Recorder emits Kubernetes Events on the KubeNeuron object; nil (tests)
	// disables event emission.
	Recorder record.EventRecorder
}

// event emits a Kubernetes Event when a recorder is wired.
func (r *KubeNeuronReconciler) event(installation *kubeneuronv1alpha1.KubeNeuron, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(installation, eventType, reason, message)
}

// Reconcile compiles CRD configuration, publishes an immutable configuration
// snapshot, and converges the controller and agent workloads.
func (r *KubeNeuronReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	result, err := r.reconcile(ctx, req)
	if apierrors.IsConflict(err) {
		// Optimistic-concurrency conflicts are the normal cost of cached
		// reads with concurrent writers (kubelet status, GC, our own earlier
		// write not yet reflected in the cache). They self-heal on the next
		// pass; surfacing them as reconciler errors floods the error stream
		// on slow clusters and trips log-based test gates.
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return result, err
}

func (r *KubeNeuronReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var installation kubeneuronv1alpha1.KubeNeuron
	if err := r.Get(ctx, req.NamespacedName, &installation); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	policies, playbooks, mappings, maintenance, nodeConfigs, acceleratorProfiles, err := r.configuration(ctx)
	if err != nil {
		// Same rationale as the compile-failure path below: certificates expire on
		// a wall-clock deadline, so a transient failure to list child configuration
		// must not freeze leaf renewal. Renew best-effort, then report the failure.
		if _, pkiErr := r.ReconcilePKI(ctx, &installation, time.Now()); pkiErr != nil {
			r.event(&installation, corev1.EventTypeWarning, "ReconcileFailed",
				fmt.Sprintf("reconcile TLS material while configuration is unlistable: %v", pkiErr))
		}
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, nil, err)
	}
	snapshot, err := CompileSnapshot(&installation, policies, playbooks, mappings, maintenance, nodeConfigs, acceleratorProfiles)
	if err != nil {
		r.event(&installation, corev1.EventTypeWarning, "ConfigurationInvalid", err.Error())
		if statusErr := r.updateChildConfigurationStatuses(ctx, &installation, policies, playbooks, mappings, acceleratorProfiles, nil, err); statusErr != nil {
			err = errors.Join(err, fmt.Errorf("update child configuration statuses: %w", statusErr))
		}
		// PKI renewal must not depend on the configuration compiling. Certificates
		// expire on a wall-clock deadline regardless of whether the latest CRD edit
		// is valid, so freezing leaf renewal behind an inert config error (e.g. a
		// RecycleNode playbook with no spec.cloud) would take fleet mTLS down over a
		// mistake that never changed anything at runtime. Renew here, best-effort,
		// and still report the configuration failure as the reconcile outcome.
		if _, pkiErr := r.ReconcilePKI(ctx, &installation, time.Now()); pkiErr != nil {
			r.event(&installation, corev1.EventTypeWarning, "ReconcileFailed",
				fmt.Sprintf("reconcile TLS material while configuration is invalid: %v", pkiErr))
		}
		return ctrl.Result{RequeueAfter: reconcileRetry}, r.updateStatus(ctx, &installation, nil, err, false, "")
	}
	if snapshot.Digest != installation.Status.ConfigDigest {
		r.event(&installation, corev1.EventTypeNormal, "SnapshotPublished",
			"configuration snapshot "+snapshot.Digest+" compiled and rolling out")
	}
	if err := r.updateChildConfigurationStatuses(ctx, &installation, policies, playbooks, mappings, acceleratorProfiles, snapshot, nil); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("update child configuration statuses: %w", err))
	}

	if err := r.targetNamespaceExists(ctx, &installation); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, err)
	}

	// TLS material is settled before anything that mounts it is created or
	// rolled. Nothing renewed certificates before this existed, so an
	// installation stopped authenticating about a year after it was made.
	pkiResult, err := r.ReconcilePKI(ctx, &installation, time.Now())
	if err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("reconcile TLS material: %w", err))
	}

	if err := r.ensureConfigMap(ctx, &installation, runtimeConfigMap(&installation, snapshot)); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("reconcile runtime ConfigMap: %w", err))
	}
	if err := r.ensureConfigMap(ctx, &installation, playbooksConfigMap(&installation, snapshot)); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("reconcile playbooks ConfigMap: %w", err))
	}
	if err := r.ensureServiceAccount(ctx, &installation, controllerServiceAccount(&installation)); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("reconcile controller ServiceAccount: %w", err))
	}
	if err := r.ensureServiceAccount(ctx, &installation, agentServiceAccount(&installation)); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("reconcile agent ServiceAccount: %w", err))
	}
	if err := r.ensureClusterRole(ctx, &installation, controllerClusterRole(&installation)); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("reconcile controller ClusterRole: %w", err))
	}
	if err := r.ensureClusterRoleBinding(ctx, &installation, controllerClusterRoleBinding(&installation)); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("reconcile controller ClusterRoleBinding: %w", err))
	}
	if err := r.ensureService(ctx, &installation, controllerService(&installation)); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("reconcile controller Service: %w", err))
	}
	if installation.Spec.WorkflowStore.Type == "SQLite" {
		// Postgres installations have no state PVC: the database owns
		// durability and the controller becomes stateless.
		claim, err := controllerPVC(&installation)
		if err != nil {
			return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, err)
		}
		if err := r.ensurePersistentVolumeClaim(ctx, &installation, claim); err != nil {
			return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("reconcile controller PersistentVolumeClaim: %w", err))
		}
	}
	deployment, err := controllerDeployment(&installation, snapshot, pkiResult.Revisions.Controller)
	if err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, err)
	}
	if err := r.ensureDeployment(ctx, &installation, deployment); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("reconcile controller Deployment: %w", err))
	}
	if err := r.ensureDaemonSet(ctx, &installation, agentDaemonSet(&installation, snapshot, pkiResult.Revisions.Agent)); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("reconcile agent DaemonSet: %w", err))
	}
	// The two-DaemonSet era is retired: arming is controller-served data, one
	// DaemonSet covers the whole fleet, and the old "-agent-detect" companion
	// is removed on upgrade (only when owned by this installation).
	if err := r.removeOwnedDaemonSet(ctx, &installation, installation.Name+"-agent-detect"); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("remove retired detection agent DaemonSet: %w", err))
	}
	if err := r.ensurePDB(ctx, &installation, controllerPDB(&installation)); err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("reconcile controller PodDisruptionBudget: %w", err))
	}

	ready, readinessMessage, err := r.runtimeReady(ctx, &installation)
	if err != nil {
		return ctrl.Result{}, r.updateReconcileFailure(ctx, &installation, snapshot, fmt.Errorf("check runtime readiness: %w", err))
	}
	if err := r.updateStatus(ctx, &installation, snapshot, nil, ready, readinessMessage); err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: reconcileRetry}, nil
	}
	// Certificate expiry is a deadline, not an event: nothing in the cluster
	// changes when material gets closer to being unusable. A settled
	// installation therefore still has to wake up and look, or renewal would
	// never fire and the whole point of issuing the material would be lost.
	return ctrl.Result{RequeueAfter: pkiCheckInterval}, nil
}

func (r *KubeNeuronReconciler) configuration(ctx context.Context) (
	[]kubeneuronv1alpha1.GPURemediationPolicy,
	[]kubeneuronv1alpha1.GPUPlaybook,
	[]kubeneuronv1alpha1.GPUSignalMapping,
	[]kubeneuronv1alpha1.GPUMaintenanceWindow,
	[]kubeneuronv1alpha1.GPUNodeConfig,
	[]kubeneuronv1alpha1.AcceleratorRuntimeProfile,
	error,
) {
	var policies kubeneuronv1alpha1.GPURemediationPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("list GPURemediationPolicy resources: %w", err)
	}
	var playbooks kubeneuronv1alpha1.GPUPlaybookList
	if err := r.List(ctx, &playbooks); err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("list GPUPlaybook resources: %w", err)
	}
	var mappings kubeneuronv1alpha1.GPUSignalMappingList
	if err := r.List(ctx, &mappings); err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("list GPUSignalMapping resources: %w", err)
	}
	var maintenance kubeneuronv1alpha1.GPUMaintenanceWindowList
	if err := r.List(ctx, &maintenance); err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("list GPUMaintenanceWindow resources: %w", err)
	}
	var nodeConfigs kubeneuronv1alpha1.GPUNodeConfigList
	if err := r.List(ctx, &nodeConfigs); err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("list GPUNodeConfig resources: %w", err)
	}
	var acceleratorProfiles kubeneuronv1alpha1.AcceleratorRuntimeProfileList
	if err := r.List(ctx, &acceleratorProfiles); err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("list AcceleratorRuntimeProfile resources: %w", err)
	}
	return policies.Items, playbooks.Items, mappings.Items, maintenance.Items, nodeConfigs.Items, acceleratorProfiles.Items, nil
}

func (r *KubeNeuronReconciler) targetNamespaceExists(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) error {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var namespace corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: installation.Spec.Namespace}, &namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("target namespace %q does not exist; create it outside the KubeNeuron operator", installation.Spec.Namespace)
		}
		return fmt.Errorf("get target namespace %q: %w", installation.Spec.Namespace, err)
	}
	if namespace.DeletionTimestamp != nil || namespace.Status.Phase == corev1.NamespaceTerminating {
		return fmt.Errorf("target namespace %q is terminating", installation.Spec.Namespace)
	}
	return nil
}

func (r *KubeNeuronReconciler) runtimeReady(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) (bool, string, error) {
	if installation.Spec.WorkflowStore.Type == "SQLite" {
		if ready, message, err := r.sqliteClaimReady(ctx, installation); !ready || err != nil {
			return ready, message, err
		}
	}
	return r.workloadsReady(ctx, installation)
}

// sqliteClaimReady checks the state PVC that only SQLite installations own.
func (r *KubeNeuronReconciler) sqliteClaimReady(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) (bool, string, error) {
	var claim corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Name: installation.Name + "-controller-state", Namespace: installation.Spec.Namespace}, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "controller persistent volume claim not found", nil
		}
		return false, "controller persistent volume claim could not be read", err
	}
	if ready, message := managedObjectReady(installation, &claim, "controller persistent volume claim"); !ready {
		return false, message, nil
	}
	if claim.Status.Phase != corev1.ClaimBound {
		return false, fmt.Sprintf("controller persistent volume claim is %s, not Bound", claim.Status.Phase), nil
	}
	wantedStorage, err := sqliteStorageQuantity(installation.Spec.WorkflowStore)
	if err != nil {
		return false, "controller persistent volume claim has an invalid storage request", err
	}
	requestedStorage, ok := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok || requestedStorage.Cmp(wantedStorage) < 0 {
		return false, "controller persistent volume claim does not contain the requested storage size", nil
	}
	capacity, ok := claim.Status.Capacity[corev1.ResourceStorage]
	if !ok || capacity.Cmp(requestedStorage) < 0 {
		return false, "controller persistent volume claim resize is not complete", nil
	}
	for _, condition := range claim.Status.Conditions {
		if condition.Status != corev1.ConditionFalse {
			return false, fmt.Sprintf("controller persistent volume claim has active %s condition", condition.Type), nil
		}
	}
	if status := claim.Status.AllocatedResourceStatuses[corev1.ResourceStorage]; status != "" {
		return false, fmt.Sprintf("controller persistent volume claim resize status is %s", status), nil
	}
	return true, "", nil
}

// workloadsReady checks the controller Deployment and agent DaemonSet.
func (r *KubeNeuronReconciler) workloadsReady(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) (bool, string, error) {
	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: installation.Name + "-controller", Namespace: installation.Spec.Namespace}, &deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "controller deployment not found", nil
		}
		return false, "controller deployment could not be read", err
	}
	if ready, message := managedObjectReady(installation, &deployment, "controller deployment"); !ready {
		return false, message, nil
	}
	if deployment.Status.ObservedGeneration < deployment.Generation {
		return false, "controller deployment has not observed its current generation", nil
	}
	desiredReplicas := int32(1)
	if deployment.Spec.Replicas != nil {
		desiredReplicas = *deployment.Spec.Replicas
	}
	if desiredReplicas <= 0 {
		return false, "controller deployment has no desired replicas", nil
	}
	if deployment.Status.UpdatedReplicas != desiredReplicas ||
		deployment.Status.Replicas != desiredReplicas ||
		deployment.Status.AvailableReplicas < desiredReplicas ||
		deployment.Status.UnavailableReplicas != 0 {
		return false, "controller deployment is not fully available at its current generation", nil
	}
	var agents appsv1.DaemonSet
	if err := r.Get(ctx, types.NamespacedName{Name: installation.Name + "-agent", Namespace: installation.Spec.Namespace}, &agents); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "agent daemonset not found", nil
		}
		return false, "agent daemonset could not be read", err
	}
	if ready, message := managedObjectReady(installation, &agents, "agent daemonset"); !ready {
		return false, message, nil
	}
	if agents.Status.ObservedGeneration < agents.Generation {
		return false, "agent daemonset has not observed its current generation", nil
	}
	if agents.Status.DesiredNumberScheduled == 0 {
		return false, "agent daemonset has no desired pods", nil
	}
	desiredAgents := agents.Status.DesiredNumberScheduled
	if agents.Status.UpdatedNumberScheduled != desiredAgents ||
		agents.Status.CurrentNumberScheduled != desiredAgents ||
		agents.Status.NumberAvailable != desiredAgents ||
		agents.Status.NumberUnavailable != 0 ||
		agents.Status.NumberMisscheduled != 0 {
		return false, "agent daemonset is not fully available at its current generation", nil
	}

	return true, "runtime is available", nil
}

func managedObjectReady(installation *kubeneuronv1alpha1.KubeNeuron, object client.Object, description string) (bool, string) {
	if object.GetDeletionTimestamp() != nil {
		return false, description + " is terminating"
	}
	if !metav1.IsControlledBy(object, installation) {
		return false, description + " is not controlled by this KubeNeuron installation"
	}
	return true, ""
}

func (r *KubeNeuronReconciler) updateStatus(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, snapshot *Snapshot, validationErr error, runtimeReady bool, runtimeMessage string) error {
	before := installation.DeepCopy()
	installation.Status.ObservedGeneration = installation.Generation
	if validationErr != nil {
		meta.SetStatusCondition(&installation.Status.Conditions, metav1.Condition{
			Type:               "ConfigurationValid",
			Status:             metav1.ConditionFalse,
			Reason:             "ValidationFailed",
			Message:            validationErr.Error(),
			ObservedGeneration: installation.Generation,
			LastTransitionTime: metav1.Now(),
		})
		meta.SetStatusCondition(&installation.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "ConfigurationInvalid",
			Message:            "runtime resources are not updated until configuration is valid",
			ObservedGeneration: installation.Generation,
			LastTransitionTime: metav1.Now(),
		})
	} else {
		installation.Status.ConfigDigest = snapshot.Digest
		installation.Status.RuntimeConfigMap = installation.Name + RuntimeConfigSuffix
		installation.Status.PlaybooksConfigMap = installation.Name + PlaybooksConfigSuffix
		meta.SetStatusCondition(&installation.Status.Conditions, metav1.Condition{
			Type:               "ConfigurationValid",
			Status:             metav1.ConditionTrue,
			Reason:             "Compiled",
			Message:            "custom resources compiled into an immutable runtime snapshot",
			ObservedGeneration: installation.Generation,
			LastTransitionTime: metav1.Now(),
		})
		setTLSMaterialCondition(installation, nil)
		meta.SetStatusCondition(&installation.Status.Conditions, metav1.Condition{
			Type:               "DependenciesConfigured",
			Status:             metav1.ConditionTrue,
			Reason:             "ReferencesValidated",
			Message:            "external dependencies are declared; their upstream operators own readiness",
			ObservedGeneration: installation.Generation,
			LastTransitionTime: metav1.Now(),
		})
		status := metav1.ConditionFalse
		reason := "RuntimeUnavailable"
		message := runtimeMessage
		if message == "" {
			message = "controller or agents are not yet available"
		}
		if runtimeReady {
			status = metav1.ConditionTrue
			reason = "RuntimeAvailable"
		}
		meta.SetStatusCondition(&installation.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: installation.Generation,
			LastTransitionTime: metav1.Now(),
		})
	}
	if reflect.DeepEqual(before.Status, installation.Status) {
		return nil
	}
	return r.Status().Patch(ctx, installation, client.MergeFrom(before))
}

// setTLSMaterialCondition raises TLSMaterialValid=False when the failure is a
// CA that needs rotating, and clears it otherwise.
//
// It is its own condition because it is the one reconcile failure a human must
// act on, and the action is a documented procedure rather than a bug report.
// It also blocks EVERYTHING — no ConfigMap, Deployment or DaemonSet converges
// while it stands — for up to maxRenewalLead before the certificate actually
// expires. That is deliberate: replacing a trust root in place leaves a fleet
// that cannot mutually authenticate. But "Ready=False, reason
// ReconciliationFailed" is indistinguishable from a transient apiserver blip,
// so an operator could watch configuration changes silently fail to land for
// weeks with nothing to alert on.
func setTLSMaterialCondition(installation *kubeneuronv1alpha1.KubeNeuron, reconcileErr error) {
	var caErr *CARotationRequiredError
	if !errors.As(reconcileErr, &caErr) {
		// Healthy, or failing for some other reason: a completed rotation must
		// not leave its condition behind for somebody to alert on forever.
		meta.RemoveStatusCondition(&installation.Status.Conditions, "TLSMaterialValid")
		return
	}
	meta.SetStatusCondition(&installation.Status.Conditions, metav1.Condition{
		Type:   "TLSMaterialValid",
		Status: metav1.ConditionFalse,
		Reason: "CARotationRequired",
		Message: fmt.Sprintf("%s: %s. Nothing else in this installation will converge until it is rotated — "+
			"follow the expand/activate/retire procedure in docs/operations.md (TLS material and its renewal).",
			caErr.Secret, caErr.Reason),
		ObservedGeneration: installation.Generation,
		LastTransitionTime: metav1.Now(),
	})
}

func (r *KubeNeuronReconciler) updateReconcileFailure(
	ctx context.Context,
	installation *kubeneuronv1alpha1.KubeNeuron,
	snapshot *Snapshot,
	reconcileErr error,
) error {
	if apierrors.IsConflict(reconcileErr) {
		// A transient write conflict is not an installation failure: the
		// next pass retries with a fresh read. Flipping Ready/Configured
		// here would churn status on every busy cluster.
		return reconcileErr
	}
	r.event(installation, corev1.EventTypeWarning, "ReconcileFailed", reconcileErr.Error())
	before := installation.DeepCopy()
	installation.Status.ObservedGeneration = installation.Generation
	configurationStatus := metav1.ConditionUnknown
	configurationReason := "ConfigurationReadFailed"
	configurationMessage := "configuration could not be read and compiled"
	if snapshot != nil {
		configurationStatus = metav1.ConditionTrue
		configurationReason = "Compiled"
		configurationMessage = "custom resources compiled successfully; runtime reconciliation failed"
	}
	meta.SetStatusCondition(&installation.Status.Conditions, metav1.Condition{
		Type:               "ConfigurationValid",
		Status:             configurationStatus,
		Reason:             configurationReason,
		Message:            configurationMessage,
		ObservedGeneration: installation.Generation,
		LastTransitionTime: metav1.Now(),
	})
	meta.SetStatusCondition(&installation.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "ReconciliationFailed",
		Message:            reconcileErr.Error(),
		ObservedGeneration: installation.Generation,
		LastTransitionTime: metav1.Now(),
	})

	// A CA that needs rotating gets its own condition, because it is the one
	// reconcile failure a human must act on and the action is a documented
	// procedure rather than a bug report.
	//
	// It also blocks EVERYTHING — no ConfigMap, Deployment or DaemonSet
	// converges while it stands — for up to maxRenewalLead before the CA
	// actually expires. That is deliberate (a trust root replaced in place
	// leaves a fleet that cannot mutually authenticate), but "Ready=False,
	// reason ReconciliationFailed" is indistinguishable from a transient
	// apiserver blip, so an operator could watch configuration changes
	// silently not land for weeks. Naming it makes it alertable.
	setTLSMaterialCondition(installation, reconcileErr)

	if reflect.DeepEqual(before.Status, installation.Status) {
		return reconcileErr
	}
	if statusErr := r.Status().Patch(ctx, installation, client.MergeFrom(before)); statusErr != nil {
		return errors.Join(reconcileErr, fmt.Errorf("update KubeNeuron failure status: %w", statusErr))
	}
	return reconcileErr
}

// updateChildConfigurationStatuses makes child CR status a projection of the
// same compilation result used to produce the runtime snapshot. It never
// reports stale success for a new child generation: a failed installation
// compilation marks every participating playbook and policy invalid until a
// complete snapshot can be compiled again. AcceleratorRuntimeProfile status
// means only that its desired contract was compiled; it never claims runtime
// observation freshness or remediation eligibility.
func (r *KubeNeuronReconciler) updateChildConfigurationStatuses(
	ctx context.Context,
	installation *kubeneuronv1alpha1.KubeNeuron,
	policies []kubeneuronv1alpha1.GPURemediationPolicy,
	playbooks []kubeneuronv1alpha1.GPUPlaybook,
	mappings []kubeneuronv1alpha1.GPUSignalMapping,
	acceleratorProfiles []kubeneuronv1alpha1.AcceleratorRuntimeProfile,
	snapshot *Snapshot,
	validationErr error,
) error {
	var books map[string]*playbook.Playbook
	if validationErr == nil {
		var err error
		books, err = compilePlaybookIndex(installation.Name, playbooks)
		if err != nil {
			return fmt.Errorf("rebuild compiled playbook index for status: %w", err)
		}
	}

	for i := range playbooks {
		book := &playbooks[i]
		if book.Spec.KubeNeuronRef != installation.Name {
			continue
		}
		if err := r.updateGPUPlaybookStatus(ctx, book, snapshot, validationErr); err != nil {
			return err
		}
	}
	for i := range policies {
		policy := &policies[i]
		if policy.Spec.KubeNeuronRef != installation.Name {
			continue
		}
		if err := r.updateGPURemediationPolicyStatus(ctx, installation, policy, books, validationErr); err != nil {
			return err
		}
	}
	for i := range acceleratorProfiles {
		profile := &acceleratorProfiles[i]
		if profile.Spec.KubeNeuronRef != installation.Name {
			continue
		}
		if err := r.updateAcceleratorRuntimeProfileStatus(ctx, profile, validationErr); err != nil {
			return err
		}
	}
	for i := range mappings {
		mapping := &mappings[i]
		if mapping.Spec.KubeNeuronRef != installation.Name {
			continue
		}
		if err := r.updateGPUSignalMappingStatus(ctx, installation, mapping, validationErr); err != nil {
			return err
		}
	}
	return nil
}

// updateGPUSignalMappingStatus publishes whether a selected mapping joined the
// immutable runtime snapshot — the same generation-bound Ready contract the
// other child configuration kinds have had since they became consumable. A
// selected-but-invalid mapping already fails the whole installation closed;
// this is the positive half, so an operator (or a harness) can wait on the
// mapping itself instead of inferring from the root.
func (r *KubeNeuronReconciler) updateGPUSignalMappingStatus(
	ctx context.Context,
	installation *kubeneuronv1alpha1.KubeNeuron,
	mapping *kubeneuronv1alpha1.GPUSignalMapping,
	validationErr error,
) error {
	before := mapping.DeepCopy()
	mapping.Status.ObservedGeneration = mapping.Generation
	if validationErr != nil {
		mapping.Status.Digest = ""
		meta.SetStatusCondition(&mapping.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "CompilationFailed",
			Message:            validationErr.Error(),
			ObservedGeneration: mapping.Generation,
			LastTransitionTime: metav1.Now(),
		})
	} else {
		data, err := compileSignalMappings(installation.Name, []kubeneuronv1alpha1.GPUSignalMapping{*mapping})
		if err != nil {
			return fmt.Errorf("compile GPUSignalMapping %q for status: %w", mapping.Name, err)
		}
		mapping.Status.Digest = sha256Hex(data)
		meta.SetStatusCondition(&mapping.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Compiled",
			Message:            "signal mapping compiled into the immutable runtime snapshot",
			ObservedGeneration: mapping.Generation,
			LastTransitionTime: metav1.Now(),
		})
	}
	if reflect.DeepEqual(before.Status, mapping.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, mapping, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("update GPUSignalMapping %q status: %w", mapping.Name, err)
	}
	return nil
}

func (r *KubeNeuronReconciler) updateGPUPlaybookStatus(
	ctx context.Context,
	book *kubeneuronv1alpha1.GPUPlaybook,
	snapshot *Snapshot,
	validationErr error,
) error {
	before := book.DeepCopy()
	book.Status.ObservedGeneration = book.Generation
	if validationErr != nil {
		book.Status.Digest = ""
		meta.SetStatusCondition(&book.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "CompilationFailed",
			Message:            validationErr.Error(),
			ObservedGeneration: book.Generation,
			LastTransitionTime: metav1.Now(),
		})
	} else {
		data, ok := snapshot.Playbooks[book.Name+".yaml"]
		if !ok {
			return fmt.Errorf("playbook %q was compiled but is absent from the snapshot", book.Name)
		}
		book.Status.Digest = sha256Hex(data)
		meta.SetStatusCondition(&book.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Compiled",
			Message:            "playbook compiled into the immutable runtime snapshot",
			ObservedGeneration: book.Generation,
			LastTransitionTime: metav1.Now(),
		})
	}
	if reflect.DeepEqual(before.Status, book.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, book, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("update GPUPlaybook %q status: %w", book.Name, err)
	}
	return nil
}

func (r *KubeNeuronReconciler) updateGPURemediationPolicyStatus(
	ctx context.Context,
	installation *kubeneuronv1alpha1.KubeNeuron,
	policy *kubeneuronv1alpha1.GPURemediationPolicy,
	books map[string]*playbook.Playbook,
	validationErr error,
) error {
	before := policy.DeepCopy()
	policy.Status.ObservedGeneration = policy.Generation
	if validationErr != nil {
		policy.Status.ResolvedPlaybook = ""
		policy.Status.Digest = ""
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "CompilationFailed",
			Message:            validationErr.Error(),
			ObservedGeneration: policy.Generation,
			LastTransitionTime: metav1.Now(),
		})
	} else {
		compiled, err := compilePolicies(installation, []kubeneuronv1alpha1.GPURemediationPolicy{*policy}, books)
		if err != nil {
			return fmt.Errorf("compile GPURemediationPolicy %q for status: %w", policy.Name, err)
		}
		data, err := yaml.Marshal(compiled[0])
		if err != nil {
			return fmt.Errorf("marshal GPURemediationPolicy %q for status digest: %w", policy.Name, err)
		}
		policy.Status.ResolvedPlaybook = policy.Spec.PlaybookRef
		policy.Status.Digest = sha256Hex(data)
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Resolved",
			Message:            "policy resolved to an immutable compiled playbook",
			ObservedGeneration: policy.Generation,
			LastTransitionTime: metav1.Now(),
		})
	}
	if reflect.DeepEqual(before.Status, policy.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, policy, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("update GPURemediationPolicy %q status: %w", policy.Name, err)
	}
	return nil
}

// updateAcceleratorRuntimeProfileStatus records only whether the desired
// contract joined a coherent immutable snapshot. It deliberately does not
// represent agent report freshness, detected hardware, or remediation
// eligibility; those are controller-time observations.
func (r *KubeNeuronReconciler) updateAcceleratorRuntimeProfileStatus(
	ctx context.Context,
	profile *kubeneuronv1alpha1.AcceleratorRuntimeProfile,
	validationErr error,
) error {
	before := profile.DeepCopy()
	profile.Status.ObservedGeneration = profile.Generation
	if validationErr != nil {
		meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "CompilationFailed",
			Message:            validationErr.Error(),
			ObservedGeneration: profile.Generation,
			LastTransitionTime: metav1.Now(),
		})
	} else {
		meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Compiled",
			Message:            "accelerator runtime profile compiled into the immutable runtime snapshot",
			ObservedGeneration: profile.Generation,
			LastTransitionTime: metav1.Now(),
		})
	}
	if reflect.DeepEqual(before.Status, profile.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, profile, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("update AcceleratorRuntimeProfile %q status: %w", profile.Name, err)
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (r *KubeNeuronReconciler) ensureConfigMap(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, wanted *corev1.ConfigMap) error {
	actual := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name, Namespace: wanted.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if err := ensureControlledBy(installation, actual, "ConfigMap"); err != nil {
			return err
		}
		actual.Labels = copyStringMap(wanted.Labels)
		actual.Data = copyStringMap(wanted.Data)
		return setOwner(r.Scheme, installation, actual)
	})
	return err
}

func (r *KubeNeuronReconciler) ensureServiceAccount(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, wanted *corev1.ServiceAccount) error {
	actual := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name, Namespace: wanted.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if err := ensureControlledBy(installation, actual, "ServiceAccount"); err != nil {
			return err
		}
		actual.Labels = copyStringMap(wanted.Labels)
		// Merge annotations rather than replace: the IRSA role annotation comes
		// from the CR, but a cluster tool may have added its own that must
		// survive a reconcile.
		if len(wanted.Annotations) != 0 {
			if actual.Annotations == nil {
				actual.Annotations = map[string]string{}
			}
			for k, v := range wanted.Annotations {
				actual.Annotations[k] = v
			}
		}
		actual.AutomountServiceAccountToken = wanted.AutomountServiceAccountToken
		return setOwner(r.Scheme, installation, actual)
	})
	return err
}

func (r *KubeNeuronReconciler) ensureClusterRole(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, wanted *rbacv1.ClusterRole) error {
	actual := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if err := ensureControlledBy(installation, actual, "ClusterRole"); err != nil {
			return err
		}
		actual.Labels = copyStringMap(wanted.Labels)
		actual.Rules = append([]rbacv1.PolicyRule(nil), wanted.Rules...)
		return setOwner(r.Scheme, installation, actual)
	})
	return err
}

func (r *KubeNeuronReconciler) ensureClusterRoleBinding(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, wanted *rbacv1.ClusterRoleBinding) error {
	actual := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if err := ensureControlledBy(installation, actual, "ClusterRoleBinding"); err != nil {
			return err
		}
		actual.Labels = copyStringMap(wanted.Labels)
		actual.RoleRef = wanted.RoleRef
		actual.Subjects = append([]rbacv1.Subject(nil), wanted.Subjects...)
		return setOwner(r.Scheme, installation, actual)
	})
	return err
}

func (r *KubeNeuronReconciler) ensureService(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, wanted *corev1.Service) error {
	actual := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name, Namespace: wanted.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if err := ensureControlledBy(installation, actual, "Service"); err != nil {
			return err
		}
		actual.Labels = copyStringMap(wanted.Labels)
		actual.Spec.Selector = copyStringMap(wanted.Spec.Selector)
		actual.Spec.Ports = append([]corev1.ServicePort(nil), wanted.Spec.Ports...)
		return setOwner(r.Scheme, installation, actual)
	})
	return err
}

func (r *KubeNeuronReconciler) ensurePersistentVolumeClaim(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, wanted *corev1.PersistentVolumeClaim) error {
	actual := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name, Namespace: wanted.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if err := ensureControlledBy(installation, actual, "PersistentVolumeClaim"); err != nil {
			return err
		}
		actual.Labels = copyStringMap(wanted.Labels)
		if actual.ResourceVersion == "" {
			actual.Spec = *wanted.Spec.DeepCopy()
			actual.Annotations = copyStringMap(wanted.Annotations)
		} else {
			wantedIntent := wanted.Annotations[storageClassIntentAnnotation]
			if recordedIntent, ok := actual.Annotations[storageClassIntentAnnotation]; ok && recordedIntent != wantedIntent {
				return fmt.Errorf(
					"spec.workflowStore.sqlite.storageClassName is immutable: recorded intent %q, requested intent %q",
					recordedIntent,
					wantedIntent,
				)
			}
			if err := reconcilePersistentVolumeClaimSpec(actual, wanted); err != nil {
				return err
			}
			if actual.Annotations == nil {
				actual.Annotations = make(map[string]string)
			}
			actual.Annotations[storageClassIntentAnnotation] = wantedIntent
		}
		return setOwner(r.Scheme, installation, actual)
	})
	return err
}

func reconcilePersistentVolumeClaimSpec(actual, wanted *corev1.PersistentVolumeClaim) error {
	if !reflect.DeepEqual(actual.Spec.AccessModes, wanted.Spec.AccessModes) {
		return fmt.Errorf("PersistentVolumeClaim %s/%s access modes are immutable", actual.Namespace, actual.Name)
	}
	if !reflect.DeepEqual(actual.Spec.VolumeMode, wanted.Spec.VolumeMode) {
		return fmt.Errorf("PersistentVolumeClaim %s/%s volumeMode is immutable", actual.Namespace, actual.Name)
	}
	if wanted.Spec.StorageClassName != nil &&
		(actual.Spec.StorageClassName == nil || *actual.Spec.StorageClassName != *wanted.Spec.StorageClassName) {
		return fmt.Errorf("PersistentVolumeClaim %s/%s storageClassName is immutable", actual.Namespace, actual.Name)
	}

	current, ok := actual.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		return fmt.Errorf("PersistentVolumeClaim %s/%s has no storage request", actual.Namespace, actual.Name)
	}
	wantedSize, ok := wanted.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		return fmt.Errorf("desired PersistentVolumeClaim %s/%s has no storage request", wanted.Namespace, wanted.Name)
	}
	switch wantedSize.Cmp(current) {
	case -1:
		return fmt.Errorf(
			"spec.workflowStore.sqlite.size cannot decrease existing PersistentVolumeClaim %s/%s from %s to %s",
			actual.Namespace,
			actual.Name,
			current.String(),
			wantedSize.String(),
		)
	case 1:
		actual.Spec.Resources.Requests[corev1.ResourceStorage] = wantedSize
	}
	return nil
}

func (r *KubeNeuronReconciler) ensureDeployment(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, wanted *appsv1.Deployment) error {
	actual := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name, Namespace: wanted.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if err := ensureControlledBy(installation, actual, "Deployment"); err != nil {
			return err
		}
		actual.Labels = copyStringMap(wanted.Labels)
		actual.Annotations = annotationsPreservingPrefixes(actual.Annotations, wanted.Annotations, deploymentSystemAnnotationPrefix)
		actual.Spec = *wanted.Spec.DeepCopy()
		return setOwner(r.Scheme, installation, actual)
	})
	return err
}

func (r *KubeNeuronReconciler) ensureDaemonSet(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, wanted *appsv1.DaemonSet) error {
	actual := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name, Namespace: wanted.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if err := ensureControlledBy(installation, actual, "DaemonSet"); err != nil {
			return err
		}
		actual.Labels = copyStringMap(wanted.Labels)
		actual.Annotations = annotationsPreservingPrefixes(actual.Annotations, wanted.Annotations, daemonSetSystemAnnotationPrefix)
		actual.Spec = *wanted.Spec.DeepCopy()
		return setOwner(r.Scheme, installation, actual)
	})
	return err
}

// removeOwnedDaemonSet deletes a DaemonSet this installation controls, and
// only then: an unowned object with the same name is reported, never deleted.
// Absence is success.
func (r *KubeNeuronReconciler) removeOwnedDaemonSet(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, name string) error {
	var actual appsv1.DaemonSet
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: installation.Spec.Namespace}, &actual)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(&actual, installation) {
		return fmt.Errorf("DaemonSet %s exists but is not controlled by this installation; refusing to delete it", name)
	}
	return client.IgnoreNotFound(r.Delete(ctx, &actual))
}

func (r *KubeNeuronReconciler) ensurePDB(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, wanted *policyv1.PodDisruptionBudget) error {
	actual := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name, Namespace: wanted.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if err := ensureControlledBy(installation, actual, "PodDisruptionBudget"); err != nil {
			return err
		}
		actual.Labels = copyStringMap(wanted.Labels)
		actual.Spec = *wanted.Spec.DeepCopy()
		return setOwner(r.Scheme, installation, actual)
	})
	return err
}

func annotationsPreservingPrefixes(actual, wanted map[string]string, prefixes ...string) map[string]string {
	result := copyStringMap(wanted)
	for key, value := range actual {
		for _, prefix := range prefixes {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			if result == nil {
				result = make(map[string]string)
			}
			result[key] = value
			break
		}
	}
	return result
}

func ensureControlledBy(installation *kubeneuronv1alpha1.KubeNeuron, object client.Object, kind string) error {
	if object.GetResourceVersion() == "" || metav1.IsControlledBy(object, installation) {
		return nil
	}
	name := object.GetName()
	if object.GetNamespace() != "" {
		name = object.GetNamespace() + "/" + name
	}
	return fmt.Errorf("%s %q already exists and is not controlled by KubeNeuron %q", kind, name, installation.Name)
}

func (r *KubeNeuronReconciler) mapConfigurationToKubeNeuron(_ context.Context, object client.Object) []reconcile.Request {
	var kubeNeuronRef string
	switch value := object.(type) {
	case *kubeneuronv1alpha1.GPURemediationPolicy:
		kubeNeuronRef = value.Spec.KubeNeuronRef
	case *kubeneuronv1alpha1.GPUPlaybook:
		kubeNeuronRef = value.Spec.KubeNeuronRef
	case *kubeneuronv1alpha1.GPUSignalMapping:
		kubeNeuronRef = value.Spec.KubeNeuronRef
	case *kubeneuronv1alpha1.GPUMaintenanceWindow:
		kubeNeuronRef = value.Spec.KubeNeuronRef
	case *kubeneuronv1alpha1.GPUNodeConfig:
		kubeNeuronRef = value.Spec.KubeNeuronRef
	case *kubeneuronv1alpha1.AcceleratorRuntimeProfile:
		kubeNeuronRef = value.Spec.KubeNeuronRef
	}
	if kubeNeuronRef == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: kubeNeuronRef}}}
}

// mapTLSSecretToKubeNeuron promptly rolls workloads when any mounted TLS
// Secret changes, including material managed by cert-manager or another issuer.
// Owner references only cover operator-issued Secrets, so Owns alone is not
// sufficient here.
func (r *KubeNeuronReconciler) mapTLSSecretToKubeNeuron(ctx context.Context, object client.Object) []reconcile.Request {
	secret, ok := object.(*corev1.Secret)
	if !ok {
		return nil
	}
	var installations kubeneuronv1alpha1.KubeNeuronList
	if err := r.List(ctx, &installations); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range installations.Items {
		installation := &installations.Items[i]
		if installation.Spec.Namespace != secret.Namespace {
			continue
		}
		if _, used := tlsSecretNames(installation)[secret.Name]; used {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: installation.Name}})
		}
	}
	return requests
}

// SetupWithManager registers the root reconciler and maps configuration CR
// changes back to the referenced KubeNeuron installation.
func (r *KubeNeuronReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubeneuronv1alpha1.KubeNeuron{}).
		Owns(&corev1.ConfigMap{}).
		// The operator issues TLS material, so it has to notice that material
		// being deleted or replaced. Without this a certificate could vanish and
		// nothing would put it back until some unrelated change happened to
		// trigger a reconcile.
		Owns(&corev1.Secret{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapTLSSecretToKubeNeuron)).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&rbacv1.ClusterRole{}).
		Owns(&rbacv1.ClusterRoleBinding{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&kubeneuronv1alpha1.GPURemediationPolicy{}, handler.EnqueueRequestsFromMapFunc(r.mapConfigurationToKubeNeuron)).
		Watches(&kubeneuronv1alpha1.GPUPlaybook{}, handler.EnqueueRequestsFromMapFunc(r.mapConfigurationToKubeNeuron)).
		Watches(&kubeneuronv1alpha1.GPUSignalMapping{}, handler.EnqueueRequestsFromMapFunc(r.mapConfigurationToKubeNeuron)).
		Watches(&kubeneuronv1alpha1.GPUMaintenanceWindow{}, handler.EnqueueRequestsFromMapFunc(r.mapConfigurationToKubeNeuron)).
		Watches(&kubeneuronv1alpha1.GPUNodeConfig{}, handler.EnqueueRequestsFromMapFunc(r.mapConfigurationToKubeNeuron)).
		Watches(&kubeneuronv1alpha1.AcceleratorRuntimeProfile{}, handler.EnqueueRequestsFromMapFunc(r.mapConfigurationToKubeNeuron)).
		Complete(r)
}

var _ reconcile.Reconciler = (*KubeNeuronReconciler)(nil)
