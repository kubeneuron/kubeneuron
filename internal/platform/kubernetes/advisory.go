package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

// checkpointAdvisoryKeys is the closed set of annotation keys AdviseCheckpoint
// may write, and checkpointWithdrawalKeys the one key WithdrawAdvisory may.
// Spelled out for the same reason as checkpointRequestKeys: RBAC grants patch
// on the whole object, and these lists are what confine it. The request keys,
// the workload-owned keys and anything outside the prefix are absent from
// both, so an advisory can never be mistaken for a request on the object and
// a withdrawal can never remove what a workload wrote.
var (
	checkpointAdvisoryKeys = map[string]struct{}{
		checkpoint.AnnotationAdvisory:           {},
		checkpoint.AnnotationAdvisoryAt:         {},
		checkpoint.AnnotationAdvisoryIncident:   {},
		checkpoint.AnnotationAdvisoryRound:      {},
		checkpoint.AnnotationAdvisoryReason:     {},
		checkpoint.AnnotationAdvisoryNextAction: {},
	}
	checkpointWithdrawalKeys = map[string]struct{}{
		checkpoint.AnnotationAdvisoryWithdrawnAt: {},
	}
)

// AdviseCheckpoint stamps adv on the exact pod the caller listed. It is the
// advisory twin of RequestCheckpoint: same scope checks before any read, the
// same live read and confinement, the same guarded JSON Patch. It differs in
// what it resolves against the object: an advisory of the same incident and
// round already there is the stamp this call would make, so nothing is
// written; any other advisory is superseded, because one notice stands on an
// object at a time and it is the current round's. A withdrawal left by an
// earlier round is cleared by the new stamp only in the sense that the
// advisory keys now name a round the withdrawal does not belong to; the
// withdrawn-at key itself is not touched.
func (p *Platform) AdviseCheckpoint(ctx context.Context, policy checkpoint.Policy, node string, w platform.Workload, adv checkpoint.Advisory) error {
	if err := checkpointScope(policy, node, w); err != nil {
		return err
	}
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("%w: %s/%s: %v", errCheckpointRequestInvalid, w.Namespace, w.Name, err)
	}
	if err := adv.Validate(); err != nil {
		return fmt.Errorf("%w: %s/%s: %v", errCheckpointRequestInvalid, w.Namespace, w.Name, err)
	}
	pod, err := p.client.CoreV1().Pods(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: %s/%s", platform.ErrCheckpointWorkloadGone, w.Namespace, w.Name)
		}
		return fmt.Errorf("checkpoint advisory: reading %s/%s: %w", w.Namespace, w.Name, err)
	}
	if err := confinePod(node, w, pod); err != nil {
		return err
	}
	if existing, ok := checkpoint.ParseAdvisory(pod.Annotations); ok && adv.SameRound(existing) {
		return nil // this round's notice is already on the object
	}
	desired := adv.Annotations()
	for key := range desired {
		if _, allowed := checkpointAdvisoryKeys[key]; !allowed || !checkpoint.IsOwnedAnnotation(key) {
			return fmt.Errorf("checkpoint advisory: refusing to write annotation %q on %s/%s: not an operator-owned advisory key", key, w.Namespace, w.Name)
		}
	}
	if annotationsMatch(pod.Annotations, desired) {
		return nil
	}
	return p.patchCheckpointAnnotations(ctx, "advisory", node, w, pod, desired, nil)
}

// WithdrawAdvisory stamps the withdrawal key on the advisory of incidentID and
// round in force on the exact pod the caller listed. A pod carrying no
// advisory, an advisory of another incident or round, or one already
// withdrawn is left untouched and reported withdrawn=false: the notice this
// call is about does not stand on it, so there is nothing to say. The patch
// itself tests the advisory's incident and round beside the usual guards, so
// a stamp of a NEWER round that lands between the read and the write makes
// the write fail (ErrCheckpointConflict) instead of withdrawing a notice
// this call never read.
func (p *Platform) WithdrawAdvisory(ctx context.Context, policy checkpoint.Policy, node string, w platform.Workload, incidentID string, round int, at time.Time) (bool, error) {
	if err := checkpointScope(policy, node, w); err != nil {
		return false, err
	}
	if incidentID == "" || round <= 0 || at.IsZero() {
		return false, fmt.Errorf("%w: %s/%s: withdrawal names no incident, round or time", errCheckpointRequestInvalid, w.Namespace, w.Name)
	}
	pod, err := p.client.CoreV1().Pods(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Errorf("%w: %s/%s", platform.ErrCheckpointWorkloadGone, w.Namespace, w.Name)
		}
		return false, fmt.Errorf("checkpoint advisory: reading %s/%s: %w", w.Namespace, w.Name, err)
	}
	if err := confinePod(node, w, pod); err != nil {
		return false, err
	}
	existing, ok := checkpoint.ParseAdvisory(pod.Annotations)
	if !ok || existing.IncidentID != incidentID || existing.Round != round {
		return false, nil
	}
	if checkpoint.IsAdvisoryWithdrawn(pod.Annotations) {
		return false, nil
	}
	desired := checkpoint.WithdrawalAnnotations(at)
	for key := range desired {
		if _, allowed := checkpointWithdrawalKeys[key]; !allowed || !checkpoint.IsOwnedAnnotation(key) {
			return false, fmt.Errorf("checkpoint advisory: refusing to write annotation %q on %s/%s: not the withdrawal key", key, w.Namespace, w.Name)
		}
	}
	guards := map[string]string{
		checkpoint.AnnotationAdvisoryIncident: incidentID,
		checkpoint.AnnotationAdvisoryRound:    strconv.Itoa(round),
	}
	if err := p.patchCheckpointAnnotations(ctx, "withdrawal", node, w, pod, desired, guards); err != nil {
		return false, err
	}
	return true, nil
}

// patchCheckpointAnnotations sends the guarded write checkpointRequestPatch
// builds, mapping the apiserver's answers to the shared sentinels the way
// RequestCheckpoint does. what names the write for the error text only.
// guards are further annotation values the write is conditional on (JSON
// Patch test ops), beyond the object's UID, node and the keys it sets.
func (p *Platform) patchCheckpointAnnotations(ctx context.Context, what, node string, w platform.Workload, pod *corev1.Pod, desired, guards map[string]string) error {
	ops := checkpointRequestPatch(node, w, pod, desired)
	if len(guards) > 0 {
		guarded := make([]map[string]any, 0, len(ops)+len(guards))
		guarded = append(guarded, ops[:2]...) // uid and node first, as always
		for _, key := range slices.Sorted(maps.Keys(guards)) {
			guarded = append(guarded, annotationTestOp(key, guards[key], true))
		}
		ops = append(guarded, ops[2:]...)
	}
	patch, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	if _, err := p.client.CoreV1().Pods(w.Namespace).Patch(ctx, w.Name, k8stypes.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return fmt.Errorf("%w: %s/%s", platform.ErrCheckpointWorkloadGone, w.Namespace, w.Name)
		case apierrors.IsInvalid(err), apierrors.IsConflict(err):
			return fmt.Errorf("%w: %s/%s: %v", platform.ErrCheckpointConflict, w.Namespace, w.Name, err)
		}
		return fmt.Errorf("checkpoint %s: stamping %s/%s: %w", what, w.Namespace, w.Name, err)
	}
	return nil
}
