// SPDX-License-Identifier: LicenseRef-KubeNeuron

package kubernetes

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/kubeneuron/kubeneuron/internal/topology"
)

// This file is the Kubernetes side of the device topology classifier's
// inputs: the node's advertised extended resources, and the opt-in read-only
// observation of Dynamic Resource Allocation (the node's ResourceSlices and
// the claim references of the Pods scheduled on it). Nothing here writes,
// and nothing here is consulted by any node-scoped rung.

// NodeResources implements platform.NodeResourceObserver: the node's own
// status.capacity and status.allocatable, restricted to domain-qualified
// extended resources (the only names that can describe a device) and read
// the same way NodeLabels is — the watch-maintained cache first, a live Get
// when the cache is not serving. Quantities are node-written and are passed
// through as saturating int64 values; the classifier applies the clamp.
func (p *Platform) NodeResources(ctx context.Context, node string) (topology.Resources, bool, error) {
	if lp := p.nodeLister.Load(); lp != nil {
		if n, err := (*lp).Get(node); err == nil {
			return nodeResources(n), true, nil
		} else if apierrors.IsNotFound(err) {
			return topology.Resources{}, false, nil
		}
		// Any other lister error falls through to the live read.
	}
	n, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return topology.Resources{}, false, nil
	}
	if err != nil {
		return topology.Resources{}, false, err
	}
	return nodeResources(n), true, nil
}

func nodeResources(n *corev1.Node) topology.Resources {
	return topology.Resources{
		Source:      topology.ResourceSourceNode,
		Capacity:    extendedResourceValues(n.Status.Capacity),
		Allocatable: extendedResourceValues(n.Status.Allocatable),
	}
}

// extendedResourceValues keeps only domain-qualified names. Core resources
// (cpu, memory, pods, ephemeral-storage, hugepages-*) never describe a device
// and would only churn the classifier's inputs digest.
func extendedResourceValues(list corev1.ResourceList) map[string]int64 {
	out := make(map[string]int64, len(list))
	for name, quantity := range list {
		if !strings.Contains(string(name), "/") {
			continue
		}
		out[string(name)] = quantity.Value()
	}
	return out
}

// Dynamic Resource Allocation is read ONLY at resource.k8s.io/v1 and ONLY
// through list. The pinned client libraries (k8s.io/api v0.33) carry no typed
// v1 client for the group, so the read goes through the dynamic client as
// unstructured objects and takes nothing from them but bounded counts and
// validated driver names. v1beta1/v1beta2/v1alpha3 are deliberately not
// consulted: a cluster serving only a beta version is reported as not
// observed, which the classifier refuses. Slice locality is modelled only
// for an explicit spec.nodeName (see sliceLocality); any other locality
// shape is reported as not observed rather than filtered out.
//
// ResourceClaim use is NOT read from resource.k8s.io at all. A claim is in
// use on a node exactly when a Pod scheduled there references one, so the
// observation lists the node's Pods (core/v1, the get/list/watch the
// controller already holds) and inspects spec.resourceClaims. That keeps the
// conditional RBAC grant at resourceslices alone: no resourceclaims or
// resourceclaimtemplates read is ever taken or granted, and nothing in the
// group is ever created, patched, updated, deleted or bound.
var (
	draGroupVersion   = schema.GroupVersion{Group: "resource.k8s.io", Version: "v1"}
	draResourceSlices = draGroupVersion.WithResource("resourceslices")
)

// draObservationTimeout bounds one observation. The read sits on the
// readiness and admission paths, both of which already tolerate an apiserver
// round trip; it must never hold either for longer than that.
const draObservationTimeout = 5 * time.Second

// Stable reasons for a not-observed result. They are rendered in the
// classification, so they are short tokens (see topology.MaxDRAReasonLength)
// and carry no error text and no object data.
const (
	draReasonClientUnavailable = "dynamic-client-unavailable"
	draReasonAPIUnavailable    = "api-unavailable"
	draReasonForbidden         = "forbidden"
	draReasonTimeout           = "timeout"
	draReasonListFailed        = "list-failed"
	draReasonMalformed         = "malformed-object"
	draReasonPodsForbidden     = "pods-forbidden"
	draReasonPodsListFailed    = "pods-list-failed"
	draReasonMalformedClaim    = "malformed-claim-reference"
	// draReasonLocalityUnsupported: a slice is scoped by nodeSelector,
	// allNodes or perDeviceNodeSelection, which this version does not
	// evaluate and which may include the node.
	draReasonLocalityUnsupported = "locality-unsupported"
	// draReasonLocalityInvalid: a slice names no locality mechanism, more
	// than one, or a nodeName that is empty or not a DNS subdomain, so it
	// cannot be placed anywhere.
	draReasonLocalityInvalid = "locality-invalid"
	// draReasonDriverInvalid: a slice for the node carries no spec.driver or
	// one that is not a DNS subdomain.
	draReasonDriverInvalid = "driver-invalid"
	// draReasonBoundsExceeded: more slices, devices, drivers or claims than
	// the topology package's MaxDRA* bounds admit.
	draReasonBoundsExceeded = "bounds-exceeded"
)

// ObserveDRA implements platform.DRAObserver. It never returns an error: a
// read that cannot be performed is a not-observed observation with a reason,
// and the classifier turns that into a refusal. Exactly two reads make one
// observation, both under one draObservationTimeout context, and both must
// succeed: the node's ResourceSlices, then the node's Pods for their claim
// references. No discovery call precedes them. A Pod list that fails, or a claim
// reference that is not well formed, poisons the whole observation; a
// successful slice read is never reported as observed on its own. The
// caller is expected to invoke it only when the installation enabled DRA
// discovery; the platform does not know the configuration and would read on
// every call.
func (p *Platform) ObserveDRA(ctx context.Context, node string) topology.DRAObservation {
	notObserved := func(reason string) topology.DRAObservation {
		return topology.DRAObservation{State: topology.DRANotObserved, Reason: reason, APIVersion: draGroupVersion.String()}
	}
	if p == nil || p.dynamic == nil || p.client == nil {
		return notObserved(draReasonClientUnavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, draObservationTimeout)
	defer cancel()

	// There is deliberately no discovery round trip ahead of the list: the
	// discovery client takes no context, so it could outlive
	// draObservationTimeout and hold the readiness or admission path with
	// it. The v1 list under the bounded context is the whole probe. An
	// apiserver that does not serve resource.k8s.io/v1 (none at all, or a
	// beta version only) answers the list with NotFound, which is mapped to
	// api-unavailable below; one that serves v1 but refuses this service
	// account answers Forbidden. Both are told apart without a second read.
	//
	// Every v1 slice is listed, deliberately without the spec.nodeName field
	// selector: a slice scoped by nodeSelector, allNodes or
	// perDeviceNodeSelection has no nodeName, and a server-side selector
	// would hide it and let the node read as "no DRA here". Locality is
	// decided client-side per slice by sliceLocality.
	list, err := p.dynamic.Resource(draResourceSlices).List(ctx, metav1.ListOptions{})
	if err != nil {
		switch {
		case apierrors.IsForbidden(err):
			return notObserved(draReasonForbidden)
		case apierrors.IsNotFound(err):
			return notObserved(draReasonAPIUnavailable)
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || apierrors.IsTimeout(err):
			return notObserved(draReasonTimeout)
		default:
			return notObserved(draReasonListFailed)
		}
	}
	out := topology.DRAObservation{State: topology.DRAObserved, APIVersion: draGroupVersion.String()}
	drivers := map[string]struct{}{}
	for i := range list.Items {
		slice := &list.Items[i]
		match, reason := sliceLocality(slice.Object, node)
		if reason != "" {
			return notObserved(reason)
		}
		if !match {
			continue
		}
		out.ResourceSlices++
		devices, found, err := unstructured.NestedSlice(slice.Object, "spec", "devices")
		if err != nil {
			return notObserved(draReasonMalformed)
		}
		if found {
			out.Devices += len(devices)
		}
		driver, found, err := unstructured.NestedString(slice.Object, "spec", "driver")
		if err != nil {
			return notObserved(draReasonMalformed)
		}
		// spec.driver is required and must be a DNS subdomain; it is the only
		// string taken from a slice, and only in that shape.
		if !found || len(validation.IsDNS1123Subdomain(driver)) != 0 {
			return notObserved(draReasonDriverInvalid)
		}
		drivers[driver] = struct{}{}
		// Bounds are checked as the counts grow, so the sum can never run
		// past them however many slices the cluster carries.
		if out.ResourceSlices > topology.MaxDRACount || out.Devices > topology.MaxDRACount || len(drivers) > topology.MaxDRADrivers {
			return notObserved(draReasonBoundsExceeded)
		}
	}
	for driver := range drivers {
		out.Drivers = append(out.Drivers, driver)
	}
	sort.Strings(out.Drivers)

	// Claim references: a read-only core Pod list restricted to this node,
	// re-checked client-side like the slices. Every Pod scheduled here counts,
	// whatever its phase: a claim referenced by a Pod that has not been
	// deleted yet may still be allocated, and the conservative reading is the
	// only safe one.
	pods, err := p.nodePods(ctx, node)
	if err != nil {
		switch {
		case apierrors.IsForbidden(err):
			return notObserved(draReasonPodsForbidden)
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || apierrors.IsTimeout(err):
			return notObserved(draReasonTimeout)
		default:
			return notObserved(draReasonPodsListFailed)
		}
	}
	for i := range pods {
		pod := &pods[i]
		if pod.Spec.NodeName != node {
			continue
		}
		direct, template, ok := podClaimReferences(pod)
		if !ok {
			return notObserved(draReasonMalformedClaim)
		}
		if direct+template > 0 {
			out.ClaimPods++
			out.DirectClaims += direct
			out.TemplateClaims += template
		}
		if out.ClaimPods > topology.MaxDRACount || out.DirectClaims > topology.MaxDRACount || out.TemplateClaims > topology.MaxDRACount {
			return notObserved(draReasonBoundsExceeded)
		}
	}
	out.PodsInspected = true
	return out
}

// sliceLocality says whether one v1 ResourceSlice is published for node. The
// v1 API requires exactly one of spec.nodeName, spec.nodeSelector,
// spec.allNodes and spec.perDeviceNodeSelection, and this version models
// only the first: a slice naming the target is evidence, and a slice naming
// another node (and nothing else) is ignored. Every other shape — a
// selector, allNodes, per-device selection, no mechanism at all, an empty
// or invalid name, more than one mechanism — may reach this node without
// the classifier being able to tell, so it ends the observation with a
// stable reason instead of being counted as nothing. A nodeName is only
// compared once it is a DNS subdomain, as the API requires of node names;
// a malformed one is never taken for "some other node". A field of the
// wrong type is malformed. No selector is evaluated and nothing but the
// node name is compared.
func sliceLocality(obj map[string]any, node string) (match bool, reason string) {
	nodeName, hasNodeName, err := unstructured.NestedString(obj, "spec", "nodeName")
	if err != nil {
		return false, draReasonMalformed
	}
	_, hasSelector, err := unstructured.NestedMap(obj, "spec", "nodeSelector")
	if err != nil {
		return false, draReasonMalformed
	}
	allNodes, _, err := unstructured.NestedBool(obj, "spec", "allNodes")
	if err != nil {
		return false, draReasonMalformed
	}
	perDevice, _, err := unstructured.NestedBool(obj, "spec", "perDeviceNodeSelection")
	if err != nil {
		return false, draReasonMalformed
	}
	mechanisms := 0
	for _, set := range []bool{hasNodeName, hasSelector, allNodes, perDevice} {
		if set {
			mechanisms++
		}
	}
	switch {
	case mechanisms != 1:
		return false, draReasonLocalityInvalid
	case !hasNodeName:
		return false, draReasonLocalityUnsupported
	case nodeName == "" || len(validation.IsDNS1123Subdomain(nodeName)) != 0:
		return false, draReasonLocalityInvalid
	case nodeName == node:
		return true, ""
	default:
		return false, ""
	}
}

// podClaimReferences counts the ResourceClaim references in one Pod's
// spec.resourceClaims and reports whether every entry is well formed: a
// DNS-label name and exactly one of resourceClaimName /
// resourceClaimTemplateName, itself a non-empty DNS subdomain, with every
// container-level claim naming one of those entries. The apiserver enforces
// the same shape on admission, so any other form is unexpected data that
// cannot be read as "no claim". Only counts leave this function: no name
// does.
func podClaimReferences(pod *corev1.Pod) (direct, template int, ok bool) {
	names := make(map[string]struct{}, len(pod.Spec.ResourceClaims))
	for i := range pod.Spec.ResourceClaims {
		claim := &pod.Spec.ResourceClaims[i]
		if len(validation.IsDNS1123Label(claim.Name)) != 0 {
			return 0, 0, false
		}
		if _, dup := names[claim.Name]; dup {
			return 0, 0, false
		}
		names[claim.Name] = struct{}{}
		switch {
		case claim.ResourceClaimName != nil && claim.ResourceClaimTemplateName != nil:
			return 0, 0, false
		case claim.ResourceClaimName != nil:
			if len(validation.IsDNS1123Subdomain(*claim.ResourceClaimName)) != 0 {
				return 0, 0, false
			}
			direct++
		case claim.ResourceClaimTemplateName != nil:
			if len(validation.IsDNS1123Subdomain(*claim.ResourceClaimTemplateName)) != 0 {
				return 0, 0, false
			}
			template++
		default:
			return 0, 0, false
		}
	}
	for _, claims := range containerClaims(pod) {
		for _, claim := range claims {
			if _, known := names[claim.Name]; !known {
				return 0, 0, false
			}
		}
	}
	return direct, template, true
}

// containerClaims gathers resources.claims across every container kind so a
// dangling container reference is seen whatever container carries it.
func containerClaims(pod *corev1.Pod) [][]corev1.ResourceClaim {
	out := make([][]corev1.ResourceClaim, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers)+len(pod.Spec.EphemeralContainers))
	for i := range pod.Spec.InitContainers {
		out = append(out, pod.Spec.InitContainers[i].Resources.Claims)
	}
	for i := range pod.Spec.Containers {
		out = append(out, pod.Spec.Containers[i].Resources.Claims)
	}
	for i := range pod.Spec.EphemeralContainers {
		out = append(out, pod.Spec.EphemeralContainers[i].Resources.Claims)
	}
	return out
}
