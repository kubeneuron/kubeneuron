package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

// GPUAutonomyPlanReconciler owns only the declarative CRD status projection.
// The controller's durable operational store owns approvals, effects and the
// hash-chained audit trail; keeping those out of a mutable Kubernetes spec is
// what prevents GitOps reconciliation from accidentally replaying an effect.
type GPUAutonomyPlanReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Now    func() time.Time
}

func (r *GPUAutonomyPlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var plan kubeneuronv1alpha1.GPUAutonomyPlan
	if err := r.Get(ctx, req.NamespacedName, &plan); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	digest := autonomyPlanSpecDigest(plan.Spec)
	state, message, validationErr := validateGPUAutonomyPlanSpec(plan.Spec, now)
	if validationErr != nil {
		state = "Invalid"
		message = validationErr.Error()
	}
	if plan.Status.ObservedGeneration == plan.Generation &&
		plan.Status.ConfigDigest == digest && plan.Status.State == state &&
		conditionMatches(plan.Status.Conditions, validationErr == nil, message) {
		if state == "Draft" && plan.Spec.ExpiresAt.After(now) {
			return ctrl.Result{RequeueAfter: plan.Spec.ExpiresAt.Sub(now)}, nil
		}
		return ctrl.Result{}, nil
	}

	previousDigest := plan.Status.ConfigDigest
	plan.Status.ObservedGeneration = plan.Generation
	plan.Status.ConfigDigest = digest
	plan.Status.State = state
	transition := metav1.NewTime(now)
	plan.Status.LastTransitionTime = &transition
	if previousDigest != "" && previousDigest != digest {
		// A material spec revision starts a new lifecycle. Approval records
		// cannot be rebound to an unseen selector/action/evidence envelope.
		plan.Status.Approvals = nil
		plan.Status.SimulationID = ""
		plan.Status.RolloutID = ""
		if validationErr == nil && state != "Expired" {
			plan.Status.State = "Draft"
		}
	}
	ready := validationErr == nil && plan.Status.State != "Expired"
	meta.SetStatusCondition(&plan.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus(ready),
		Reason:             conditionReason(plan.Status.State),
		Message:            message,
		ObservedGeneration: plan.Generation,
		LastTransitionTime: transition,
	})
	if err := r.Status().Update(ctx, &plan); err != nil {
		return ctrl.Result{}, err
	}
	if plan.Status.State == "Draft" && plan.Spec.ExpiresAt.After(now) {
		return ctrl.Result{RequeueAfter: plan.Spec.ExpiresAt.Sub(now)}, nil
	}
	return ctrl.Result{}, nil
}

func (r *GPUAutonomyPlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&kubeneuronv1alpha1.GPUAutonomyPlan{}).Complete(r)
}

func validateGPUAutonomyPlanSpec(spec kubeneuronv1alpha1.GPUAutonomyPlanSpec, now time.Time) (state, message string, err error) {
	if len(spec.Selector.MatchLabels) == 0 || len(spec.Selector.MatchExpressions) != 0 {
		return "", "", fmt.Errorf("selector must contain nonempty matchLabels only")
	}
	for key, value := range spec.Selector.MatchLabels {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return "", "", fmt.Errorf("selector contains an empty label key or value")
		}
	}
	if strings.TrimSpace(spec.PolicyRef) == "" || strings.TrimSpace(spec.ProfileRef) == "" {
		return "", "", fmt.Errorf("policyRef and profileRef are required")
	}
	if !immutableAutonomyReference(spec.PolicyRef) || !immutableAutonomyReference(spec.ProfileRef) {
		return "", "", fmt.Errorf("policyRef and profileRef must name immutable revisions (name@sha256:digest or name#generation)")
	}
	if len(spec.AllowedActions) != 1 {
		return "", "", fmt.Errorf("exactly one allowedActions item is required")
	}
	if _, err := time.ParseDuration(spec.Evidence.MaxAge); err != nil || spec.Evidence.MaxAge == "0s" {
		return "", "", fmt.Errorf("evidence.maxAge must be a positive duration")
	}
	if len(spec.Evidence.RequiredSources) == 0 {
		return "", "", fmt.Errorf("evidence.requiredSources is required")
	}
	sources := map[string]bool{}
	for _, source := range spec.Evidence.RequiredSources {
		source = strings.ToLower(strings.TrimSpace(source))
		if !validAutonomyEvidenceSource(source) || sources[source] {
			return "", "", fmt.Errorf("evidence.requiredSources must be supported, nonempty and unique")
		}
		sources[source] = true
	}
	if spec.Guardrails.MaxConcurrentNodes <= 0 || spec.Guardrails.MaxActionsPerHour <= 0 || spec.Guardrails.ErrorBudget < 0 {
		return "", "", fmt.Errorf("guardrail limits are invalid")
	}
	if !spec.Guardrails.NoActiveIncident {
		return "", "", fmt.Errorf("guardrails.noActiveIncident must be true")
	}
	for _, window := range spec.Guardrails.MaintenanceWindows {
		if strings.TrimSpace(window) == "" {
			return "", "", fmt.Errorf("guardrails.maintenanceWindows entries must be nonempty")
		}
	}
	if spec.Rollout.CanaryNodes <= 0 {
		return "", "", fmt.Errorf("rollout.canaryNodes must be positive")
	}
	if spec.Rollout.CanaryNodes > spec.Guardrails.MaxConcurrentNodes {
		return "", "", fmt.Errorf("rollout.canaryNodes cannot exceed guardrails.maxConcurrentNodes")
	}
	if duration, durationErr := time.ParseDuration(spec.Rollout.BakeDuration); durationErr != nil || duration <= 0 {
		return "", "", fmt.Errorf("rollout.bakeDuration must be a positive duration")
	}
	previous := int32(0)
	for _, target := range spec.Rollout.ExpansionSteps {
		if target <= spec.Rollout.CanaryNodes || target <= previous {
			return "", "", fmt.Errorf("rollout.expansionSteps must be strictly increasing and greater than canaryNodes")
		}
		previous = target
	}
	if len(spec.Approvals.RequiredRoles) < 2 || !spec.Approvals.DistinctSubjects {
		return "", "", fmt.Errorf("at least two distinct approval roles are required")
	}
	roles := map[string]bool{}
	for _, role := range spec.Approvals.RequiredRoles {
		role = strings.TrimSpace(role)
		if role == "" || roles[role] {
			return "", "", fmt.Errorf("approval roles must be nonempty and unique")
		}
		roles[role] = true
	}
	if spec.ExpiresAt.IsZero() {
		return "", "", fmt.Errorf("expiresAt is required")
	}
	if !spec.ExpiresAt.After(now) {
		return "Expired", "plan expiry elapsed", nil
	}
	return "Draft", "validated; simulation and distinct approvals are required before canary", nil
}

func immutableAutonomyReference(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	name, revision, ok := strings.Cut(value, "@sha256:")
	if ok {
		return strings.TrimSpace(name) != "" && strings.TrimSpace(revision) != ""
	}
	name, revision, ok = strings.Cut(value, "#")
	return ok && strings.TrimSpace(name) != "" && strings.TrimSpace(revision) != ""
}

func validAutonomyEvidenceSource(source string) bool {
	switch source {
	case "agent", "controller", "dcgm":
		return true
	default:
		return false
	}
}

func autonomyPlanSpecDigest(spec kubeneuronv1alpha1.GPUAutonomyPlanSpec) string {
	// Normalize set-like slices before hashing so a semantically equivalent
	// role/source order does not silently invalidate reviewed approvals.
	copy := spec
	copy.Evidence.RequiredSources = append([]string(nil), spec.Evidence.RequiredSources...)
	copy.Approvals.RequiredRoles = append([]string(nil), spec.Approvals.RequiredRoles...)
	sort.Strings(copy.Evidence.RequiredSources)
	sort.Strings(copy.Approvals.RequiredRoles)
	blob, _ := json.Marshal(copy)
	sum := sha256.Sum256(blob)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func conditionStatus(ok bool) metav1.ConditionStatus {
	if ok {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func conditionReason(state string) string {
	switch state {
	case "Draft":
		return "Validated"
	case "Expired":
		return "Expired"
	default:
		return "InvalidSpec"
	}
}

func conditionMatches(conditions []metav1.Condition, ready bool, message string) bool {
	for _, condition := range conditions {
		if condition.Type == "Ready" {
			return (condition.Status == metav1.ConditionTrue) == ready && condition.Message == message
		}
	}
	return false
}
