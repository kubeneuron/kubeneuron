// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ktesting "k8s.io/client-go/testing"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
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
	client := p.dynamic.(*dynamicfake.FakeDynamicClient)
	client.PrependReactor("get", "jobs", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(batchJobs.GroupResource(), "job", nil)
	})
	check(contextReasonForbidden)
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

func TestTrainingWorkersIncludesUnmanagedAcceleratorRequests(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "training", Name: "worker", UID: "pod-uid"}, Spec: corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}}}}}
	finished := pod.DeepCopy()
	finished.Name = "finished"
	finished.Status.Phase = corev1.PodSucceeded
	zero := pod.DeepCopy()
	zero.Name = "zero"
	zero.Spec.Containers[0].Resources.Requests["nvidia.com/gpu"] = resource.MustParse("0")
	p := &Platform{client: fake.NewSimpleClientset(pod, finished, zero)}
	workers, err := p.TrainingWorkers(t.Context(), "node")
	if err != nil || len(workers) != 1 || workers[0].PodUID != "pod-uid" || workers[0].OwnerUnknownReason != contextReasonNoOwner {
		t.Fatalf("workers=%+v err=%v", workers, err)
	}
}

func TestTrainingControllerState(t *testing.T) {
	job := unstructuredObject(batchJobs, "Job", "training", "job", nil)
	job.Object["status"] = map[string]any{"active": int64(1)}
	if got := trainingControllerState(job); got != "Active" {
		t.Fatal(got)
	}
	job.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Failed", "status": "False"}}}
	if got := trainingControllerState(job); got != "Unknown" {
		t.Fatal(got)
	}
	ray := unstructuredObject(rayJobs, "RayJob", "training", "ray", nil)
	ray.Object["status"] = map[string]any{"jobStatus": "RUNNING"}
	if got := trainingControllerState(ray); got != "RUNNING" {
		t.Fatal(got)
	}
}

func TestIndexedJobCompletionIndex(t *testing.T) {
	job := unstructuredObject(batchJobs, "Job", "training", "job", nil)
	job.Object["spec"] = map[string]any{"completionMode": "Indexed", "completions": int64(2)}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
	for _, raw := range []string{"0", "1", "2", "-1", "", "01"} {
		pod.Annotations["batch.kubernetes.io/job-completion-index"] = raw
		got := jobCompletionIndex(pod, job)
		if (got != nil) != (raw == "0" || raw == "1") {
			t.Fatalf("index %q: %v", raw, got)
		}
	}
}

func TestRecreationDependsOnObservedOwner(t *testing.T) {
	job := unstructuredObject(batchJobs, "Job", "training", "job", nil)
	if workerRecreation(nil, "Unknown", false) != "Unknown" || workerRecreation(nil, "Unknown", true) != "No" || workerRecreation(job, "Active", false) != "Yes" {
		t.Fatal("unexpected owner capability")
	}
	job.Object["spec"] = map[string]any{"suspend": true}
	if workerRecreation(job, "Active", false) != "No" {
		t.Fatal("suspended Job marked capable")
	}
}

func TestStandaloneRayClusterOwner(t *testing.T) {
	cluster := unstructuredObject(rayClusters, "RayCluster", "training", "standalone", nil)
	cluster.SetUID("cluster-uid")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "training", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cluster, rayClusters.GroupVersion().WithKind("RayCluster"))}}}
	p := &Platform{dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), cluster)}
	chain, owner, reason := p.trainingOwner(t.Context(), pod)
	if len(chain) != 1 || owner != nil || reason != "raycluster-without-rayjob" {
		t.Fatalf("chain=%v owner=%v reason=%q", chain, owner, reason)
	}
}

func TestPodUsesGPURequestOnlyAndZeroLimit(t *testing.T) {
	resources := corev1.ResourceRequirements{Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Resources: resources}}}}
	if !podUsesGPU(pod) {
		t.Fatal("request-only GPU missed")
	}
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("0")}}
	if podUsesGPU(pod) {
		t.Fatal("zero GPU limit counted")
	}
}
