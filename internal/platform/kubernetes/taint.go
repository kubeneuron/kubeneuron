package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
	return p.rewriteTaints(ctx, node, func(current []corev1.Taint) ([]corev1.Taint, bool) {
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
func (p *Platform) RemoveDegradedTaint(ctx context.Context, node string) error {
	return p.rewriteTaints(ctx, node, func(current []corev1.Taint) ([]corev1.Taint, bool) {
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
func (p *Platform) rewriteTaints(ctx context.Context, node string, mutate func([]corev1.Taint) ([]corev1.Taint, bool)) error {
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
		patch, err := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/resourceVersion", "value": current.ResourceVersion},
			{"op": "add", "path": "/spec/taints", "value": desired},
		})
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
	if lp := p.nodeLister.Load(); lp != nil {
		cached, err := (*lp).List(labels.Everything())
		if err == nil {
			out := make([]string, 0, len(cached))
			for _, node := range cached {
				if hasDegradedTaint(node.Spec.Taints) {
					out = append(out, node.Name)
				}
			}
			sort.Strings(out)
			return out, nil
		}
	}
	nodes, err := p.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range nodes.Items {
		if hasDegradedTaint(nodes.Items[i].Spec.Taints) {
			out = append(out, nodes.Items[i].Name)
		}
	}
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
