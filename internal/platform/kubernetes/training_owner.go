// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
