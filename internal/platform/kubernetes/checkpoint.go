package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

var _ platform.WorkloadCheckpointer = (*Platform)(nil)

// checkpointRequestKeys is the closed set of annotation keys RequestCheckpoint
// may write. It is spelled out here, rather than derived from whatever
// checkpoint.Request.Annotations happens to render, because this is the in-code
// half of the `pods: patch` privilege boundary: RBAC grants patch on the whole
// object, and this list is what confines it to the operator-owned request keys.
// The workload-owned opt-in, max-wait and state keys are deliberately absent.
var checkpointRequestKeys = map[string]struct{}{
	checkpoint.AnnotationRequestedAt: {},
	checkpoint.AnnotationDeadlineAt:  {},
	checkpoint.AnnotationIncident:    {},
	checkpoint.AnnotationReason:      {},
	checkpoint.AnnotationNextAction:  {},
}

// RequestCheckpoint stamps req on the exact pod the caller listed.
//
// The pod is read live first and confined in code before anything is written:
// its namespace must be on the policy allowlist, it must still be on node, and
// it must still be the UID that was listed. A pod that fails any of those is
// not the workload the decision was made about, and nothing is patched. The
// write itself is a JSON Patch that re-asserts the identity and pins every key
// it is about to change, so an object that moved between the read and the
// write is rejected by the apiserver rather than overwritten.
//
// No clock is consulted here: req.RequestedAt is the caller's now, which is
// also what decides whether a request from another incident is still live.
func (p *Platform) RequestCheckpoint(ctx context.Context, policy checkpoint.Policy, node string, w platform.Workload, req checkpoint.Request) (checkpoint.Request, error) {
	if err := checkpointScope(policy, node, w); err != nil {
		return checkpoint.Request{}, err
	}
	if err := checkpointRequestValid(policy, w, req); err != nil {
		return checkpoint.Request{}, err
	}
	pod, err := p.client.CoreV1().Pods(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return checkpoint.Request{}, fmt.Errorf("%w: %s/%s", platform.ErrCheckpointWorkloadGone, w.Namespace, w.Name)
		}
		return checkpoint.Request{}, fmt.Errorf("checkpoint: reading %s/%s: %w", w.Namespace, w.Name, err)
	}
	if err := confinePod(node, w, pod); err != nil {
		return checkpoint.Request{}, err
	}

	inForce := req
	if existing, ok := checkpoint.ParseRequest(pod.Annotations); ok {
		if existing.IncidentID != req.IncidentID {
			if existing.DeadlineAt.After(req.RequestedAt) {
				// The error names the workload only. The foreign incident ID
				// and deadline are annotation values on the object, which a
				// tenant with patch rights could have written, and an error
				// ends up in logs and audit rows. The parsed LIVE request is
				// returned alongside, as the contract says: it is what the
				// caller binds its wait to, and the listing snapshot it holds
				// may be older than this read and name another incident.
				return existing, fmt.Errorf("%w: %s/%s carries a live request from another incident",
					platform.ErrCheckpointForeignRequest, w.Namespace, w.Name)
			}
			// An expired stamp of an earlier incident is residue, not a hold.
		} else {
			inForce.DeadlineAt = checkpoint.ResolveDeadline(req.DeadlineAt, req.IncidentID, &existing)
			if !existing.RequestedAt.IsZero() {
				// The original request time is the durable record of when this
				// incident first asked; a resumed request keeps it.
				inForce.RequestedAt = existing.RequestedAt
			}
		}
	}

	desired := inForce.Annotations()
	for key := range desired {
		if _, allowed := checkpointRequestKeys[key]; !allowed || !checkpoint.IsOwnedAnnotation(key) {
			return checkpoint.Request{}, fmt.Errorf("checkpoint: refusing to write annotation %q on %s/%s: not an operator-owned request key", key, w.Namespace, w.Name)
		}
	}
	if annotationsMatch(pod.Annotations, desired) {
		return inForce, nil // already stamped exactly so: write nothing
	}

	patch, err := json.Marshal(checkpointRequestPatch(node, w, pod, desired))
	if err != nil {
		return checkpoint.Request{}, err
	}
	if _, err := p.client.CoreV1().Pods(w.Namespace).Patch(ctx, w.Name, k8stypes.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return checkpoint.Request{}, fmt.Errorf("%w: %s/%s", platform.ErrCheckpointWorkloadGone, w.Namespace, w.Name)
		// A failed test op comes back 422 (Invalid); a write conflict 409.
		case apierrors.IsInvalid(err), apierrors.IsConflict(err):
			return checkpoint.Request{}, fmt.Errorf("%w: %s/%s: %v", platform.ErrCheckpointConflict, w.Namespace, w.Name, err)
		}
		return checkpoint.Request{}, fmt.Errorf("checkpoint: stamping %s/%s: %w", w.Namespace, w.Name, err)
	}
	return inForce, nil
}

// ObserveCheckpoint reads the exact pod live and reports what
// checkpoint.Classify needs, without ever presenting another object as the
// workload's answer.
//
// A pod that is being deleted is reported Terminal: it is on its way out, and
// the wait for it can only ever be shortened by saying so.
func (p *Platform) ObserveCheckpoint(ctx context.Context, policy checkpoint.Policy, node string, w platform.Workload) checkpoint.Observation {
	if err := checkpointScope(policy, node, w); err != nil {
		return checkpoint.Observation{Err: err}
	}
	pod, err := p.client.CoreV1().Pods(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return checkpoint.Observation{Found: false}
		}
		return checkpoint.Observation{Err: fmt.Errorf("checkpoint: reading %s/%s: %w", w.Namespace, w.Name, err)}
	}
	if string(pod.UID) != w.UID {
		// A replacement under the same name. Report the UID so Classify
		// counts the original as exited, and none of the replacement's
		// annotations, so nothing it carries can read as an acknowledgement.
		return checkpoint.Observation{Found: true, UID: string(pod.UID)}
	}
	if err := confinePod(node, w, pod); err != nil {
		return checkpoint.Observation{Err: err}
	}
	return checkpoint.Observation{
		Found:       true,
		UID:         string(pod.UID),
		Terminal:    podTerminal(pod),
		Annotations: copyAnnotations(pod.Annotations),
	}
}

// checkpointScope rejects, before any read, a request the adapter could not
// confine: a namespace the policy does not allow, or a workload with no node
// or UID to pin the write to. It fails closed on a disabled policy because
// Policy.AllowsNamespace does.
func checkpointScope(policy checkpoint.Policy, node string, w platform.Workload) error {
	switch {
	case w.Namespace == "" || w.Name == "":
		return fmt.Errorf("%w: workload has no namespace/name", platform.ErrCheckpointScope)
	case !policy.AllowsNamespace(w.Namespace):
		return fmt.Errorf("%w: namespace %q is not on the checkpoint allowlist", platform.ErrCheckpointScope, w.Namespace)
	case node == "":
		return fmt.Errorf("%w: %s/%s: no node to confine the request to", platform.ErrCheckpointScope, w.Namespace, w.Name)
	case w.UID == "":
		return fmt.Errorf("%w: %s/%s: no UID to confine the request to", platform.ErrCheckpointScope, w.Namespace, w.Name)
	}
	return nil
}

// errCheckpointRequestInvalid marks a request the adapter refused to stamp
// because the request itself, or the policy it was made under, is not one the
// protocol could durably carry. Nothing is read or written for such a request.
var errCheckpointRequestInvalid = errors.New("checkpoint: invalid request")

// checkpointRequestValid rejects, before any read, a request that must not
// reach the object even if a caller is wrong: this is the last check before
// the `pods: patch` privilege is used. Scope (namespace, node, UID, disabled
// policy) is checkpointScope's job and is assumed to have passed.
//
// A request is stamped only when it is complete and unambiguous once read
// back by ParseRequest after a restart: an incident ID exactly as it will be
// parsed, a reason and a next action a human can act on, two timestamps that
// survive the checkpoint.TimestampLayout round trip (RFC 3339 with the
// fractional second kept, so a sub-second window is stamped as granted), a
// deadline strictly after the request time,
// and a window no longer than the policy's MaxWait. The window bound is on the
// fresh request only; a same-incident stamp already on the object is resolved
// later by ResolveDeadline and may be earlier, including already expired.
func checkpointRequestValid(policy checkpoint.Policy, w platform.Workload, req checkpoint.Request) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s/%s: %s", errCheckpointRequestInvalid, w.Namespace, w.Name, fmt.Sprintf(format, args...))
	}
	if err := policy.Validate(); err != nil {
		return fail("%v", err)
	}
	switch {
	case strings.TrimSpace(req.IncidentID) == "":
		// ParseRequest would never read this back, so it could not be resumed
		// after a restart; a stamp that cannot be resumed must not go out.
		return fail("names no incident")
	case strings.TrimSpace(req.IncidentID) != req.IncidentID:
		// ParseRequest trims, so a padded ID would read back as a different
		// incident and never resume its own window.
		return fail("incident ID %q has surrounding whitespace", req.IncidentID)
	case strings.TrimSpace(string(req.Reason)) == "":
		return fail("names no reason")
	case strings.TrimSpace(req.NextAction) == "":
		return fail("names no next action")
	case req.RequestedAt.IsZero():
		return fail("has no requested-at time")
	case req.DeadlineAt.IsZero():
		return fail("has no deadline")
	}
	for _, ts := range []struct {
		name string
		at   time.Time
	}{{"requested-at", req.RequestedAt}, {"deadline", req.DeadlineAt}} {
		rendered := ts.at.UTC().Format(checkpoint.TimestampLayout)
		if parsed, err := time.Parse(time.RFC3339, rendered); err != nil || !parsed.Equal(ts.at) {
			return fail("%s %v is not representable as RFC 3339", ts.name, ts.at)
		}
	}
	if !req.DeadlineAt.After(req.RequestedAt) {
		return fail("deadline %s is not after requested-at %s",
			req.DeadlineAt.UTC().Format(checkpoint.TimestampLayout), req.RequestedAt.UTC().Format(checkpoint.TimestampLayout))
	}
	if window := req.DeadlineAt.Sub(req.RequestedAt); window > policy.MaxWait {
		return fail("window %v exceeds the policy maxWait %v", window, policy.MaxWait)
	}
	return nil
}

// confinePod checks that the live pod is the instance that was listed: same
// UID, still on the expected node. The namespace and name are already fixed
// by the GET that produced it.
func confinePod(node string, w platform.Workload, pod *corev1.Pod) error {
	if string(pod.UID) != w.UID {
		return fmt.Errorf("%w: %s/%s was replaced (listed UID %s, live UID %s)",
			platform.ErrCheckpointScope, w.Namespace, w.Name, w.UID, pod.UID)
	}
	if pod.Spec.NodeName != node {
		return fmt.Errorf("%w: %s/%s is on node %q, not %q",
			platform.ErrCheckpointScope, w.Namespace, w.Name, pod.Spec.NodeName, node)
	}
	return nil
}

// checkpointRequestPatch builds the guarded write. Every guard is a `test` op
// against what was just read: the identity of the object, the node it is on,
// and the current value (or absence) of each key about to change. Any of them
// failing means the object is no longer the one the decision was made about.
//
// A pod with no annotations at all is handled the way ownerSetGuard does for
// nodes: the map itself is tested for null and created, because a test against
// a member of a missing map fails as MISSING rather than as a failed test.
func checkpointRequestPatch(node string, w platform.Workload, pod *corev1.Pod, desired map[string]string) []map[string]any {
	ops := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": w.UID},
		{"op": "test", "path": "/spec/nodeName", "value": node},
	}
	if len(pod.Annotations) == 0 {
		ops = append(ops,
			map[string]any{"op": "test", "path": "/metadata/annotations", "value": nil},
			map[string]any{"op": "add", "path": "/metadata/annotations", "value": map[string]string{}},
		)
	} else {
		for _, key := range slices.Sorted(maps.Keys(desired)) {
			current, present := pod.Annotations[key]
			ops = append(ops, annotationTestOp(key, current, present))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(desired)) {
		ops = append(ops, setAnnotationOp(key, desired[key]))
	}
	return ops
}

// annotationsMatch reports whether every desired key already holds its
// desired value on the object.
func annotationsMatch(current, desired map[string]string) bool {
	for key, value := range desired {
		if got, ok := current[key]; !ok || got != value {
			return false
		}
	}
	return true
}

// podTerminal reports whether the pod has finished or is being deleted.
func podTerminal(pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return true
	}
	return pod.DeletionTimestamp != nil
}
