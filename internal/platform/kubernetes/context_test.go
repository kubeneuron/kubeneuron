package kubernetes

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubeneuron/kubeneuron/internal/platform"
)

func contextPod() *corev1.Pod {
	p := pod("trainer-3", controllerRef("Job"), corev1.PodRunning, false)
	p.Namespace = "ml-training"
	p.UID = k8stypes.UID("uid-trainer-3")
	p.OwnerReferences[0].Name = "trainer"
	p.OwnerReferences[0].UID = k8stypes.UID("uid-job")
	p.Labels = map[string]string{
		kueueQueueLabel: "team-a",
		rayClusterLabel: "run-42-raycluster", rayNodeTypeLbl: "worker", rayGroupLabel: "gpu-workers",
		rayOriginCRD: "RayJob", rayOriginName: "run-42",
	}
	return p
}

func unstructuredObject(gvr schema.GroupVersionResource, kind, ns, name string, extra map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": gvr.Group + "/" + gvr.Version, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": ns},
	}
	for k, v := range extra {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}

func TestResolveWorkloadContextReadsOwnersOnlyAndNeverWrites(t *testing.T) {
	p1 := contextPod()
	client := fake.NewSimpleClientset(p1)
	scheme := runtime.NewScheme()
	job := unstructuredObject(batchJobs, "Job", "ml-training", "trainer", map[string]any{
		"spec":   map[string]any{"completions": int64(8), "parallelism": int64(8), "completionMode": "Indexed", "suspend": false},
		"status": map[string]any{"active": int64(8)},
	})
	job.SetUID("uid-job")
	workload := unstructuredObject(kueueWorkloads, "Workload", "ml-training", "job-trainer-abc", map[string]any{
		"spec":   map[string]any{"queueName": "team-a", "priority": int64(100), "active": true},
		"status": map[string]any{"admission": map[string]any{"clusterQueue": "gpus"}, "conditions": []any{map[string]any{"type": "Admitted", "status": "True", "lastTransitionTime": "2026-11-05T10:00:00Z"}}},
	})
	workload.SetLabels(map[string]string{kueueJobUIDLbl: "uid-job"})
	cluster := unstructuredObject(rayClusters, "RayCluster", "ml-training", "run-42-raycluster", nil)
	rayJob := unstructuredObject(rayJobs, "RayJob", "ml-training", "run-42", nil)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		batchJobs: "JobList", kueueWorkloads: "WorkloadList", kueueWorkloadsLegacy: "WorkloadList", rayClusters: "RayClusterList", rayJobs: "RayJobList",
	}, job, workload, cluster, rayJob)
	p := &Platform{client: client, dynamic: dyn}
	w := listedWorkload(p1)
	w.Node = "n1"

	got := p.ResolveWorkloadContext(context.Background(), w, platform.ContextSelection{BatchJobs: true, Kueue: true, KubeRay: true})
	if len(got.NotObserved) != 0 {
		t.Fatalf("not observed = %v, want everything observed", got.NotObserved)
	}
	if len(got.OwnerChain) != 1 || got.OwnerChain[0] != "Job/trainer" {
		t.Fatalf("owner chain = %v", got.OwnerChain)
	}
	var bj map[string]any
	if err := json.Unmarshal(got.Context["batch_job"], &bj); err != nil || bj["indexed"] != true || bj["completions"] != float64(8) || bj["active"] != float64(8) {
		t.Fatalf("batch_job = %s (%v)", got.Context["batch_job"], err)
	}
	var kq map[string]any
	if err := json.Unmarshal(got.Context["kueue"], &kq); err != nil || kq["workload"] != "job-trainer-abc" || kq["cluster_queue"] != "gpus" || kq["local_queue"] != "team-a" || kq["priority"] != float64(100) || kq["admitted_at"] != "2026-11-05T10:00:00Z" {
		t.Fatalf("kueue = %s (%v)", got.Context["kueue"], err)
	}
	var ray map[string]any
	if err := json.Unmarshal(got.Context["ray"], &ray); err != nil || ray["cluster"] != "run-42-raycluster" || ray["node_type"] != "worker" || ray["ray_job"] != "run-42" || ray["cluster_found"] != true {
		t.Fatalf("ray = %s (%v)", got.Context["ray"], err)
	}
	for _, a := range dyn.Actions() {
		if verb := a.GetVerb(); verb != "get" && verb != "list" {
			t.Fatalf("resolver performed a %s on %s", verb, a.GetResource().Resource)
		}
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" {
			t.Fatalf("resolver performed a %s on pods", a.GetVerb())
		}
	}

	// Only the selected resolvers run.
	got = p.ResolveWorkloadContext(context.Background(), w, platform.ContextSelection{BatchJobs: true})
	if _, has := got.Context["kueue"]; has || len(got.Context) != 1 {
		t.Fatalf("unselected resolver ran: %v", got.Context)
	}
}

func TestResolveWorkloadContextReportsNotObservedInsteadOfFailing(t *testing.T) {
	p1 := contextPod()
	client := fake.NewSimpleClientset(p1)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		batchJobs: "JobList", kueueWorkloads: "WorkloadList", kueueWorkloadsLegacy: "WorkloadList", rayClusters: "RayClusterList", rayJobs: "RayJobList",
	})
	// Kueue's group is not served: the apiserver answers NotFound with no kind.
	dyn.PrependReactor("list", "workloads", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewGenericServerResponse(404, "list", schema.GroupResource{}, "", "the server could not find the requested resource", 0, false)
	})
	// Ray is forbidden to this service account.
	dyn.PrependReactor("get", "rayclusters", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "ray.io", Resource: "rayclusters"}, "run-42-raycluster", nil)
	})
	p := &Platform{client: client, dynamic: dyn}
	w := listedWorkload(p1)

	got := p.ResolveWorkloadContext(context.Background(), w, platform.ContextSelection{BatchJobs: true, Kueue: true, KubeRay: true})
	if got.NotObserved["kueue"] != contextReasonAPIUnavailable {
		t.Fatalf("kueue not-observed = %q, want api-unavailable", got.NotObserved["kueue"])
	}
	if got.NotObserved["kuberay"] != contextReasonForbidden {
		t.Fatalf("kuberay not-observed = %q, want forbidden", got.NotObserved["kuberay"])
	}
	// The Job object itself is missing: an object not found is read-failed
	// territory for a direct get, reported as api-unavailable by the
	// classifier (NotFound), never a crash.
	if _, has := got.Context["batch_job"]; has {
		t.Fatalf("batch_job observed from nothing: %s", got.Context["batch_job"])
	}
	if len(got.OwnerChain) != 1 {
		t.Fatalf("owner chain = %v", got.OwnerChain)
	}

	// A Pod without the labels or owners has no context and nothing to report.
	bare := pod("bare", nil, corev1.PodRunning, false)
	bare.Namespace = "ml-training"
	bare.UID = k8stypes.UID("uid-bare")
	client2 := fake.NewSimpleClientset(bare)
	p2 := &Platform{client: client2, dynamic: dyn}
	got = p2.ResolveWorkloadContext(context.Background(), listedWorkload(bare), platform.ContextSelection{BatchJobs: true, Kueue: true, KubeRay: true})
	if len(got.Context) != 0 || len(got.NotObserved) != 0 {
		t.Fatalf("bare pod: context=%v notObserved=%v", got.Context, got.NotObserved)
	}

	// A replaced Pod is not the one that was listed.
	w.UID = "uid-other"
	got = p.ResolveWorkloadContext(context.Background(), w, platform.ContextSelection{BatchJobs: true})
	if got.NotObserved["batchJobs"] != "replaced" {
		t.Fatalf("replaced pod not-observed = %v", got.NotObserved)
	}
	_ = metav1.Now
}

// A Kueue that serves v1beta1 only (older than v0.14) answers the v1beta2
// list with 404; the resolver then reads the same fields off v1beta1.
func TestResolveKueueFallsBackToV1beta1(t *testing.T) {
	p1 := contextPod()
	client := fake.NewSimpleClientset(p1)
	scheme := runtime.NewScheme()
	workload := unstructuredObject(kueueWorkloadsLegacy, "Workload", "ml-training", "job-trainer-abc", map[string]any{
		"spec":   map[string]any{"queueName": "team-a", "priority": int64(7)},
		"status": map[string]any{"admission": map[string]any{"clusterQueue": "gpus"}},
	})
	workload.SetLabels(map[string]string{kueueJobUIDLbl: "uid-job"})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		batchJobs: "JobList", kueueWorkloads: "WorkloadList", kueueWorkloadsLegacy: "WorkloadList", rayClusters: "RayClusterList", rayJobs: "RayJobList",
	}, workload)
	dyn.PrependReactor("list", "workloads", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Version == "v1beta2" {
			return true, nil, apierrors.NewGenericServerResponse(404, "list", schema.GroupResource{}, "", "the server could not find the requested resource", 0, false)
		}
		return false, nil, nil
	})
	p := &Platform{client: client, dynamic: dyn}
	got := p.ResolveWorkloadContext(context.Background(), listedWorkload(p1), platform.ContextSelection{Kueue: true})
	if got.NotObserved["kueue"] != "" {
		t.Fatalf("kueue not-observed = %q, want resolved through v1beta1", got.NotObserved["kueue"])
	}
	var kq map[string]any
	if err := json.Unmarshal(got.Context["kueue"], &kq); err != nil || kq["workload"] != "job-trainer-abc" || kq["cluster_queue"] != "gpus" || kq["priority"] != float64(7) {
		t.Fatalf("kueue = %s (%v)", got.Context["kueue"], err)
	}
}

// KubeRay 1.7 puts the originated-from labels on the RayCluster a RayJob
// creates, and an owner reference to the RayJob; the Pods carry neither. The
// resolver names the RayJob from either, and still from the Pod when present.
func TestResolveRayNamesTheJobFromTheCluster(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		owners []metav1.OwnerReference
	}{
		{"cluster labels", map[string]string{rayOriginCRD: "RayJob", rayOriginName: "run-42"}, nil},
		{"cluster owner reference", nil, []metav1.OwnerReference{{APIVersion: "ray.io/v1", Kind: "RayJob", Name: "run-42"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p1 := contextPod()
			delete(p1.Labels, rayOriginCRD)
			delete(p1.Labels, rayOriginName)
			cluster := unstructuredObject(rayClusters, "RayCluster", "ml-training", "run-42-raycluster", nil)
			cluster.SetLabels(tc.labels)
			cluster.SetOwnerReferences(tc.owners)
			rayJob := unstructuredObject(rayJobs, "RayJob", "ml-training", "run-42", nil)
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
				rayClusters: "RayClusterList", rayJobs: "RayJobList",
			}, cluster, rayJob)
			p := &Platform{client: fake.NewSimpleClientset(p1), dynamic: dyn}
			w := listedWorkload(p1)
			got := p.ResolveWorkloadContext(context.Background(), w, platform.ContextSelection{KubeRay: true})
			if len(got.NotObserved) != 0 {
				t.Fatalf("not observed = %v", got.NotObserved)
			}
			var ray map[string]any
			if err := json.Unmarshal(got.Context["ray"], &ray); err != nil || ray["ray_job"] != "run-42" || ray["cluster_found"] != true {
				t.Fatalf("ray = %s (%v)", got.Context["ray"], err)
			}
			for _, a := range dyn.Actions() {
				if verb := a.GetVerb(); verb != "get" && verb != "list" {
					t.Fatalf("resolver performed a %s", verb)
				}
			}
		})
	}
	// A cluster that names no job leaves ray_job absent rather than guessed.
	p1 := contextPod()
	delete(p1.Labels, rayOriginCRD)
	delete(p1.Labels, rayOriginName)
	cluster := unstructuredObject(rayClusters, "RayCluster", "ml-training", "run-42-raycluster", nil)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{rayClusters: "RayClusterList", rayJobs: "RayJobList"}, cluster)
	p := &Platform{client: fake.NewSimpleClientset(p1), dynamic: dyn}
	got := p.ResolveWorkloadContext(context.Background(), listedWorkload(p1), platform.ContextSelection{KubeRay: true})
	var ray map[string]any
	if err := json.Unmarshal(got.Context["ray"], &ray); err != nil {
		t.Fatal(err)
	}
	if _, ok := ray["ray_job"]; ok {
		t.Fatalf("ray_job guessed: %s", got.Context["ray"])
	}
}

// A Job re-created under the Pod's owner name is not the Pod's owner: the
// resolver says owner-replaced rather than describing the wrong object. A
// queue-labelled Pod with no controller owner names its queue and no
// Workload: the namespace is never listed unfiltered.
func TestContextResolversNeverAttributeAnotherObject(t *testing.T) {
	p1 := contextPod()
	client := fake.NewSimpleClientset(p1)
	scheme := runtime.NewScheme()
	replacement := unstructuredObject(batchJobs, "Job", "ml-training", "trainer", map[string]any{
		"spec": map[string]any{"completions": int64(1)},
	})
	replacement.SetUID("uid-job-replacement")
	other := unstructuredObject(kueueWorkloads, "Workload", "ml-training", "job-other-team", map[string]any{
		"spec":   map[string]any{"queueName": "other-team", "active": true},
		"status": map[string]any{"admission": map[string]any{"clusterQueue": "other"}},
	})
	other.SetLabels(map[string]string{kueueJobUIDLbl: "uid-somebody-else"})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		batchJobs: "JobList", kueueWorkloads: "WorkloadList", kueueWorkloadsLegacy: "WorkloadList",
	}, replacement, other)
	p := &Platform{client: client, dynamic: dyn}
	w := listedWorkload(p1)
	w.Node = "n1"

	got := p.ResolveWorkloadContext(context.Background(), w, platform.ContextSelection{BatchJobs: true, Kueue: true})
	if got.NotObserved["batchJobs"] != contextReasonOwnerReplaced {
		t.Fatalf("not observed = %v, want batchJobs %s", got.NotObserved, contextReasonOwnerReplaced)
	}
	if _, has := got.Context["batch_job"]; has {
		t.Fatalf("the replacement Job must not describe the Pod: %s", got.Context["batch_job"])
	}
	var kq map[string]any
	if err := json.Unmarshal(got.Context["kueue"], &kq); err != nil || kq["workload"] != "" || kq["local_queue"] != "team-a" {
		t.Fatalf("kueue = %s (%v), want the queue and no Workload: the only Workload belongs to another owner", got.Context["kueue"], err)
	}

	// No controller owner at all: the queue label alone, and no listing.
	orphan := contextPod()
	orphan.OwnerReferences = nil
	orphan.Name = "orphan"
	client = fake.NewSimpleClientset(orphan)
	var listed int
	dyn.PrependReactor("list", "workloads", func(k8stesting.Action) (bool, runtime.Object, error) {
		listed++
		return false, nil, nil
	})
	p = &Platform{client: client, dynamic: dyn}
	w = listedWorkload(orphan)
	w.Node = "n1"
	got = p.ResolveWorkloadContext(context.Background(), w, platform.ContextSelection{Kueue: true})
	if err := json.Unmarshal(got.Context["kueue"], &kq); err != nil || kq["workload"] != "" || kq["workload_lookup"] != contextReasonNoOwner {
		t.Fatalf("kueue = %s (%v), want no Workload and the no-owner reason", got.Context["kueue"], err)
	}
	if listed != 0 {
		t.Fatalf("the namespace's Workloads were listed %d time(s) for a Pod with no owner", listed)
	}
}
