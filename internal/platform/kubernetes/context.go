// SPDX-License-Identifier: LicenseRef-KubeNeuron

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubeneuron/kubeneuron/internal/platform"
)

// Read-only workload context resolvers (docs/workload-safe-recovery-design.md,
// S5). Each answers one question for the impact preview — whose Pod is this
// — through get/list of the owning object, and does nothing else: no write,
// no compiled dependency on Kueue or KubeRay, every read bounded. An absent
// CRD group is "not observed", never an error.

// Resource identities, read through the dynamic client as unstructured
// objects.
var (
	batchJobs = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	// Kueue serves v1beta2 since v0.14 and stopped serving v1beta1 in v0.20;
	// the fields the resolver reads (spec.queueName, spec.priority,
	// spec.active, status.admission.clusterQueue, the Admitted condition)
	// are the same in both, so the resolver asks for v1beta2 and falls back
	// to v1beta1 when the server has no such version.
	kueueWorkloads       = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "workloads"}
	kueueWorkloadsLegacy = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta1", Resource: "workloads"}
	rayClusters          = schema.GroupVersionResource{Group: "ray.io", Version: "v1", Resource: "rayclusters"}
	rayJobs              = schema.GroupVersionResource{Group: "ray.io", Version: "v1", Resource: "rayjobs"}
	contextTimeout       = 5 * time.Second
	contextListCap       = int64(50)
	kueueQueueLabel      = "kueue.x-k8s.io/queue-name"
	kueueJobUIDLbl       = "kueue.x-k8s.io/job-uid"
	rayClusterLabel      = "ray.io/cluster"
	rayNodeTypeLbl       = "ray.io/node-type"
	rayGroupLabel        = "ray.io/group"
	rayOriginCRD         = "ray.io/originated-from-crd"
	rayOriginName        = "ray.io/originated-from-cr-name"
)

// Not-observed reasons. Bounded tokens for explanation, not a closed set.
const (
	contextReasonClientUnavailable = "dynamic-client-unavailable"
	contextReasonAPIUnavailable    = "api-unavailable"
	contextReasonForbidden         = "forbidden"
	contextReasonTimeout           = "timeout"
	contextReasonReadFailed        = "read-failed"
	contextReasonMalformed         = "malformed-object"
	contextReasonNoOwner           = "no-owner"
	// contextReasonOwnerReplaced: the owning object named on the Pod exists
	// by name but is not the object the Pod belongs to (a replacement Job
	// of the same name; the Pod is an orphan of the old one).
	contextReasonOwnerReplaced = "owner-replaced"
)

var _ platform.WorkloadContextResolver = (*Platform)(nil)

// ResolveWorkloadContext implements platform.WorkloadContextResolver.
func (p *Platform) ResolveWorkloadContext(ctx context.Context, w platform.Workload, which platform.ContextSelection) platform.WorkloadContext {
	out := platform.WorkloadContext{Context: map[string]json.RawMessage{}, NotObserved: map[string]string{}}
	if p == nil || p.client == nil {
		for _, name := range which.Names() {
			out.NotObserved[name] = contextReasonClientUnavailable
		}
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, contextTimeout)
	defer cancel()
	pod, err := p.client.CoreV1().Pods(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		reason := classifyContextError(ctx, err)
		for _, name := range which.Names() {
			out.NotObserved[name] = reason
		}
		return out
	}
	if string(pod.UID) != w.UID {
		for _, name := range which.Names() {
			out.NotObserved[name] = "replaced"
		}
		return out
	}
	out.OwnerChain = ownerChain(pod)
	if which.BatchJobs {
		if raw, reason := p.resolveBatchJob(ctx, pod); reason != "" {
			out.NotObserved["batchJobs"] = reason
		} else if raw != nil {
			out.Context["batch_job"] = raw
		}
	}
	if which.Kueue {
		if raw, reason := p.resolveKueue(ctx, pod); reason != "" {
			out.NotObserved["kueue"] = reason
		} else if raw != nil {
			out.Context["kueue"] = raw
		}
	}
	if which.KubeRay {
		if raw, reason := p.resolveRay(ctx, pod); reason != "" {
			out.NotObserved["kuberay"] = reason
		} else if raw != nil {
			out.Context["ray"] = raw
		}
	}
	return out
}

// ownerChain renders the Pod's controller references as "Kind/name" entries,
// bounded to the direct owners (no further walk: one read per resolver).
func ownerChain(pod *corev1.Pod) []string {
	var out []string
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "" || ref.Name == "" {
			continue
		}
		out = append(out, ref.Kind+"/"+ref.Name)
	}
	return out
}

func classifyContextError(ctx context.Context, err error) string {
	switch {
	case apierrors.IsNotFound(err):
		return contextReasonAPIUnavailable
	case apierrors.IsForbidden(err):
		return contextReasonForbidden
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded), apierrors.IsTimeout(err):
		return contextReasonTimeout
	default:
		return contextReasonReadFailed
	}
}

// resolveBatchJob follows a Job owner reference to batch/v1 and reads the
// shape of the job: completions, parallelism, completion mode, suspend, and
// how many Pods are active. No owner means no context, not an error.
func (p *Platform) resolveBatchJob(ctx context.Context, pod *corev1.Pod) (json.RawMessage, string) {
	var owner *metav1.OwnerReference
	for i := range pod.OwnerReferences {
		if pod.OwnerReferences[i].Kind == "Job" {
			owner = &pod.OwnerReferences[i]
			break
		}
	}
	if owner == nil {
		return nil, ""
	}
	if p.dynamic == nil {
		return nil, contextReasonClientUnavailable
	}
	job, err := p.dynamic.Resource(batchJobs).Namespace(pod.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil {
		return nil, classifyContextError(ctx, err)
	}
	if owner.UID != "" && job.GetUID() != owner.UID {
		// Same name, different object: a Job re-created under the old name
		// does not describe this Pod, and saying so is the context.
		return nil, contextReasonOwnerReplaced
	}
	view := map[string]any{"name": owner.Name}
	if v, found, err := unstructured.NestedInt64(job.Object, "spec", "completions"); err == nil && found {
		view["completions"] = v
	}
	if v, found, err := unstructured.NestedInt64(job.Object, "spec", "parallelism"); err == nil && found {
		view["parallelism"] = v
	}
	if v, found, err := unstructured.NestedString(job.Object, "spec", "completionMode"); err == nil && found {
		view["completion_mode"] = v
		view["indexed"] = v == "Indexed"
	}
	if v, found, err := unstructured.NestedBool(job.Object, "spec", "suspend"); err == nil && found {
		view["suspend"] = v
	}
	if v, found, err := unstructured.NestedInt64(job.Object, "status", "active"); err == nil && found {
		view["active"] = v
	}
	return marshalContext(view)
}

// resolveKueue reads the Kueue Workload admitting the Pod's owning Job:
// queue name from the Pod or Job label, the Workload by the job-uid label,
// and its admission (ClusterQueue), priority and admitted time. The Workload
// is only ever looked up by the Pod's controller owner UID: a queue-labelled
// Pod with no controller owner names its queue and nothing else, because an
// unfiltered namespace listing would attribute whichever Workload came
// first — another team's — to this Pod.
func (p *Platform) resolveKueue(ctx context.Context, pod *corev1.Pod) (json.RawMessage, string) {
	queue := pod.Labels[kueueQueueLabel]
	var ownerUID string
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			ownerUID = string(ref.UID)
			break
		}
	}
	if queue == "" && ownerUID == "" {
		return nil, ""
	}
	if ownerUID == "" {
		return marshalContext(map[string]any{"local_queue": queue, "workload": "", "active": false, "workload_lookup": contextReasonNoOwner})
	}
	if p.dynamic == nil {
		return nil, contextReasonClientUnavailable
	}
	opts := metav1.ListOptions{Limit: contextListCap, LabelSelector: kueueJobUIDLbl + "=" + ownerUID}
	list, err := p.dynamic.Resource(kueueWorkloads).Namespace(pod.Namespace).List(ctx, opts)
	if err != nil && (apierrors.IsNotFound(err) || meta.IsNoMatchError(err)) {
		// An older Kueue that serves v1beta1 only.
		list, err = p.dynamic.Resource(kueueWorkloadsLegacy).Namespace(pod.Namespace).List(ctx, opts)
	}
	if err != nil {
		return nil, classifyContextError(ctx, err)
	}
	view := map[string]any{"local_queue": queue}
	if len(list.Items) == 0 {
		view["workload"] = ""
		view["active"] = false
		return marshalContext(view)
	}
	wl := list.Items[0].Object
	view["workload"] = list.Items[0].GetName()
	if v, found, err := unstructured.NestedString(wl, "spec", "queueName"); err == nil && found && v != "" {
		view["local_queue"] = v
	}
	if v, found, err := unstructured.NestedInt64(wl, "spec", "priority"); err == nil && found {
		view["priority"] = v
	}
	if v, found, err := unstructured.NestedString(wl, "status", "admission", "clusterQueue"); err == nil && found {
		view["cluster_queue"] = v
	}
	if v, found, err := unstructured.NestedBool(wl, "spec", "active"); err == nil && found {
		view["active"] = v
	} else {
		view["active"] = true
	}
	if conditions, found, err := unstructured.NestedSlice(wl, "status", "conditions"); err == nil && found {
		for _, c := range conditions {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] == "Admitted" && m["status"] == "True" {
				if at, ok := m["lastTransitionTime"].(string); ok {
					view["admitted_at"] = at
				}
			}
		}
	}
	return marshalContext(view)
}

// resolveRay reads the Pod's Ray labels and, when the cluster object exists,
// confirms it; a RayJob is named from the originated-from labels on the Pod
// or, failing that, on the cluster (or the cluster's RayJob owner).
func (p *Platform) resolveRay(ctx context.Context, pod *corev1.Pod) (json.RawMessage, string) {
	cluster := pod.Labels[rayClusterLabel]
	if cluster == "" {
		return nil, ""
	}
	view := map[string]any{
		"cluster":   cluster,
		"node_type": pod.Labels[rayNodeTypeLbl],
		"group":     pod.Labels[rayGroupLabel],
	}
	if pod.Labels[rayOriginCRD] == "RayJob" && pod.Labels[rayOriginName] != "" {
		view["ray_job"] = pod.Labels[rayOriginName]
	}
	if p.dynamic == nil {
		return nil, contextReasonClientUnavailable
	}
	rc, err := p.dynamic.Resource(rayClusters).Namespace(pod.Namespace).Get(ctx, cluster, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) && !isGroupUnavailable(err) {
			view["cluster_found"] = false
			return marshalContext(view)
		}
		return nil, classifyContextError(ctx, err)
	}
	view["cluster_found"] = true
	// KubeRay (1.7) stamps the originated-from labels on the RayCluster a
	// RayJob creates, not on that cluster's Pods; the cluster also carries an
	// owner reference to the RayJob. Either names the job when the Pod did not.
	if _, named := view["ray_job"]; !named {
		labels := rc.GetLabels()
		if labels[rayOriginCRD] == "RayJob" && labels[rayOriginName] != "" {
			view["ray_job"] = labels[rayOriginName]
		} else {
			for _, owner := range rc.GetOwnerReferences() {
				if owner.Kind == "RayJob" && owner.Name != "" {
					view["ray_job"] = owner.Name
					break
				}
			}
		}
	}
	if name, ok := view["ray_job"].(string); ok && name != "" {
		if _, err := p.dynamic.Resource(rayJobs).Namespace(pod.Namespace).Get(ctx, name, metav1.GetOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return nil, classifyContextError(ctx, err)
		}
	}
	return marshalContext(view)
}

// isGroupUnavailable tells "the group/version is not served" from "the named
// object does not exist": the apiserver answers both with NotFound, but the
// former carries no resource kind in its status details.
func isGroupUnavailable(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	details := status.Status().Details
	return details == nil || details.Kind == ""
}

func marshalContext(view map[string]any) (json.RawMessage, string) {
	raw, err := json.Marshal(view)
	if err != nil {
		return nil, contextReasonMalformed
	}
	return raw, ""
}
