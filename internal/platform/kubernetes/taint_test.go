package kubernetes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// foreignTaint stands in for something else's taint — a node autoscaler's, an
// operator's — that this product must never disturb.
var foreignTaint = corev1.Taint{
	Key: "example.com/dedicated", Value: "training", Effect: corev1.TaintEffectNoSchedule,
}

func taintedNode(name string, taints ...corev1.Taint) *corev1.Node {
	n := gpuNode(name, "8")
	n.ResourceVersion = "1"
	n.Spec.Taints = taints
	return n
}

func nodeTaints(t *testing.T, client *fake.Clientset, name string) []corev1.Taint {
	t.Helper()
	got, err := client.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return got.Spec.Taints
}

func TestApplyDegradedTaintLeavesOtherTaintsAlone(t *testing.T) {
	client := fake.NewSimpleClientset(taintedNode("gpu-1", foreignTaint))
	p := &Platform{client: client}

	if err := p.ApplyDegradedTaint(context.Background(), "gpu-1", "ecc-dbe", "PreferNoSchedule"); err != nil {
		t.Fatal(err)
	}
	taints := nodeTaints(t, client, "gpu-1")
	if len(taints) != 2 {
		t.Fatalf("taints = %+v, want the foreign taint kept and ours added", taints)
	}
	if taints[0] != foreignTaint {
		t.Fatalf("taints[0] = %+v, want somebody else's taint untouched", taints[0])
	}
	if taints[1].Key != DegradedTaintKey || taints[1].Value != "ecc-dbe" ||
		taints[1].Effect != corev1.TaintEffectPreferNoSchedule {
		t.Fatalf("taints[1] = %+v, want the degraded mark", taints[1])
	}
}

// The janitor calls this on every pass; a write per pass would be an apiserver
// load for no change at all.
func TestApplyDegradedTaintWritesNothingWhenAlreadyCorrect(t *testing.T) {
	client := fake.NewSimpleClientset(taintedNode("gpu-1"))
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.ApplyDegradedTaint(ctx, "gpu-1", "ecc-dbe", "PreferNoSchedule"); err != nil {
		t.Fatal(err)
	}

	patches := 0
	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		return false, nil, nil
	})
	if err := p.ApplyDegradedTaint(ctx, "gpu-1", "ecc-dbe", "PreferNoSchedule"); err != nil {
		t.Fatal(err)
	}
	if patches != 0 {
		t.Fatalf("patches = %d, want none: the node already carried exactly this mark", patches)
	}
}

// Changing the configured effect must actually reach the node, not be mistaken
// for "already marked".
func TestApplyDegradedTaintReplacesAStaleEffect(t *testing.T) {
	client := fake.NewSimpleClientset(taintedNode("gpu-1", corev1.Taint{
		Key: DegradedTaintKey, Value: "ecc-dbe", Effect: corev1.TaintEffectPreferNoSchedule,
	}))
	p := &Platform{client: client}
	if err := p.ApplyDegradedTaint(context.Background(), "gpu-1", "ecc-dbe", "NoSchedule"); err != nil {
		t.Fatal(err)
	}
	taints := nodeTaints(t, client, "gpu-1")
	if len(taints) != 1 || taints[0].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("taints = %+v, want exactly one mark carrying the new effect", taints)
	}
}

func TestRemoveDegradedTaintRemovesOnlyOurs(t *testing.T) {
	client := fake.NewSimpleClientset(taintedNode("gpu-1", foreignTaint, corev1.Taint{
		Key: DegradedTaintKey, Value: "ecc-dbe", Effect: corev1.TaintEffectPreferNoSchedule,
	}))
	p := &Platform{client: client}
	if err := p.RemoveDegradedTaint(context.Background(), "gpu-1"); err != nil {
		t.Fatal(err)
	}
	taints := nodeTaints(t, client, "gpu-1")
	if len(taints) != 1 || taints[0] != foreignTaint {
		t.Fatalf("taints = %+v, want only somebody else's taint left", taints)
	}
}

// The janitor removes against whatever the cluster reports, so it will
// routinely ask about nodes that carry no mark.
func TestRemoveDegradedTaintIsANoOpWhenAbsent(t *testing.T) {
	client := fake.NewSimpleClientset(taintedNode("gpu-1", foreignTaint))
	p := &Platform{client: client}
	patches := 0
	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		return false, nil, nil
	})
	if err := p.RemoveDegradedTaint(context.Background(), "gpu-1"); err != nil {
		t.Fatal(err)
	}
	if patches != 0 {
		t.Fatalf("patches = %d, want none for a node that was never marked", patches)
	}
}

// Taints are an atomic list, so every write replaces all of them. The
// resourceVersion test is the only thing standing between this feature and
// silently erasing a taint another controller added a millisecond earlier.
func TestTaintWriteCarriesAnOptimisticConcurrencyTest(t *testing.T) {
	client := fake.NewSimpleClientset(taintedNode("gpu-1"))
	p := &Platform{client: client}
	var captured []byte
	client.PrependReactor("patch", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		captured = action.(k8stesting.PatchAction).GetPatch()
		return false, nil, nil
	})
	if err := p.ApplyDegradedTaint(context.Background(), "gpu-1", "ecc-dbe", "PreferNoSchedule"); err != nil {
		t.Fatal(err)
	}
	var ops []map[string]any
	if err := json.Unmarshal(captured, &ops); err != nil {
		t.Fatalf("patch is not a JSON patch: %v (%s)", err, captured)
	}
	if len(ops) != 2 || ops[0]["op"] != "test" || ops[0]["path"] != "/metadata/resourceVersion" {
		t.Fatalf("patch = %s, want a resourceVersion test before the write", captured)
	}
	if ops[0]["value"] != "1" {
		t.Fatalf("test op pins %v, want the resourceVersion that was read", ops[0]["value"])
	}
}

// Somebody else rewrote the taints between our read and our write: recompute
// against the new list rather than forcing ours over theirs.
func TestTaintWriteRetriesWhenTheListMovedUnderIt(t *testing.T) {
	client := fake.NewSimpleClientset(taintedNode("gpu-1", foreignTaint))
	p := &Platform{client: client}
	attempts := 0
	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts == 1 {
			return true, nil, apierrors.NewInvalid(
				schema.GroupKind{Kind: "Node"}, "gpu-1", nil)
		}
		return false, nil, nil
	})
	if err := p.ApplyDegradedTaint(context.Background(), "gpu-1", "ecc-dbe", "PreferNoSchedule"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want one rejected write and one retry", attempts)
	}
	if len(nodeTaints(t, client, "gpu-1")) != 2 {
		t.Fatal("the retry must land the mark beside the taint that displaced it")
	}
}

// A list that keeps moving must fail loudly rather than loop forever.
func TestTaintWriteGivesUpAfterBoundedRetries(t *testing.T) {
	client := fake.NewSimpleClientset(taintedNode("gpu-1"))
	p := &Platform{client: client}
	attempts := 0
	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Resource: "nodes"}, "gpu-1", nil)
	})
	err := p.ApplyDegradedTaint(context.Background(), "gpu-1", "ecc-dbe", "PreferNoSchedule")
	if err == nil || !strings.Contains(err.Error(), "changed under every attempt") {
		t.Fatalf("err = %v, want a bounded give-up", err)
	}
	if attempts != taintPatchAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, taintPatchAttempts)
	}
}

// Recovery must be able to find the marks without this process remembering
// them, including on a node whose GPU capacity has since disappeared — that is
// exactly the node whose mark would otherwise be stranded.
func TestDegradedTaintedNodesReportsEveryMarkedNode(t *testing.T) {
	marked := taintedNode("gpu-1", corev1.Taint{
		Key: DegradedTaintKey, Effect: corev1.TaintEffectPreferNoSchedule,
	})
	noCapacity := gpuNode("gpu-2", "")
	noCapacity.Spec.Taints = []corev1.Taint{{Key: DegradedTaintKey, Effect: corev1.TaintEffectNoSchedule}}
	client := fake.NewSimpleClientset(marked, noCapacity, taintedNode("gpu-3", foreignTaint))
	p := &Platform{client: client}

	nodes, err := p.DegradedTaintedNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("marked nodes = %v, want both marked nodes and neither unmarked one", nodes)
	}
}
