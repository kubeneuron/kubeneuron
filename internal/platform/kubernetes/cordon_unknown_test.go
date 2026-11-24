// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestMarkCordonUnknownIfOwnerHoldsAndRecordsOnce(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}

	marked, err := p.MarkCordonUnknownIfOwner(ctx, "gpu-1", "inc-1")
	if err != nil || !marked {
		t.Fatalf("MarkCordonUnknownIfOwner = %v, %v; want the first record written", marked, err)
	}
	marked, err = p.MarkCordonUnknownIfOwner(ctx, "gpu-1", "inc-1")
	if err != nil || marked {
		t.Fatalf("second MarkCordonUnknownIfOwner = %v, %v; want no second record, so no second notice", marked, err)
	}
	if marked, _ := p.MarkCordonUnknownIfOwner(ctx, "gpu-1", "inc-gone"); marked {
		t.Fatal("recorded a hold that is not in the owner set")
	}

	nodes, err := p.CordonedNodes(ctx)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("CordonedNodes = %v, %v", nodes, err)
	}
	if !nodes[0].HeldBy("inc-1") {
		t.Fatalf("held owners = %v, want the kept hold marked held", nodes[0].HeldOwners)
	}
	got, _ := client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if got.Annotations[cordonUnknownOwnersAnnotation] != `["inc-1"]` {
		t.Fatalf("unknown owners = %q", got.Annotations[cordonUnknownOwnersAnnotation])
	}
}

func TestMarkCordonUnknownIfOwnerOnAnUntrackedCordon(t *testing.T) {
	node := gpuNode("gpu-1", "8")
	node.Spec.Unschedulable = true
	node.Annotations = map[string]string{cordonReasonAnnotation: "kubeneuron: ecc-dbe (inc-1)"}
	client := fake.NewSimpleClientset(node)
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()

	marked, err := p.MarkCordonUnknownIfOwner(ctx, "gpu-1", "legacy-reason:kubeneuron: ecc-dbe (inc-1)")
	if err != nil || !marked {
		t.Fatalf("MarkCordonUnknownIfOwner = %v, %v", marked, err)
	}
	nodes, _ := p.CordonedNodes(ctx)
	if len(nodes) != 1 || !nodes[0].Held {
		t.Fatalf("CordonedNodes = %+v, want the untracked cordon held", nodes)
	}
}

// After a human uncordons a kept node, everything KubeNeuron wrote goes, and
// so does its karpenter.sh/do-not-disrupt pin. A pin that was there before
// KubeNeuron cordoned the node is somebody else's and stays.
func TestClearUnknownCordonsReleasedByHand(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pinnedFirst bool
	}{{"pin set by KubeNeuron", false}, {"pin set by its owner", true}} {
		t.Run(tc.name, func(t *testing.T) {
			node := gpuNode("gpu-1", "8")
			if tc.pinnedFirst {
				node.Annotations = map[string]string{doNotDisruptAnnotation: "true"}
			}
			client := fake.NewSimpleClientset(node, gpuNode("gpu-2", "8"))
			applyAPIServerPatchSemantics(client)
			p := &Platform{client: client}
			ctx := context.Background()
			for _, n := range []string{"gpu-1", "gpu-2"} {
				if err := p.CordonForOwner(ctx, n, "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
					t.Fatal(err)
				}
				if _, err := p.MarkCordonUnknownIfOwner(ctx, n, "inc-1"); err != nil {
					t.Fatal(err)
				}
			}

			cleared, err := p.ClearUnknownCordonsReleasedByHand(ctx)
			if err != nil || len(cleared) != 0 {
				t.Fatalf("cleared = %v, %v while both nodes are still cordoned", cleared, err)
			}

			uncordonByHand(t, client, "gpu-1")
			cleared, err = p.ClearUnknownCordonsReleasedByHand(ctx)
			if err != nil || len(cleared) != 1 || cleared[0] != "gpu-1" {
				t.Fatalf("cleared = %v, %v; want only the node a human uncordoned", cleared, err)
			}
			got, _ := client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
			for _, ann := range cordonAnnotations {
				if _, left := got.Annotations[ann]; left {
					t.Errorf("annotation %s left on the uncordoned node", ann)
				}
			}
			if _, pinned := got.Annotations[doNotDisruptAnnotation]; pinned != tc.pinnedFirst {
				t.Fatalf("do-not-disrupt present = %v, want %v", pinned, tc.pinnedFirst)
			}
			other, _ := client.CoreV1().Nodes().Get(ctx, "gpu-2", metav1.GetOptions{})
			if !other.Spec.Unschedulable || other.Annotations[cordonUnknownOwnersAnnotation] == "" {
				t.Fatal("the node still cordoned lost its kept cordon")
			}
		})
	}
}

func uncordonByHand(t *testing.T, client *fake.Clientset, name string) {
	t.Helper()
	ctx := context.Background()
	node, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node.Spec.Unschedulable = false
	if _, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A hand uncordon of a node that also holds a cordon for an incident the store
// knows clears only the unknown hold. The other hold keeps its bookkeeping
// and the do-not-disrupt pin.
func TestClearUnknownCordonsReleasedByHandLeavesKnownHolds(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	for _, id := range []string{"inc-old", "inc-live"} {
		if err := p.CordonForOwner(ctx, "gpu-1", id, "kubeneuron: ecc-dbe ("+id+")"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.MarkCordonUnknownIfOwner(ctx, "gpu-1", "inc-old"); err != nil {
		t.Fatal(err)
	}
	uncordonByHand(t, client, "gpu-1")

	cleared, err := p.ClearUnknownCordonsReleasedByHand(ctx)
	if err != nil || len(cleared) != 1 {
		t.Fatalf("cleared = %v, %v", cleared, err)
	}
	got, _ := client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if got.Annotations[cordonOwnersAnnotation] != `["inc-live"]` {
		t.Fatalf("owners = %q, want only the known hold", got.Annotations[cordonOwnersAnnotation])
	}
	if held := got.Annotations[cordonHeldOwnersAnnotation]; held != `[]` && held != "" {
		t.Fatalf("held owners = %q, want the unknown hold gone", held)
	}
	if _, left := got.Annotations[cordonUnknownOwnersAnnotation]; left {
		t.Fatal("the unknown record stayed")
	}
	if _, pinned := got.Annotations[doNotDisruptAnnotation]; !pinned {
		t.Fatal("do-not-disrupt went while a known hold still has the node")
	}
	if _, kept := got.Annotations[cordonRestoreAnnotation]; !kept {
		t.Fatal("the restore record went while a known hold still has the node")
	}
}

// ForgetUnknownCordon undoes the mark so the next pass marks and reports
// again; the cordon stays.
func TestForgetUnknownCordon(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.MarkCordonUnknownIfOwner(ctx, "gpu-1", "inc-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.ForgetUnknownCordon(ctx, "gpu-1", "inc-1"); err != nil {
		t.Fatal(err)
	}
	nodes, _ := p.CordonedNodes(ctx)
	if len(nodes) != 1 || nodes[0].HeldBy("inc-1") {
		t.Fatalf("CordonedNodes = %+v, want the cordon kept and the hold no longer held", nodes)
	}
	if marked, _ := p.MarkCordonUnknownIfOwner(ctx, "gpu-1", "inc-1"); !marked {
		t.Fatal("the mark was not forgotten, so the next pass cannot report again")
	}
}
