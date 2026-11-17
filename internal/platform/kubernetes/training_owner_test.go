// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/utils/ptr"
)

func TestTrainingOwnerRequiresSameNamespaceAndUID(t *testing.T) {
	job := unstructuredObject(batchJobs, "Job", "training", "job", nil)
	job.SetUID("replacement")
	p := &Platform{dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), job)}
	ref := metav1.OwnerReference{APIVersion: "batch/v1", Kind: "Job", Name: "job", UID: "original", Controller: ptr.To(true)}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "training", OwnerReferences: []metav1.OwnerReference{ref}}}
	check := func(reason string) {
		t.Helper()
		chain, owner, got := p.trainingOwner(t.Context(), pod)
		if len(chain) != 1 || chain[0].UID != pod.OwnerReferences[0].UID || got != reason || (owner != nil) != (reason == "") {
			t.Fatalf("chain=%v owner=%v reason=%q, want %q", chain, owner, got, reason)
		}
	}
	check(contextReasonOwnerReplaced)
	pod.OwnerReferences[0].UID = types.UID("replacement")
	check("")
	pod.Namespace = "another"
	check(contextReasonAPIUnavailable)
}

func TestTrainingOwnerResolvesJobSetAndRayJob(t *testing.T) {
	for _, tc := range []struct {
		childKind, parentKind string
		child, parent         schema.GroupVersionResource
	}{
		{"Job", "JobSet", batchJobs, schema.GroupVersionResource{Group: "jobset.x-k8s.io", Version: "v1alpha2", Resource: "jobsets"}},
		{"RayCluster", "RayJob", rayClusters, rayJobs},
	} {
		t.Run(tc.parentKind, func(t *testing.T) {
			child := unstructuredObject(tc.child, tc.childKind, "training", "child", nil)
			root := unstructuredObject(tc.parent, tc.parentKind, "training", "root", nil)
			child.SetUID("child-uid")
			root.SetUID("root-uid")
			child.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(root, tc.parent.GroupVersion().WithKind(tc.parentKind))})
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "training", OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(child, tc.child.GroupVersion().WithKind(tc.childKind)),
			}}}
			p := &Platform{dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), child, root)}
			chain, owner, reason := p.trainingOwner(t.Context(), pod)
			if len(chain) != 2 || reason != "" || owner == nil || owner.GetUID() != root.GetUID() {
				t.Fatalf("chain=%v owner=%v reason=%q", chain, owner, reason)
			}
		})
	}
}
