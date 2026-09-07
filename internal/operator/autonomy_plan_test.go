package operator

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

func TestGPUAutonomyPlanReconcilerValidatesAndProjectsDraft(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	plan := validGPUAutonomyPlan(now)
	scheme := newOperatorScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kubeneuronv1alpha1.GPUAutonomyPlan{}).
		WithObjects(plan).Build()
	reconciler := &GPUAutonomyPlanReconciler{Client: fakeClient, Scheme: scheme, Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: plan.Name}}); err != nil {
		t.Fatal(err)
	}
	var got kubeneuronv1alpha1.GPUAutonomyPlan
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: plan.Name}, &got); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if got.Status.State != "Draft" || got.Status.ConfigDigest == "" || ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("status = %#v", got.Status)
	}
}

func TestGPUAutonomyPlanReconcilerExpiresAndRejectsInvalidSpec(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	plan := validGPUAutonomyPlan(now)
	plan.Name = "expired-plan"
	plan.Spec.ExpiresAt = metav1.NewTime(now.Add(-time.Minute))
	scheme := newOperatorScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kubeneuronv1alpha1.GPUAutonomyPlan{}).
		WithObjects(plan).Build()
	reconciler := &GPUAutonomyPlanReconciler{Client: fakeClient, Scheme: scheme, Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: plan.Name}}); err != nil {
		t.Fatal(err)
	}
	var got kubeneuronv1alpha1.GPUAutonomyPlan
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: plan.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.State != "Expired" {
		t.Fatalf("expired plan status = %#v", got.Status)
	}
}

func TestGPUAutonomyPlanReconcilerRejectsBlankMaintenanceWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	plan := validGPUAutonomyPlan(now)
	plan.Name = "blank-maintenance-window"
	plan.Spec.Guardrails.MaintenanceWindows = []string{" "}
	scheme := newOperatorScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kubeneuronv1alpha1.GPUAutonomyPlan{}).
		WithObjects(plan).Build()
	reconciler := &GPUAutonomyPlanReconciler{Client: fakeClient, Scheme: scheme, Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: plan.Name}}); err != nil {
		t.Fatal(err)
	}
	var got kubeneuronv1alpha1.GPUAutonomyPlan
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: plan.Name}, &got); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if got.Status.State != "Invalid" || ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("blank maintenance window must be invalid: %#v", got.Status)
	}
}

func validGPUAutonomyPlan(now time.Time) *kubeneuronv1alpha1.GPUAutonomyPlan {
	return &kubeneuronv1alpha1.GPUAutonomyPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "autonomy-plan", Generation: 1},
		Spec: kubeneuronv1alpha1.GPUAutonomyPlanSpec{
			Selector:       metav1.LabelSelector{MatchLabels: map[string]string{"pool": "a100"}},
			PolicyRef:      "ecc-policy@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ProfileRef:     "nvidia-a100#1",
			AllowedActions: []kubeneuronv1alpha1.AcceleratorRuntimeAction{kubeneuronv1alpha1.AcceleratorRuntimeActionResetDevice},
			Evidence:       kubeneuronv1alpha1.GPUAutonomyEvidenceSpec{MaxAge: "5m", RequiredSources: []string{"agent", "controller"}},
			Guardrails:     kubeneuronv1alpha1.GPUAutonomyGuardrailsSpec{MaxConcurrentNodes: 1, MaxActionsPerHour: 2, ErrorBudget: 0, NoActiveIncident: true},
			Rollout:        kubeneuronv1alpha1.GPUAutonomyRolloutSpec{CanaryNodes: 1, BakeDuration: "1m"},
			Approvals:      kubeneuronv1alpha1.GPUAutonomyApprovalsSpec{RequiredRoles: []string{"platform", "safety"}, DistinctSubjects: true},
			ExpiresAt:      metav1.NewTime(now.Add(time.Hour)),
		},
	}
}
