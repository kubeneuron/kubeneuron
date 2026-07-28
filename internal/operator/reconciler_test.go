package operator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

func TestReconcileCreatesSQLiteClaimAndControllerDeployment(t *testing.T) {
	ctx := context.Background()
	scheme := newOperatorScheme(t)
	installation := testKubeNeuron()
	installation.UID = types.UID("fleet-uid")
	installation.Generation = 1
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: installation.Spec.Namespace}}
	playbook := &kubeneuronv1alpha1.GPUPlaybook{
		ObjectMeta: metav1.ObjectMeta{Name: "observe"},
		Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
			KubeNeuronRef: installation.Name,
			Target:        kubeneuronv1alpha1.PlaybookTargetGPU,
			Steps: []kubeneuronv1alpha1.PlaybookStep{{
				Name:   "observe",
				Action: kubeneuronv1alpha1.ActionObserve,
			}},
		},
	}
	policy := &kubeneuronv1alpha1.GPURemediationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "observe"},
		Spec: kubeneuronv1alpha1.GPURemediationPolicySpec{
			KubeNeuronRef: installation.Name,
			Match:         kubeneuronv1alpha1.SignalMatch{Class: "test"},
			PlaybookRef:   playbook.Name,
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&kubeneuronv1alpha1.KubeNeuron{},
			&kubeneuronv1alpha1.GPUPlaybook{},
			&kubeneuronv1alpha1.GPURemediationPolicy{},
			&appsv1.Deployment{},
			&appsv1.DaemonSet{},
		).
		WithObjects(installation, namespace, playbook, policy).
		Build()
	reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: installation.Name}}
	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != reconcileRetry {
		t.Fatalf("Reconcile() requeue = %v, want %v", result.RequeueAfter, reconcileRetry)
	}

	var claim corev1.PersistentVolumeClaim
	claimKey := client.ObjectKey{Name: "fleet-controller-state", Namespace: installation.Spec.Namespace}
	if err := fakeClient.Get(ctx, claimKey, &claim); err != nil {
		t.Fatalf("get PersistentVolumeClaim: %v", err)
	}
	owner := metav1.GetControllerOf(&claim)
	if owner == nil || owner.UID != installation.UID {
		t.Fatalf("PersistentVolumeClaim controller = %#v, want UID %q", owner, installation.UID)
	}

	var deployment appsv1.Deployment
	deploymentKey := client.ObjectKey{Name: "fleet-controller", Namespace: installation.Spec.Namespace}
	if err := fakeClient.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	stateVolume := findVolume(deployment.Spec.Template.Spec.Volumes, "state")
	if stateVolume == nil || stateVolume.PersistentVolumeClaim == nil ||
		stateVolume.PersistentVolumeClaim.ClaimName != claim.Name {
		t.Fatalf("Deployment state volume = %#v, want claim %q", stateVolume, claim.Name)
	}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
}

func TestReconcilePublishesCompiledChildConfigurationStatus(t *testing.T) {
	ctx := context.Background()
	scheme := newOperatorScheme(t)
	installation := testKubeNeuron()
	installation.UID = types.UID("fleet-uid")
	installation.Generation = 7
	playbook, policy := reconcileConfiguration(installation)
	playbook.Generation = 3
	policy.Generation = 4
	profile := testAcceleratorRuntimeProfile(installation.Name, "nvidia-a100", "a100", "a")
	profile.Generation = 5
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: installation.Spec.Namespace}}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&kubeneuronv1alpha1.KubeNeuron{},
			&kubeneuronv1alpha1.GPUPlaybook{},
			&kubeneuronv1alpha1.GPURemediationPolicy{},
			&kubeneuronv1alpha1.AcceleratorRuntimeProfile{},
			&appsv1.Deployment{},
			&appsv1.DaemonSet{},
		).
		WithObjects(installation, namespace, playbook, policy, &profile).
		Build()
	reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: installation.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var actualBook kubeneuronv1alpha1.GPUPlaybook
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: playbook.Name}, &actualBook); err != nil {
		t.Fatal(err)
	}
	bookReady := meta.FindStatusCondition(actualBook.Status.Conditions, "Ready")
	if actualBook.Status.ObservedGeneration != playbook.Generation || actualBook.Status.Digest == "" ||
		bookReady == nil || bookReady.Status != metav1.ConditionTrue || bookReady.Reason != "Compiled" ||
		bookReady.ObservedGeneration != playbook.Generation {
		t.Fatalf("GPUPlaybook status = %#v, want current compiled status", actualBook.Status)
	}

	var actualPolicy kubeneuronv1alpha1.GPURemediationPolicy
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: policy.Name}, &actualPolicy); err != nil {
		t.Fatal(err)
	}
	policyReady := meta.FindStatusCondition(actualPolicy.Status.Conditions, "Ready")
	if actualPolicy.Status.ObservedGeneration != policy.Generation || actualPolicy.Status.Digest == "" ||
		actualPolicy.Status.ResolvedPlaybook != playbook.Name || policyReady == nil ||
		policyReady.Status != metav1.ConditionTrue || policyReady.Reason != "Resolved" ||
		policyReady.ObservedGeneration != policy.Generation {
		t.Fatalf("GPURemediationPolicy status = %#v, want current resolved status", actualPolicy.Status)
	}

	var actualProfile kubeneuronv1alpha1.AcceleratorRuntimeProfile
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: profile.Name}, &actualProfile); err != nil {
		t.Fatal(err)
	}
	profileReady := meta.FindStatusCondition(actualProfile.Status.Conditions, "Ready")
	if actualProfile.Status.ObservedGeneration != profile.Generation || profileReady == nil ||
		profileReady.Status != metav1.ConditionTrue || profileReady.Reason != "Compiled" ||
		profileReady.ObservedGeneration != profile.Generation {
		t.Fatalf("AcceleratorRuntimeProfile status = %#v, want current compiled status", actualProfile.Status)
	}
}

func TestReconcileClearsChildSuccessStatusWhenConfigurationIsInvalid(t *testing.T) {
	ctx := context.Background()
	scheme := newOperatorScheme(t)
	installation := testKubeNeuron()
	installation.Generation = 8
	playbook, policy := reconcileConfiguration(installation)
	playbook.Generation = 5
	policy.Generation = 6
	playbook.Status = kubeneuronv1alpha1.GPUPlaybookStatus{
		ObservedGeneration: 4,
		Digest:             "stale-digest",
		Conditions: []metav1.Condition{{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Compiled",
			ObservedGeneration: 4,
			LastTransitionTime: metav1.Now(),
		}},
	}
	policy.Status = kubeneuronv1alpha1.GPURemediationPolicyStatus{
		ObservedGeneration: 5,
		ResolvedPlaybook:   playbook.Name,
		Digest:             "stale-digest",
		Conditions: []metav1.Condition{{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Resolved",
			ObservedGeneration: 5,
			LastTransitionTime: metav1.Now(),
		}},
	}
	playbook.Spec.Steps[0].Action = kubeneuronv1alpha1.PlaybookAction("Unsupported")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&kubeneuronv1alpha1.KubeNeuron{},
			&kubeneuronv1alpha1.GPUPlaybook{},
			&kubeneuronv1alpha1.GPURemediationPolicy{},
		).
		WithObjects(installation, playbook, policy).
		Build()
	reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: installation.Name}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != reconcileRetry {
		t.Fatalf("Reconcile() requeue = %v, want %v", result.RequeueAfter, reconcileRetry)
	}

	var actualBook kubeneuronv1alpha1.GPUPlaybook
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: playbook.Name}, &actualBook); err != nil {
		t.Fatal(err)
	}
	bookReady := meta.FindStatusCondition(actualBook.Status.Conditions, "Ready")
	if actualBook.Status.ObservedGeneration != playbook.Generation || actualBook.Status.Digest != "" ||
		bookReady == nil || bookReady.Status != metav1.ConditionFalse || bookReady.Reason != "CompilationFailed" ||
		bookReady.ObservedGeneration != playbook.Generation {
		t.Fatalf("GPUPlaybook status = %#v, want current failed status", actualBook.Status)
	}

	var actualPolicy kubeneuronv1alpha1.GPURemediationPolicy
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: policy.Name}, &actualPolicy); err != nil {
		t.Fatal(err)
	}
	policyReady := meta.FindStatusCondition(actualPolicy.Status.Conditions, "Ready")
	if actualPolicy.Status.ObservedGeneration != policy.Generation || actualPolicy.Status.Digest != "" ||
		actualPolicy.Status.ResolvedPlaybook != "" || policyReady == nil ||
		policyReady.Status != metav1.ConditionFalse || policyReady.Reason != "CompilationFailed" ||
		policyReady.ObservedGeneration != policy.Generation {
		t.Fatalf("GPURemediationPolicy status = %#v, want current failed status", actualPolicy.Status)
	}
}

func TestReconcileClearsStaleReadyWhenTargetNamespaceIsMissing(t *testing.T) {
	ctx := context.Background()
	scheme := newOperatorScheme(t)
	installation := testKubeNeuron()
	installation.UID = types.UID("fleet-uid")
	installation.Generation = 2
	installation.Status = kubeneuronv1alpha1.KubeNeuronStatus{
		ObservedGeneration: 1,
		Conditions: []metav1.Condition{{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "RuntimeAvailable",
			Message:            "stale ready state",
			ObservedGeneration: 1,
			LastTransitionTime: metav1.Now(),
		}},
	}
	playbook, policy := reconcileConfiguration(installation)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&kubeneuronv1alpha1.KubeNeuron{},
			&kubeneuronv1alpha1.GPUPlaybook{},
			&kubeneuronv1alpha1.GPURemediationPolicy{},
		).
		WithObjects(installation, playbook, policy).
		Build()
	reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: installation.Name}})
	if err == nil || !strings.Contains(err.Error(), "target namespace") {
		t.Fatalf("Reconcile() error = %v, want missing target namespace", err)
	}
	assertReconcileFailureStatus(t, ctx, fakeClient, installation.Name, installation.Generation, "target namespace")

	var claim corev1.PersistentVolumeClaim
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: installation.Name + "-controller-state", Namespace: installation.Spec.Namespace}, &claim); err == nil {
		t.Fatal("Reconcile() created a PVC without an existing target namespace")
	}
	var namespace corev1.Namespace
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: installation.Spec.Namespace}, &namespace); err == nil {
		t.Fatal("Reconcile() must not create the target namespace")
	}
}

func TestTargetNamespaceExistsUsesUncachedAPIReader(t *testing.T) {
	scheme := newOperatorScheme(t)
	installation := testKubeNeuron()
	apiReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: installation.Spec.Namespace}}).
		Build()
	reconciler := &KubeNeuronReconciler{
		Client:    fake.NewClientBuilder().WithScheme(scheme).Build(),
		APIReader: apiReader,
		Scheme:    scheme,
	}

	if err := reconciler.targetNamespaceExists(context.Background(), installation); err != nil {
		t.Fatalf("targetNamespaceExists() error = %v, want uncached API reader namespace", err)
	}
}

func TestReconcileClearsStaleReadyWhenConfigurationListFails(t *testing.T) {
	ctx := context.Background()
	scheme := newOperatorScheme(t)
	installation := testKubeNeuron()
	installation.UID = types.UID("fleet-uid")
	installation.Generation = 4
	installation.Status.Conditions = []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "RuntimeAvailable",
		Message:            "stale ready state",
		ObservedGeneration: 3,
		LastTransitionTime: metav1.Now(),
	}}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&kubeneuronv1alpha1.KubeNeuron{},
			&kubeneuronv1alpha1.GPUPlaybook{},
			&kubeneuronv1alpha1.GPURemediationPolicy{},
		).
		WithObjects(installation).
		Build()
	failingClient := &listFailingClient{Client: baseClient, err: errors.New("simulated list outage")}
	reconciler := &KubeNeuronReconciler{Client: failingClient, Scheme: scheme}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: installation.Name}})
	if err == nil || !strings.Contains(err.Error(), "simulated list outage") {
		t.Fatalf("Reconcile() error = %v, want list failure", err)
	}
	assertReconcileFailureStatus(t, ctx, baseClient, installation.Name, installation.Generation, "simulated list outage")

	var actual kubeneuronv1alpha1.KubeNeuron
	if err := baseClient.Get(ctx, client.ObjectKey{Name: installation.Name}, &actual); err != nil {
		t.Fatal(err)
	}
	configuration := meta.FindStatusCondition(actual.Status.Conditions, "ConfigurationValid")
	if configuration == nil || configuration.Status != metav1.ConditionUnknown || configuration.ObservedGeneration != installation.Generation {
		t.Fatalf("ConfigurationValid = %#v, want current-generation Unknown", configuration)
	}
}

func TestReconcileClearsStaleReadyOnOwnedResourceCollision(t *testing.T) {
	ctx := context.Background()
	scheme := newOperatorScheme(t)
	installation := testKubeNeuron()
	installation.UID = types.UID("fleet-uid")
	installation.Generation = 3
	installation.Status.Conditions = []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "RuntimeAvailable",
		Message:            "stale ready state",
		ObservedGeneration: 2,
		LastTransitionTime: metav1.Now(),
	}}
	playbook, policy := reconcileConfiguration(installation)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: installation.Spec.Namespace}}
	collision := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      installation.Name + RuntimeConfigSuffix,
			Namespace: installation.Spec.Namespace,
		},
		Data: map[string]string{"owner": "someone-else"},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&kubeneuronv1alpha1.KubeNeuron{},
			&kubeneuronv1alpha1.GPUPlaybook{},
			&kubeneuronv1alpha1.GPURemediationPolicy{},
		).
		WithObjects(installation, namespace, playbook, policy, collision).
		Build()
	reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: installation.Name}})
	if err == nil || !strings.Contains(err.Error(), "not controlled") {
		t.Fatalf("Reconcile() error = %v, want ownership collision", err)
	}
	assertReconcileFailureStatus(t, ctx, fakeClient, installation.Name, installation.Generation, "not controlled")

	var actual corev1.ConfigMap
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(collision), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Data["owner"] != "someone-else" || len(actual.Data) != 1 {
		t.Fatalf("unowned ConfigMap was mutated: %#v", actual.Data)
	}
}

func TestEnsurePersistentVolumeClaimPreservesBoundFieldsOnGrowth(t *testing.T) {
	ctx := context.Background()
	scheme := newOperatorScheme(t)
	installation := testKubeNeuron()
	installation.UID = types.UID("fleet-uid")
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}

	wanted, err := controllerPVC(installation)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ensurePersistentVolumeClaim(ctx, installation, wanted); err != nil {
		t.Fatalf("initial ensurePersistentVolumeClaim() error = %v", err)
	}

	var actual corev1.PersistentVolumeClaim
	key := client.ObjectKeyFromObject(wanted)
	if err := fakeClient.Get(ctx, key, &actual); err != nil {
		t.Fatal(err)
	}
	defaultClass := "cluster-default"
	actual.Spec.StorageClassName = &defaultClass
	actual.Spec.VolumeName = "pv-fleet"
	actual.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"disk": "gpu-state"}}
	if err := fakeClient.Update(ctx, &actual); err != nil {
		t.Fatal(err)
	}

	installation.Spec.WorkflowStore.SQLite = &kubeneuronv1alpha1.SQLiteStoreSpec{Size: "10Gi"}
	wanted, err = controllerPVC(installation)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ensurePersistentVolumeClaim(ctx, installation, wanted); err != nil {
		t.Fatalf("growth ensurePersistentVolumeClaim() error = %v", err)
	}
	if err := fakeClient.Get(ctx, key, &actual); err != nil {
		t.Fatal(err)
	}
	if got := actual.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Fatalf("storage request = %s, want 10Gi", got.String())
	}
	if actual.Spec.StorageClassName == nil || *actual.Spec.StorageClassName != defaultClass {
		t.Fatalf("storageClassName = %v, want preserved %q", actual.Spec.StorageClassName, defaultClass)
	}
	if actual.Spec.VolumeName != "pv-fleet" {
		t.Fatalf("volumeName = %q, want pv-fleet", actual.Spec.VolumeName)
	}
	if actual.Spec.Selector == nil || actual.Spec.Selector.MatchLabels["disk"] != "gpu-state" {
		t.Fatalf("selector = %#v, want preserved selector", actual.Spec.Selector)
	}
}

func TestEnsurePersistentVolumeClaimRejectsStorageClassIntentChangeAfterDefaulting(t *testing.T) {
	ctx := context.Background()
	scheme := newOperatorScheme(t)
	installation := testKubeNeuron()
	installation.UID = types.UID("fleet-uid")
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}

	wanted, err := controllerPVC(installation)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ensurePersistentVolumeClaim(ctx, installation, wanted); err != nil {
		t.Fatal(err)
	}
	var actual corev1.PersistentVolumeClaim
	key := client.ObjectKeyFromObject(wanted)
	if err := fakeClient.Get(ctx, key, &actual); err != nil {
		t.Fatal(err)
	}
	defaultClass := "cluster-default"
	actual.Spec.StorageClassName = &defaultClass
	if err := fakeClient.Update(ctx, &actual); err != nil {
		t.Fatal(err)
	}

	installation.Spec.WorkflowStore.SQLite.StorageClassName = &defaultClass
	wanted, err = controllerPVC(installation)
	if err != nil {
		t.Fatal(err)
	}
	err = reconciler.ensurePersistentVolumeClaim(ctx, installation, wanted)
	if err == nil || !strings.Contains(err.Error(), "storageClassName is immutable") {
		t.Fatalf("ensurePersistentVolumeClaim() error = %v, want immutable intent error", err)
	}
	if err := fakeClient.Get(ctx, key, &actual); err != nil {
		t.Fatal(err)
	}
	if got := actual.Annotations[storageClassIntentAnnotation]; got != "default" {
		t.Fatalf("storage-class intent = %q, want preserved default", got)
	}
}

func TestEnsurePersistentVolumeClaimRejectsUnsafeUpdates(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*kubeneuronv1alpha1.KubeNeuron)
		wantError string
		wantSize  string
		wantClass string
	}{
		{
			name: "decrease",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				class := "fast"
				installation.Spec.WorkflowStore.SQLite = &kubeneuronv1alpha1.SQLiteStoreSpec{
					Size:             "1Gi",
					StorageClassName: &class,
				}
			},
			wantError: "cannot decrease",
			wantSize:  "5Gi",
			wantClass: "fast",
		},
		{
			name: "storage class change",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				class := "slow"
				installation.Spec.WorkflowStore.SQLite = &kubeneuronv1alpha1.SQLiteStoreSpec{
					Size:             "5Gi",
					StorageClassName: &class,
				}
			},
			wantError: "storageClassName is immutable",
			wantSize:  "5Gi",
			wantClass: "fast",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := newOperatorScheme(t)
			installation := testKubeNeuron()
			installation.UID = types.UID("fleet-uid")
			class := "fast"
			installation.Spec.WorkflowStore.SQLite = &kubeneuronv1alpha1.SQLiteStoreSpec{
				Size:             "5Gi",
				StorageClassName: &class,
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}
			initial, err := controllerPVC(installation)
			if err != nil {
				t.Fatal(err)
			}
			if err := reconciler.ensurePersistentVolumeClaim(ctx, installation, initial); err != nil {
				t.Fatal(err)
			}

			tt.mutate(installation)
			wanted, err := controllerPVC(installation)
			if err != nil {
				t.Fatal(err)
			}
			err = reconciler.ensurePersistentVolumeClaim(ctx, installation, wanted)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ensurePersistentVolumeClaim() error = %v, want %q", err, tt.wantError)
			}

			var actual corev1.PersistentVolumeClaim
			if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(initial), &actual); err != nil {
				t.Fatal(err)
			}
			if got := actual.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse(tt.wantSize)) != 0 {
				t.Fatalf("storage request = %s, want %s", got.String(), tt.wantSize)
			}
			if actual.Spec.StorageClassName == nil || *actual.Spec.StorageClassName != tt.wantClass {
				t.Fatalf("storageClassName = %v, want %q", actual.Spec.StorageClassName, tt.wantClass)
			}
		})
	}
}

func TestEnsureManagedResourcesRejectsUnownedCollisions(t *testing.T) {
	ctx := context.Background()
	scheme := newOperatorScheme(t)
	installation := testKubeNeuron()
	installation.UID = types.UID("fleet-uid")
	claim, err := controllerPVC(installation)
	if err != nil {
		t.Fatal(err)
	}
	unownedClaim := claim.DeepCopy()
	unownedClaim.Labels = map[string]string{"owner": "someone-else"}
	unownedRole := controllerClusterRole(installation)
	unownedRole.Rules = []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}}}
	unownedBinding := controllerClusterRoleBinding(installation)
	unownedBinding.Subjects[0].Name = "someone-else"
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(unownedClaim, unownedRole, unownedBinding).
		Build()
	reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}

	if err := reconciler.ensurePersistentVolumeClaim(ctx, installation, claim); err == nil || !strings.Contains(err.Error(), "not controlled") {
		t.Fatalf("ensurePersistentVolumeClaim() error = %v, want ownership conflict", err)
	}
	if err := reconciler.ensureClusterRole(ctx, installation, controllerClusterRole(installation)); err == nil || !strings.Contains(err.Error(), "not controlled") {
		t.Fatalf("ensureClusterRole() error = %v, want ownership conflict", err)
	}
	if err := reconciler.ensureClusterRoleBinding(ctx, installation, controllerClusterRoleBinding(installation)); err == nil || !strings.Contains(err.Error(), "not controlled") {
		t.Fatalf("ensureClusterRoleBinding() error = %v, want ownership conflict", err)
	}

	var actualRole rbacv1.ClusterRole
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: unownedRole.Name}, &actualRole); err != nil {
		t.Fatal(err)
	}
	if len(actualRole.Rules) != 1 || actualRole.Rules[0].Resources[0] != "secrets" {
		t.Fatalf("unowned ClusterRole was mutated: %#v", actualRole.Rules)
	}
}

func TestEnsureWorkloadsDeclareAPIDefaultsAndPreserveSystemAnnotations(t *testing.T) {
	scheme := newOperatorScheme(t)
	installation := testKubeNeuron()
	installation.UID = types.UID("fleet-uid")
	snapshot := &Snapshot{Digest: "abc123"}

	t.Run("Deployment", func(t *testing.T) {
		wanted, err := controllerDeployment(installation, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if wanted.Spec.RevisionHistoryLimit == nil || *wanted.Spec.RevisionHistoryLimit != defaultRevisionHistoryLimit ||
			wanted.Spec.ProgressDeadlineSeconds == nil || *wanted.Spec.ProgressDeadlineSeconds != defaultProgressDeadline {
			t.Fatalf("Deployment API defaults are incomplete: %#v", wanted.Spec)
		}
		assertPodAPIDefaults(t, &wanted.Spec.Template.Spec, installation.Spec.Controller.Image)
		defaulted := wanted.DeepCopy()
		defaulted.Annotations[deploymentSystemAnnotationPrefix+"revision"] = "7"
		if err := setOwner(scheme, installation, defaulted); err != nil {
			t.Fatal(err)
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaulted).Build()
		reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}

		if err := reconciler.ensureDeployment(context.Background(), installation, wanted); err != nil {
			t.Fatal(err)
		}
		var actual appsv1.Deployment
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(defaulted), &actual); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual.Spec, defaulted.Spec) {
			t.Fatalf("Deployment API defaults were overwritten:\n got: %#v\nwant: %#v", actual.Spec, defaulted.Spec)
		}
		if actual.Annotations[deploymentSystemAnnotationPrefix+"revision"] != "7" {
			t.Fatalf("Deployment system annotations were overwritten: %#v", actual.Annotations)
		}
	})

	t.Run("DaemonSet", func(t *testing.T) {
		wanted := agentDaemonSet(installation, snapshot)
		if wanted.Spec.RevisionHistoryLimit == nil || *wanted.Spec.RevisionHistoryLimit != defaultRevisionHistoryLimit ||
			wanted.Spec.UpdateStrategy.Type != appsv1.RollingUpdateDaemonSetStrategyType ||
			wanted.Spec.UpdateStrategy.RollingUpdate == nil ||
			wanted.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable == nil ||
			wanted.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.IntValue() != 1 ||
			wanted.Spec.UpdateStrategy.RollingUpdate.MaxSurge == nil ||
			wanted.Spec.UpdateStrategy.RollingUpdate.MaxSurge.IntValue() != 0 {
			t.Fatalf("DaemonSet API defaults are incomplete: %#v", wanted.Spec)
		}
		assertPodAPIDefaults(t, &wanted.Spec.Template.Spec, installation.Spec.Agent.Image)
		defaulted := wanted.DeepCopy()
		defaulted.Annotations[daemonSetSystemAnnotationPrefix+"template.generation"] = "7"
		if err := setOwner(scheme, installation, defaulted); err != nil {
			t.Fatal(err)
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaulted).Build()
		reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}

		if err := reconciler.ensureDaemonSet(context.Background(), installation, wanted); err != nil {
			t.Fatal(err)
		}
		var actual appsv1.DaemonSet
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(defaulted), &actual); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual.Spec, defaulted.Spec) {
			t.Fatalf("DaemonSet API defaults were overwritten:\n got: %#v\nwant: %#v", actual.Spec, defaulted.Spec)
		}
		if actual.Annotations[daemonSetSystemAnnotationPrefix+"template.generation"] != "7" {
			t.Fatalf("DaemonSet system annotations were overwritten: %#v", actual.Annotations)
		}
	})
}

func assertPodAPIDefaults(t *testing.T, spec *corev1.PodSpec, image string) {
	t.Helper()
	if spec.RestartPolicy != corev1.RestartPolicyAlways ||
		spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != defaultTerminationGrace ||
		spec.DNSPolicy != corev1.DNSClusterFirst || spec.SecurityContext == nil ||
		spec.SchedulerName != corev1.DefaultSchedulerName || spec.DeprecatedServiceAccount != spec.ServiceAccountName {
		t.Fatalf("Pod API defaults are incomplete: %#v", spec)
	}
	if len(spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(spec.Containers))
	}
	container := &spec.Containers[0]
	if container.ImagePullPolicy != defaultImagePullPolicy(image) ||
		container.TerminationMessagePath != corev1.TerminationMessagePathDefault ||
		container.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		t.Fatalf("Container API defaults are incomplete: %#v", container)
	}
}

func TestRuntimeReadyAgentAvailability(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*appsv1.DaemonSet)
		wantText string
	}{
		{
			name: "zero desired",
			mutate: func(agents *appsv1.DaemonSet) {
				agents.Status = appsv1.DaemonSetStatus{ObservedGeneration: agents.Generation}
			},
			wantText: "no desired pods",
		},
		{
			name: "partial",
			mutate: func(agents *appsv1.DaemonSet) {
				agents.Status.NumberAvailable = 1
				agents.Status.NumberUnavailable = 1
			},
			wantText: "not fully available",
		},
		{
			name: "old generation",
			mutate: func(agents *appsv1.DaemonSet) {
				agents.Status.ObservedGeneration = agents.Generation - 1
			},
			wantText: "current generation",
		},
		{
			name:     "fully available",
			wantText: "runtime is available",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installation := testKubeNeuron()
			claim, deployment, agents := readyRuntimeObjects(installation)
			if tt.mutate != nil {
				tt.mutate(agents)
			}
			ready, message, err := runtimeReadyForObjects(t, installation, claim, deployment, agents)
			if err != nil {
				t.Fatalf("runtimeReady() error = %v", err)
			}
			wantReady := tt.name == "fully available"
			if ready != wantReady || !strings.Contains(message, tt.wantText) {
				t.Fatalf("runtimeReady() = (%v, %q), want ready=%v and message containing %q", ready, message, wantReady, tt.wantText)
			}
		})
	}
}

func TestRuntimeReadyRejectsStaleDeploymentAndPVCState(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*corev1.PersistentVolumeClaim, *appsv1.Deployment)
		wantText string
	}{
		{
			name: "pending claim",
			mutate: func(claim *corev1.PersistentVolumeClaim, _ *appsv1.Deployment) {
				claim.Status.Phase = corev1.ClaimPending
			},
			wantText: "not Bound",
		},
		{
			name: "resize capacity pending",
			mutate: func(claim *corev1.PersistentVolumeClaim, _ *appsv1.Deployment) {
				claim.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("10Gi")
			},
			wantText: "resize is not complete",
		},
		{
			name: "resize condition active",
			mutate: func(claim *corev1.PersistentVolumeClaim, _ *appsv1.Deployment) {
				claim.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{
					Type:   corev1.PersistentVolumeClaimResizing,
					Status: corev1.ConditionTrue,
				}}
			},
			wantText: "active Resizing condition",
		},
		{
			name: "allocated resize status",
			mutate: func(claim *corev1.PersistentVolumeClaim, _ *appsv1.Deployment) {
				claim.Status.AllocatedResourceStatuses = map[corev1.ResourceName]corev1.ClaimResourceStatus{
					corev1.ResourceStorage: corev1.PersistentVolumeClaimNodeResizePending,
				}
			},
			wantText: "resize status is NodeResizePending",
		},
		{
			name: "deployment old generation",
			mutate: func(_ *corev1.PersistentVolumeClaim, deployment *appsv1.Deployment) {
				deployment.Status.ObservedGeneration = deployment.Generation - 1
			},
			wantText: "current generation",
		},
		{
			name: "deployment old replica",
			mutate: func(_ *corev1.PersistentVolumeClaim, deployment *appsv1.Deployment) {
				deployment.Status.UpdatedReplicas = 0
			},
			wantText: "not fully available",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installation := testKubeNeuron()
			claim, deployment, agents := readyRuntimeObjects(installation)
			tt.mutate(claim, deployment)
			ready, message, err := runtimeReadyForObjects(t, installation, claim, deployment, agents)
			if err != nil {
				t.Fatalf("runtimeReady() error = %v", err)
			}
			if ready || !strings.Contains(message, tt.wantText) {
				t.Fatalf("runtimeReady() = (%v, %q), want not ready and message containing %q", ready, message, tt.wantText)
			}
		})
	}
}

func readyRuntimeObjects(installation *kubeneuronv1alpha1.KubeNeuron) (*corev1.PersistentVolumeClaim, *appsv1.Deployment, *appsv1.DaemonSet) {
	installation.UID = types.UID("fleet-uid")
	owner := *metav1.NewControllerRef(installation, kubeneuronv1alpha1.GroupVersion.WithKind("KubeNeuron"))
	storage := resource.MustParse(defaultSQLiteStorageSize)
	replicas := int32(1)
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            installation.Name + "-controller-state",
			Namespace:       installation.Spec.Namespace,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: storage,
			}},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: storage,
			},
		},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            installation.Name + "-controller",
			Namespace:       installation.Spec.Namespace,
			Generation:      2,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  2,
			Replicas:            1,
			UpdatedReplicas:     1,
			AvailableReplicas:   1,
			UnavailableReplicas: 0,
		},
	}
	agents := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            installation.Name + "-agent",
			Namespace:       installation.Spec.Namespace,
			Generation:      2,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     2,
			DesiredNumberScheduled: 2,
			CurrentNumberScheduled: 2,
			UpdatedNumberScheduled: 2,
			NumberAvailable:        2,
		},
	}
	return claim, deployment, agents
}

func runtimeReadyForObjects(
	t *testing.T,
	installation *kubeneuronv1alpha1.KubeNeuron,
	claim *corev1.PersistentVolumeClaim,
	deployment *appsv1.Deployment,
	agents *appsv1.DaemonSet,
) (bool, string, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(claim, deployment, agents).
		WithObjects(claim, deployment, agents).
		Build()
	reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}
	return reconciler.runtimeReady(context.Background(), installation)
}

func reconcileConfiguration(installation *kubeneuronv1alpha1.KubeNeuron) (*kubeneuronv1alpha1.GPUPlaybook, *kubeneuronv1alpha1.GPURemediationPolicy) {
	playbook := &kubeneuronv1alpha1.GPUPlaybook{
		ObjectMeta: metav1.ObjectMeta{Name: "observe"},
		Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
			KubeNeuronRef: installation.Name,
			Target:        kubeneuronv1alpha1.PlaybookTargetGPU,
			Steps: []kubeneuronv1alpha1.PlaybookStep{{
				Name:   "observe",
				Action: kubeneuronv1alpha1.ActionObserve,
			}},
		},
	}
	policy := &kubeneuronv1alpha1.GPURemediationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "observe"},
		Spec: kubeneuronv1alpha1.GPURemediationPolicySpec{
			KubeNeuronRef: installation.Name,
			Match:         kubeneuronv1alpha1.SignalMatch{Class: "test"},
			PlaybookRef:   playbook.Name,
		},
	}
	return playbook, policy
}

func assertReconcileFailureStatus(
	t *testing.T,
	ctx context.Context,
	kubeClient client.Client,
	name string,
	generation int64,
	wantMessage string,
) {
	t.Helper()
	var installation kubeneuronv1alpha1.KubeNeuron
	if err := kubeClient.Get(ctx, client.ObjectKey{Name: name}, &installation); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(installation.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ReconciliationFailed" {
		t.Fatalf("Ready condition = %#v, want ReconciliationFailed/False", ready)
	}
	if ready.ObservedGeneration != generation || installation.Status.ObservedGeneration != generation {
		t.Fatalf(
			"observed generations = condition %d/status %d, want %d",
			ready.ObservedGeneration,
			installation.Status.ObservedGeneration,
			generation,
		)
	}
	if !strings.Contains(ready.Message, wantMessage) {
		t.Fatalf("Ready message = %q, want text %q", ready.Message, wantMessage)
	}
}

type listFailingClient struct {
	client.Client
	err error
}

func (c *listFailingClient) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return c.err
}

func newOperatorScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		policyv1.AddToScheme,
		rbacv1.AddToScheme,
		kubeneuronv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func findVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}

// A transient optimistic-concurrency conflict must requeue silently: no
// reconciler error (slow clusters would flood the error stream and trip
// log-based test gates) and no failure status churn on the installation.
func TestReconcileRequeuesSilentlyOnUpdateConflict(t *testing.T) {
	ctx := context.Background()
	scheme := newOperatorScheme(t)
	installation := testKubeNeuron()
	installation.UID = types.UID("fleet-uid")
	playbook, policy := reconcileConfiguration(installation)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: installation.Spec.Namespace}}

	conflicts := 0
	armed := false
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&kubeneuronv1alpha1.KubeNeuron{},
			&kubeneuronv1alpha1.GPUPlaybook{},
			&kubeneuronv1alpha1.GPURemediationPolicy{},
			&appsv1.Deployment{},
			&appsv1.DaemonSet{},
		).
		WithObjects(installation, namespace, playbook, policy).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, isDeployment := obj.(*appsv1.Deployment); isDeployment && armed {
					conflicts++
					return apierrors.NewConflict(
						appsv1.Resource("deployments"), obj.GetName(),
						fmt.Errorf("the object has been modified"))
				}
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()
	reconciler := &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: installation.Name}}

	// First pass creates everything.
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	// Drift the managed Deployment so the next pass takes the update branch,
	// then arm the injected conflict for exactly that update.
	var drifted appsv1.Deployment
	deploymentKey := client.ObjectKey{Name: installation.Name + "-controller", Namespace: installation.Spec.Namespace}
	if err := fakeClient.Get(ctx, deploymentKey, &drifted); err != nil {
		t.Fatal(err)
	}
	drifted.Spec.Template.Annotations["kubeneuron.io/config-digest"] = "drifted"
	if err := fakeClient.Update(ctx, &drifted); err != nil {
		t.Fatal(err)
	}
	armed = true
	result, err := reconciler.Reconcile(ctx, request)
	if conflicts == 0 {
		t.Fatal("expected the reconciler to attempt the conflicting Deployment update")
	}
	if err != nil {
		t.Fatalf("conflict must not surface as a reconciler error, got %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("conflict must requeue, got %+v", result)
	}

	var current kubeneuronv1alpha1.KubeNeuron
	if err := fakeClient.Get(ctx, request.NamespacedName, &current); err != nil {
		t.Fatal(err)
	}
	for _, condition := range current.Status.Conditions {
		if condition.Type == "Ready" && condition.Reason == "ReconcileFailed" {
			t.Fatalf("transient conflict flipped Ready to ReconcileFailed: %+v", condition)
		}
	}
}
