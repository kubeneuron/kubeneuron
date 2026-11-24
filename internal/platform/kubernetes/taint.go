// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/kubeneuron/kubeneuron/internal/platform"
)

// DegradedTaintKey marks a node with an open KubeNeuron incident. The key is
// namespaced to this product, which is also how ownership is established: the
// janitor removes only this key, so a taint somebody else placed for their own
// reasons is never touched.
const DegradedTaintKey = "kubeneuron.io/degraded"

// taintPatchAttempts bounds the read-modify-write retry below. Node taints are
// an atomic list in the API, so writing one means writing all of them; the
// retry exists for the case where somebody else (a node autoscaler, a device
// plugin) wrote the same list between our read and our write.
const taintPatchAttempts = 4

var _ platform.NodeTainter = (*Platform)(nil)

// ApplyDegradedTaint puts the degraded mark on the node, leaving every other
// taint exactly as it was.
//
// The write is a JSON Patch whose first operation TESTS the resourceVersion we
// read. Taints carry no patch-merge strategy — they are an atomic list — so any
// write replaces the whole list, and a plain read-modify-write would silently
// erase a taint another controller added a millisecond earlier. The test turns
// that race into a rejected patch and a retry. It also keeps this on the
// `patch` verb: the controller has never held `update` on nodes and does not
// need it to gain a scheduling hint.
func (p *Platform) ApplyDegradedTaint(ctx context.Context, node, value, effect string) error {
	desired := corev1.Taint{Key: DegradedTaintKey, Value: value, Effect: corev1.TaintEffect(effect)}
	return p.rewriteTaints(ctx, node, nil, func(current []corev1.Taint) ([]corev1.Taint, bool) {
		out := make([]corev1.Taint, 0, len(current)+1)
		for _, taint := range current {
			if taint.Key == DegradedTaintKey {
				if taint.Value == desired.Value && taint.Effect == desired.Effect {
					return nil, false // already exactly right: write nothing
				}
				continue // ours, but stale (effect changed in config): replace it
			}
			out = append(out, taint)
		}
		return append(out, desired), true
	})
}

// RemoveDegradedTaint clears the mark, and is a no-op on a node that never
// carried one — the janitor calls it against whatever the cluster reports, so
// it must be safe to call about a node somebody already cleaned up.
//
// The owner and unknown records go in the same write: left behind, a stale
// owner would join the next incident's record and, once retention pruned it,
// keep that incident's taint for a human.
func (p *Platform) RemoveDegradedTaint(ctx context.Context, node string) error {
	return p.rewriteTaints(ctx, node, degradedRecordRemoveOps, func(current []corev1.Taint) ([]corev1.Taint, bool) {
		out := make([]corev1.Taint, 0, len(current))
		for _, taint := range current {
			if taint.Key == DegradedTaintKey {
				continue
			}
			out = append(out, taint)
		}
		if len(out) == len(current) {
			return nil, false
		}
		return out, true
	})
}

// rewriteTaints reads the node, lets mutate compute the desired taint list, and
// writes it back under an optimistic-concurrency test. mutate returning false
// means the node is already as it should be and nothing is written at all —
// which is what makes both callers cheap enough for a janitor to run on every
// reconcile tick.
//
// extra, when set, adds annotation ops to the same write.
func (p *Platform) rewriteTaints(ctx context.Context, node string, extra func(*corev1.Node) []map[string]any, mutate func([]corev1.Taint) ([]corev1.Taint, bool)) error {
	var lastErr error
	for range taintPatchAttempts {
		// Deliberately the live object, not the informer cache: the cache's
		// resourceVersion may be stale, and a stale version here does not
		// produce a wrong write (the test rejects it) but does produce a
		// guaranteed retry.
		current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil {
			return err
		}
		desired, changed := mutate(current.Spec.Taints)
		if !changed {
			return nil
		}
		// "add" on an existing object member replaces it (RFC 6902), so this
		// works whether or not the node already declares any taints.
		ops := []map[string]any{
			{"op": "test", "path": "/metadata/resourceVersion", "value": current.ResourceVersion},
			{"op": "add", "path": "/spec/taints", "value": desired},
		}
		if extra != nil {
			ops = append(ops, extra(current)...)
		}
		patch, err := json.Marshal(ops)
		if err != nil {
			return err
		}
		_, err = p.client.CoreV1().Nodes().Patch(ctx, node, k8stypes.JSONPatchType, patch, metav1.PatchOptions{})
		if err == nil {
			return nil
		}
		// A failed test op comes back 422 (Invalid); a genuine write conflict
		// comes back 409. Both mean "somebody else moved the taints" — re-read
		// and recompute rather than forcing our list over theirs.
		if !apierrors.IsInvalid(err) && !apierrors.IsConflict(err) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("taints on node %s changed under every attempt to rewrite them: %w", node, lastErr)
}

// DegradedTaintedNodes lists the nodes currently carrying the degraded mark.
//
// Served from the informer cache when it has synced, like CordonedNodes and for
// the same reason: this runs on every janitor pass, and the staleness is
// harmless — a taint the cache misses this pass it sees the next, and both the
// apply and the remove paths re-read live before writing.
func (p *Platform) DegradedTaintedNodes(ctx context.Context) ([]string, error) {
	nodes, err := p.listNodesForJanitor(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, node := range nodes {
		if hasDegradedTaint(node.Spec.Taints) {
			out = append(out, node.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func hasDegradedTaint(taints []corev1.Taint) bool {
	for _, taint := range taints {
		if taint.Key == DegradedTaintKey {
			return true
		}
	}
	return false
}

// degradedOwnersAnnotation records which incidents put the degraded taint on
// the node, as a JSON array of incident IDs. See platform.DegradedTaintOwnership.
const degradedOwnersAnnotation = "kubeneuron.io/degraded-owners"

// degradedUnknownAnnotation records that the janitor kept the degraded taint
// because the controller's store has no record of the incident behind it. It
// stops every later pass from removing the taint or notifying again.
const degradedUnknownAnnotation = "kubeneuron.io/degraded-unknown"

var _ platform.DegradedTaintOwnership = (*Platform)(nil)

// UpdateDegradedTaintOwners implements platform.DegradedTaintOwnership.
func (p *Platform) UpdateDegradedTaintOwners(ctx context.Context, node string, add, drop []string) error {
	current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !hasDegradedTaint(current.Spec.Taints) {
		return nil
	}
	raw, present := current.Annotations[degradedOwnersAnnotation]
	owners, err := decodeCordonOwners(raw)
	if err != nil {
		return fmt.Errorf("reading the degraded-taint owners of %s: %w", node, err)
	}
	updated := slices.DeleteFunc(slices.Clone(owners), func(o string) bool { return slices.Contains(drop, o) })
	for _, owner := range add {
		if !slices.Contains(updated, owner) {
			updated = append(updated, owner)
		}
	}
	if present && slices.Equal(updated, owners) {
		return nil
	}
	ops := ownerSetGuardFor(current, degradedOwnersAnnotation, raw, present)
	ops = append(ops, setAnnotationOp(degradedOwnersAnnotation, encodeCordonOwners(updated)))
	_, err = p.patchAnnotations(ctx, node, ops)
	return err
}

// SnapshotDegradedOwners returns the owners seen in the pass's node listing.
// Missing or malformed records require the normal per-node reconciliation path.
func (p *Platform) SnapshotDegradedOwners(ctx context.Context) map[string][]string {
	nodes, _ := ctx.Value(janitorSnapshotKey{}).([]*corev1.Node)
	out := make(map[string][]string)
	for _, node := range nodes {
		if !hasDegradedTaint(node.Spec.Taints) {
			continue
		}
		owners, err := decodeCordonOwners(node.Annotations[degradedOwnersAnnotation])
		if err == nil {
			sort.Strings(owners)
			out[node.Name] = owners
		}
	}
	return out
}

// DegradedTaintOwners implements platform.DegradedTaintOwnership.
//
// The cache is enough for every caller: a stale read can only miss a record
// written a moment ago, and each write re-reads the node live.
func (p *Platform) DegradedTaintOwners(ctx context.Context, node string) ([]string, bool, bool, error) {
	current, err := p.cachedNode(ctx, node)
	if err != nil {
		return nil, false, false, err
	}
	raw, recorded := current.Annotations[degradedOwnersAnnotation]
	owners, err := decodeCordonOwners(raw)
	if err != nil {
		return nil, false, false, fmt.Errorf("reading the degraded-taint owners of %s: %w", node, err)
	}
	_, unknown := current.Annotations[degradedUnknownAnnotation]
	return owners, recorded, unknown, nil
}

// MarkDegradedTaintUnknown implements platform.DegradedTaintOwnership.
func (p *Platform) MarkDegradedTaintUnknown(ctx context.Context, node string) (bool, error) {
	current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if !hasDegradedTaint(current.Spec.Taints) {
		return false, nil
	}
	if _, kept := current.Annotations[degradedUnknownAnnotation]; kept {
		return false, nil
	}
	ops := ownerSetGuardFor(current, degradedUnknownAnnotation, "", false)
	ops = append(ops, setAnnotationOp(degradedUnknownAnnotation, "true"))
	return p.patchAnnotations(ctx, node, ops)
}

// ForgetDegradedTaintUnknown implements platform.DegradedTaintOwnership.
func (p *Platform) ForgetDegradedTaintUnknown(ctx context.Context, node string) error {
	current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	value, present := current.Annotations[degradedUnknownAnnotation]
	if !present {
		return nil
	}
	_, err = p.patchAnnotations(ctx, node, []map[string]any{
		annotationTestOp(degradedUnknownAnnotation, value, true),
		{"op": "remove", "path": "/metadata/annotations/" + escapeJSONPointer(degradedUnknownAnnotation)},
	})
	return err
}

// ClearDegradedTaintRecords implements platform.DegradedTaintOwnership.
func (p *Platform) ClearDegradedTaintRecords(ctx context.Context) ([]string, error) {
	nodes, err := p.listNodesForJanitor(ctx)
	if err != nil {
		return nil, err
	}
	var cleared []string
	for _, cached := range nodes {
		if !staleDegradedRecords(cached) {
			continue
		}
		current, err := p.client.CoreV1().Nodes().Get(ctx, cached.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return cleared, err
		}
		if !staleDegradedRecords(current) {
			continue
		}
		// The taint list is pinned with the records: a taint applied since
		// this read fails the patch, and the next pass sees it.
		ops := []map[string]any{{"op": "test", "path": "/spec/taints", "value": current.Spec.Taints}}
		if len(current.Spec.Taints) == 0 {
			ops = nil
		}
		ops = append(ops, degradedRecordRemoveOps(current)...)
		applied, err := p.patchAnnotations(ctx, current.Name, ops)
		if err != nil {
			return cleared, err
		}
		if applied {
			cleared = append(cleared, current.Name)
		}
	}
	return cleared, nil
}

// staleDegradedRecords reports a node that carries the degraded-taint records
// but not the taint.
func staleDegradedRecords(node *corev1.Node) bool {
	if hasDegradedTaint(node.Spec.Taints) {
		return false
	}
	_, owners := node.Annotations[degradedOwnersAnnotation]
	_, unknown := node.Annotations[degradedUnknownAnnotation]
	return owners || unknown
}

// degradedRecordRemoveOps removes the owner and unknown records the node
// carries, each pinned to the value read.
func degradedRecordRemoveOps(node *corev1.Node) []map[string]any {
	var ops []map[string]any
	for _, ann := range []string{degradedOwnersAnnotation, degradedUnknownAnnotation} {
		if value, present := node.Annotations[ann]; present {
			ops = append(ops, annotationTestOp(ann, value, true), map[string]any{
				"op": "remove", "path": "/metadata/annotations/" + escapeJSONPointer(ann),
			})
		}
	}
	return ops
}

// ownerSetGuardFor is ownerSetGuard for any annotation: it creates the
// annotation map when the node has none, and otherwise pins the value read.
func ownerSetGuardFor(current *corev1.Node, key, raw string, present bool) []map[string]any {
	if len(current.Annotations) == 0 {
		return []map[string]any{
			{"op": "test", "path": "/metadata/annotations", "value": nil},
			{"op": "add", "path": "/metadata/annotations", "value": map[string]string{}},
		}
	}
	return []map[string]any{annotationTestOp(key, raw, present)}
}

// patchAnnotations applies a JSON Patch whose test ops pin what was read, and
// reports whether it was written. A failed test means the node moved since:
// nothing was written, and the caller's next pass re-reads, so it is not an
// error.
func (p *Platform) patchAnnotations(ctx context.Context, node string, ops []map[string]any) (bool, error) {
	patch, err := json.Marshal(ops)
	if err != nil {
		return false, err
	}
	_, err = p.client.CoreV1().Nodes().Patch(ctx, node, k8stypes.JSONPatchType, patch, metav1.PatchOptions{})
	if apierrors.IsInvalid(err) || apierrors.IsConflict(err) {
		return false, nil
	}
	return err == nil, err
}

// cachedNode reads a node from the informer cache once synced, and live
// otherwise.
func (p *Platform) cachedNode(ctx context.Context, node string) (*corev1.Node, error) {
	if lp := p.nodeLister.Load(); lp != nil {
		if cached, err := (*lp).Get(node); err == nil {
			return cached, nil
		}
	}
	return p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
}
