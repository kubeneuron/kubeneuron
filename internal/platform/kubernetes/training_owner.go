// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	batchv1 "k8s.io/api/batch/v1"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// trainingOwner follows the controller of a worker and, for JobSet or
// RayJob, the controller of that intermediate Job or RayCluster. References
// remain visible even when their target cannot be verified.
func (p *Platform) trainingOwner(ctx context.Context, pod *corev1.Pod) ([]metav1.OwnerReference, *unstructured.Unstructured, string) {
	ctx, cancel := context.WithTimeout(ctx, contextTimeout)
	defer cancel()
	var refs []metav1.OwnerReference
	parent := metav1.GetControllerOf(pod)
	var previousKind string
	for parent != nil {
		refs = append(refs, *parent)
		var resource schema.GroupVersionResource
		switch {
		case previousKind == "" && parent.Kind == "Job" && parent.APIVersion == "batch/v1":
			resource = batchJobs
		case previousKind == "" && parent.Kind == "RayCluster" && parent.APIVersion == "ray.io/v1":
			resource = rayClusters
		case previousKind == "RayCluster" && parent.Kind == "RayJob" && parent.APIVersion == "ray.io/v1":
			resource = rayJobs
		case previousKind == "Job" && parent.Kind == "JobSet" && parent.APIVersion == "jobset.x-k8s.io/v1alpha2":
			resource = schema.GroupVersionResource{Group: "jobset.x-k8s.io", Version: "v1alpha2", Resource: "jobsets"}
		default:
			return refs, nil, "unsupported-owner"
		}
		if p.dynamic == nil {
			return refs, nil, contextReasonClientUnavailable
		}
		if parent.UID == "" || parent.Name == "" {
			return refs, nil, contextReasonMalformed
		}
		obj, err := p.dynamic.Resource(resource).Namespace(pod.Namespace).Get(ctx, parent.Name, metav1.GetOptions{})
		if err != nil {
			return refs, nil, classifyContextError(ctx, err)
		}
		if obj.GetUID() != parent.UID {
			return refs, nil, contextReasonOwnerReplaced
		}
		previousKind = parent.Kind
		parent = metav1.GetControllerOf(obj)
		if parent == nil && previousKind != "RayCluster" {
			return refs, obj, ""
		}
	}
	return refs, nil, contextReasonNoOwner
}

// TrainingWorkers snapshots allocated workers before remediation changes the node.
func (p *Platform) TrainingWorkers(ctx context.Context, node string) ([]types.TrainingWorker, error) {
	pods, err := p.nodePods(ctx, node)
	if err != nil {
		return nil, err
	}
	observedAt := time.Now().UTC()
	workers := make([]types.TrainingWorker, 0)
	for i := range pods {
		pod := &pods[i]
		if pod.Spec.NodeName != node || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || !podUsesGPU(pod) {
			continue
		}
		chain, owner, reason := p.trainingOwner(ctx, pod)
		owners := make([]types.TrainingOwner, 0, len(chain))
		for _, ref := range chain {
			owners = append(owners, types.TrainingOwner{APIVersion: ref.APIVersion, Kind: ref.Kind, Name: ref.Name, UID: string(ref.UID)})
		}
		state := trainingControllerState(owner)
		workers = append(workers, types.TrainingWorker{
			Namespace: pod.Namespace, Pod: pod.Name, PodUID: string(pod.UID),
			Source: "kubernetes", ObservedAt: &observedAt,
			Owners: owners, CompletionIndex: jobCompletionIndex(pod, owner),
			ControllerState: state, OwnerUnknownReason: reason,
			WorkerRecreation: workerRecreation(owner, state, len(pod.OwnerReferences) == 0),
		})
	}
	sort.Slice(workers, func(i, j int) bool {
		if workers[i].Namespace != workers[j].Namespace {
			return workers[i].Namespace < workers[j].Namespace
		}
		return workers[i].Pod < workers[j].Pod
	})
	return workers, nil
}

// trainingControllerState exposes only status values defined by the known
// controller. A running controller does not establish application progress.
func trainingControllerState(owner *unstructured.Unstructured) string {
	if owner == nil {
		return "Unknown"
	}
	if owner.GetKind() == "RayJob" {
		status, _, _ := unstructured.NestedString(owner.Object, "status", "jobStatus")
		switch status {
		case "PENDING", "RUNNING", "STOPPED", "SUCCEEDED", "FAILED":
			return status
		}
		return "Unknown"
	}
	conditions, _, _ := unstructured.NestedSlice(owner.Object, "status", "conditions")
	active := map[string]bool{}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok || condition["status"] != "True" {
			continue
		}
		if typ, ok := condition["type"].(string); ok {
			active[typ] = true
		}
	}
	for _, state := range []string{"Failed", "Complete", "Completed", "Suspended", "StartupPolicyCompleted"} {
		if active[state] {
			return state
		}
	}
	if owner.GetKind() == "Job" {
		n, _, _ := unstructured.NestedInt64(owner.Object, "status", "active")
		if n > 0 {
			return "Active"
		}
	}
	return "Unknown"
}

// jobCompletionIndex reads the Job controller's annotation only for the
// verified Indexed Job and within the completion range it declared.
func jobCompletionIndex(pod *corev1.Pod, job *unstructured.Unstructured) *int64 {
	if job == nil || job.GetKind() != "Job" || job.GetAPIVersion() != "batch/v1" {
		return nil
	}
	mode, _, _ := unstructured.NestedString(job.Object, "spec", "completionMode")
	n, _, _ := unstructured.NestedInt64(job.Object, "spec", "completions")
	raw := pod.Annotations[batchv1.JobCompletionIndexAnnotation]
	index, err := strconv.ParseInt(raw, 10, 32)
	if mode != "Indexed" || err != nil || index < 0 || index >= n || strconv.FormatInt(index, 10) != raw {
		return nil
	}
	return &index
}

// workerRecreation describes the owner's ability to replace a worker at the
// observation time. It cannot predict placement or successful startup.
func workerRecreation(owner *unstructured.Unstructured, state string, standalone bool) string {
	if standalone {
		return "No"
	}
	if owner == nil {
		return "Unknown"
	}
	if owner.GetDeletionTimestamp() != nil {
		return "No"
	}
	switch state {
	case "Failed", "Complete", "Completed", "Suspended", "FAILED", "STOPPED", "SUCCEEDED":
		return "No"
	}
	if owner.GetKind() == "Job" {
		suspended, _, _ := unstructured.NestedBool(owner.Object, "spec", "suspend")
		parallelism, explicit, _ := unstructured.NestedInt64(owner.Object, "spec", "parallelism")
		if suspended || (explicit && parallelism == 0) {
			return "No"
		}
		if state == "Active" {
			return "Yes"
		}
	}
	return "Unknown"
}
