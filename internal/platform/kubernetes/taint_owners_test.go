// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var degradedTaint = corev1.Taint{Key: DegradedTaintKey, Value: "ecc-dbe", Effect: corev1.TaintEffectPreferNoSchedule}

func ownedTaintClient(t *testing.T, nodes ...*corev1.Node) (*fake.Clientset, *Platform) {
	t.Helper()
	objs := make([]runtime.Object, 0, len(nodes))
	for _, n := range nodes {
		objs = append(objs, n)
	}
	client := fake.NewSimpleClientset(objs...)
	applyAPIServerPatchSemantics(client)
	return client, &Platform{client: client}
}

func TestDegradedTaintOwnersAreRecordedAndDropped(t *testing.T) {
	_, p := ownedTaintClient(t, taintedNode("gpu-1", degradedTaint), taintedNode("gpu-2"))
	ctx := context.Background()

	owners, recorded, unknown, err := p.DegradedTaintOwners(ctx, "gpu-1")
	if err != nil || recorded || unknown || len(owners) != 0 {
		t.Fatalf("before any record: %v %v %v %v", owners, recorded, unknown, err)
	}
	if err := p.UpdateDegradedTaintOwners(ctx, "gpu-1", []string{"inc-1", "inc-2"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateDegradedTaintOwners(ctx, "gpu-1", []string{"inc-2"}, []string{"inc-1"}); err != nil {
		t.Fatal(err)
	}
	owners, recorded, _, err = p.DegradedTaintOwners(ctx, "gpu-1")
	if err != nil || !recorded || !slices.Equal(owners, []string{"inc-2"}) {
		t.Fatalf("owners = %v (recorded %v, %v), want inc-2 only", owners, recorded, err)
	}
	if err := p.UpdateDegradedTaintOwners(ctx, "gpu-2", []string{"inc-3"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, recorded, _, _ := p.DegradedTaintOwners(ctx, "gpu-2"); recorded {
		t.Fatal("recorded an owner on a node without the taint")
	}
}

func TestMarkDegradedTaintUnknownRecordsOnce(t *testing.T) {
	_, p := ownedTaintClient(t, taintedNode("gpu-1", degradedTaint), taintedNode("gpu-2"))
	ctx := context.Background()
	if marked, err := p.MarkDegradedTaintUnknown(ctx, "gpu-1"); err != nil || !marked {
		t.Fatalf("first mark = %v, %v", marked, err)
	}
	if marked, err := p.MarkDegradedTaintUnknown(ctx, "gpu-1"); err != nil || marked {
		t.Fatalf("second mark = %v, %v; want no second record, so no second notice", marked, err)
	}
	if marked, _ := p.MarkDegradedTaintUnknown(ctx, "gpu-2"); marked {
		t.Fatal("marked a node without the taint")
	}
	if _, _, unknown, _ := p.DegradedTaintOwners(ctx, "gpu-1"); !unknown {
		t.Fatal("the kept taint does not read back as kept")
	}
}

// The records go with the taint, whether KubeNeuron removes it or a human
// does.
func TestDegradedTaintRecordsGoWithTheTaint(t *testing.T) {
	client, p := ownedTaintClient(t,
		taintedNode("gpu-1", foreignTaint, degradedTaint), taintedNode("gpu-2", degradedTaint), taintedNode("gpu-3", degradedTaint))
	ctx := context.Background()
	for _, n := range []string{"gpu-1", "gpu-2", "gpu-3"} {
		if err := p.UpdateDegradedTaintOwners(ctx, n, []string{"inc-1"}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := p.MarkDegradedTaintUnknown(ctx, n); err != nil {
			t.Fatal(err)
		}
	}

	if err := p.RemoveDegradedTaint(ctx, "gpu-1"); err != nil {
		t.Fatal(err)
	}
	assertNoDegradedRecords(t, client, "gpu-1")
	if taints := nodeTaints(t, client, "gpu-1"); len(taints) != 1 || taints[0] != foreignTaint {
		t.Fatalf("taints = %+v", taints)
	}

	node, _ := client.CoreV1().Nodes().Get(ctx, "gpu-2", metav1.GetOptions{})
	node.Spec.Taints = nil // kubectl taint nodes gpu-2 kubeneuron.io/degraded-
	if _, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	cleared, err := p.ClearDegradedTaintRecords(ctx)
	if err != nil || !slices.Equal(cleared, []string{"gpu-2"}) {
		t.Fatalf("cleared = %v, %v; want only the node whose taint a human removed", cleared, err)
	}
	assertNoDegradedRecords(t, client, "gpu-2")
	if _, _, unknown, _ := p.DegradedTaintOwners(ctx, "gpu-3"); !unknown {
		t.Fatal("the node still tainted lost its records")
	}
}

func assertNoDegradedRecords(t *testing.T, client *fake.Clientset, name string) {
	t.Helper()
	got, err := client.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ann := range []string{degradedOwnersAnnotation, degradedUnknownAnnotation} {
		if _, left := got.Annotations[ann]; left {
			t.Errorf("%s: annotation %s left behind", name, ann)
		}
	}
}

// conflictNextPatches makes the next n node patches lose to a concurrent write.
func conflictNextPatches(client *fake.Clientset, n int) {
	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		if n == 0 {
			return false, nil, nil
		}
		n--
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "gpu-1", errors.New("moved"))
	})
}

// A write that lost to a concurrent one reports nothing written, so the
// caller does not notify for a mark that is not there.
func TestDegradedTaintWritesLostToAConflictReportNothingWritten(t *testing.T) {
	client, p := ownedTaintClient(t, taintedNode("gpu-1", degradedTaint))
	ctx := context.Background()
	conflictNextPatches(client, 1)
	if marked, err := p.MarkDegradedTaintUnknown(ctx, "gpu-1"); err != nil || marked {
		t.Fatalf("MarkDegradedTaintUnknown = %v, %v; want nothing written", marked, err)
	}
	if marked, err := p.MarkDegradedTaintUnknown(ctx, "gpu-1"); err != nil || !marked {
		t.Fatalf("retry = %v, %v; want the mark written", marked, err)
	}

	node, _ := client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	node.Spec.Taints = nil
	if _, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	conflictNextPatches(client, 1)
	if cleared, err := p.ClearDegradedTaintRecords(ctx); err != nil || len(cleared) != 0 {
		t.Fatalf("cleared = %v, %v; want nothing reported for a write that lost", cleared, err)
	}
	if cleared, _ := p.ClearDegradedTaintRecords(ctx); !slices.Equal(cleared, []string{"gpu-1"}) {
		t.Fatalf("retry cleared = %v", cleared)
	}
}

func TestForgetDegradedTaintUnknown(t *testing.T) {
	_, p := ownedTaintClient(t, taintedNode("gpu-1", degradedTaint))
	ctx := context.Background()
	if _, err := p.MarkDegradedTaintUnknown(ctx, "gpu-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.ForgetDegradedTaintUnknown(ctx, "gpu-1"); err != nil {
		t.Fatal(err)
	}
	if marked, _ := p.MarkDegradedTaintUnknown(ctx, "gpu-1"); !marked {
		t.Fatal("the mark was not forgotten, so the next pass cannot report again")
	}
}
