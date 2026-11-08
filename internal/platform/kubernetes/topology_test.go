package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubeneuron/kubeneuron/internal/topology"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func resourceSlice(name, node, driver string, devices int) *unstructured.Unstructured {
	list := make([]any, 0, devices)
	for i := 0; i < devices; i++ {
		list = append(list, map[string]any{"name": fmt.Sprintf("gpu-%d", i)})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "resource.k8s.io/v1",
		"kind":       "ResourceSlice",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"nodeName": node,
			"driver":   driver,
			"pool":     map[string]any{"name": node, "generation": int64(1), "resourceSliceCount": int64(1)},
			"devices":  list,
		},
	}}
}

// claimPod is a Pod scheduled on node with the given spec.resourceClaims
// entries, each container-level claim naming an entry so the Pod is the
// shape the apiserver admits.
func claimPod(namespace, name, node string, claims ...corev1.PodResourceClaim) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.PodSpec{NodeName: node, ResourceClaims: claims, Containers: []corev1.Container{{Name: "main"}}},
	}
	for _, claim := range claims {
		pod.Spec.Containers[0].Resources.Claims = append(pod.Spec.Containers[0].Resources.Claims, corev1.ResourceClaim{Name: claim.Name})
	}
	return pod
}

func directClaim(entry, claim string) corev1.PodResourceClaim {
	return corev1.PodResourceClaim{Name: entry, ResourceClaimName: &claim}
}

func templateClaim(entry, template string) corev1.PodResourceClaim {
	return corev1.PodResourceClaim{Name: entry, ResourceClaimTemplateName: &template}
}

// draTestPlatform wires a fake core clientset with no Pods on any node and a
// fake dynamic client that serves resource.k8s.io/v1 resourceslices holding
// the given objects. Nothing is wired for discovery: the observation must not
// take one, and the fake clientset's discovery answers would go unaudited.
func draTestPlatform(objects ...runtime.Object) (*Platform, *dynamicfake.FakeDynamicClient) {
	return draTestPlatformWithPods(nil, objects...)
}

// draTestPlatformWithPods is draTestPlatform with the given Pods in the core
// fake clientset.
func draTestPlatformWithPods(pods []*corev1.Pod, objects ...runtime.Object) (*Platform, *dynamicfake.FakeDynamicClient) {
	core := make([]runtime.Object, 0, len(pods))
	for _, pod := range pods {
		core = append(core, pod)
	}
	client := fake.NewSimpleClientset(core...)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{draResourceSlices: "ResourceSliceList"}, objects...)
	return &Platform{client: client, dynamic: dyn}, dyn
}

// withoutV1ResourceSlices makes the dynamic client answer the v1
// resourceslices list the way a real apiserver answers a request for a
// group/version or resource it does not serve: 404 NotFound. That is the
// one signal a beta-only or DRA-less cluster gives a v1 client, and the
// observation must read it as api-unavailable without any discovery call.
func withoutV1ResourceSlices(dyn *dynamicfake.FakeDynamicClient) {
	dyn.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(draResourceSlices.GroupResource(), "")
	})
}

func TestObserveDRAReadsOnlyV1AndOnlyThisNode(t *testing.T) {
	p, dyn := draTestPlatform(
		resourceSlice("n1-nvidia", "n1", "gpu.nvidia.com", 8),
		resourceSlice("n1-other", "n1", "accel.example.com", 2),
		resourceSlice("n2-nvidia", "n2", "gpu.nvidia.com", 8),
	)
	got := p.ObserveDRA(context.Background(), "n1")
	want := topology.DRAObservation{State: topology.DRAObserved, APIVersion: "resource.k8s.io/v1", ResourceSlices: 2, Devices: 10, Drivers: []string{"accel.example.com", "gpu.nvidia.com"}, PodsInspected: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observation = %+v, want %+v", got, want)
	}
	if got.APIVersion != topology.DRAAPIVersion {
		t.Fatalf("observation claims %q, the classifier accepts only %q", got.APIVersion, topology.DRAAPIVersion)
	}
	// Every read is a list of resourceslices at v1 and nothing else: no get,
	// no write, no other resource.
	for _, action := range dyn.Actions() {
		if action.GetVerb() != "list" || action.GetResource() != draResourceSlices {
			t.Fatalf("unexpected dynamic action %s %v", action.GetVerb(), action.GetResource())
		}
	}
	if len(dyn.Actions()) != 1 {
		t.Fatalf("dynamic actions = %d, want exactly one list", len(dyn.Actions()))
	}
	// A node with no slices and no Pods is observed as empty (with its Pods
	// inspected), not as not-observed.
	if got := p.ObserveDRA(context.Background(), "n3"); got.State != topology.DRAObserved || got.ResourceSlices != 0 || got.Devices != 0 || got.Drivers != nil || !got.PodsInspected || got.ClaimPods != 0 {
		t.Fatalf("node without slices = %+v, want an empty observation", got)
	}
	// The observation is what the classifier consumes: slices for the node
	// refuse as DRA-managed, an empty observation changes nothing.
	now := time.Now().UTC()
	snapshot := topology.Snapshot{
		Node: "n1", EvaluatedAt: now,
		Resources: topology.Resources{Source: topology.ResourceSourceNode, Capacity: map[string]int64{"nvidia.com/gpu": 1}},
		Report:    &topology.Report{ObservedAt: now.Add(-time.Second), TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned, Readiness: types.AcceleratorReadinessReady, PhysicalDevices: 1},
		DRA:       p.ObserveDRA(context.Background(), "n1"),
	}
	if c := topology.Classify(snapshot); c.Topology != topology.TopologyMixed || !c.Has(topology.ReasonDraManaged) {
		t.Fatalf("classified = %s, want mixed/DraManaged", c.Summary)
	}
	snapshot.DRA = p.ObserveDRA(context.Background(), "n3")
	snapshot.Node = "n3"
	if c := topology.Classify(snapshot); !c.Eligible() {
		t.Fatalf("classified = %s, want eligible with an empty v1 observation", c.Summary)
	}
}

// TestObserveDRAIsNotObservedWhenV1IsNotServed: a cluster serving no DRA, a
// beta version only, or v1 without the resourceslices resource answers the
// v1 list with NotFound. Every such cluster is reported not-observed with
// the stable api-unavailable reason from that one list alone: no discovery
// call is taken (the discovery client has no context and could outlive the
// observation's deadline), no beta version is tried, and the Pod list never
// runs. The classifier refuses the result. The table names the cluster
// shapes; a v1 client cannot tell them apart, and does not need to.
func TestObserveDRAIsNotObservedWhenV1IsNotServed(t *testing.T) {
	for _, name := range []string{
		"no DRA at all",
		"v1beta1 only",
		"v1beta2 and v1alpha3",
		"unrelated group only",
		"v1 without resourceslices",
	} {
		t.Run(name, func(t *testing.T) {
			p, dyn := draTestPlatform(resourceSlice("n1-nvidia", "n1", "gpu.nvidia.com", 8))
			withoutV1ResourceSlices(dyn)
			got := p.ObserveDRA(context.Background(), "n1")
			want := topology.DRAObservation{State: topology.DRANotObserved, Reason: draReasonAPIUnavailable, APIVersion: "resource.k8s.io/v1"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("observation = %+v, want %+v", got, want)
			}
			// Exactly one dynamic action, the v1 resourceslices list that
			// answered NotFound; nothing at any other version.
			if actions := dyn.Actions(); len(actions) != 1 || actions[0].GetVerb() != "list" || actions[0].GetResource() != draResourceSlices {
				t.Fatalf("dynamic actions = %v, want exactly one list of %v", actions, draResourceSlices)
			}
			// No discovery and no Pod list: the core clientset was never
			// asked anything.
			if actions := p.client.(*fake.Clientset).Actions(); len(actions) != 0 {
				t.Fatalf("core client was consulted for an unserved API: %v", actions)
			}
			if c := topology.Classify(topology.Snapshot{Node: "n1", EvaluatedAt: time.Now(), DRA: got}); c.Eligible() || !c.Has(topology.ReasonTopologyUnknown) {
				t.Fatalf("classified = %s, want a refusal", c.Summary)
			}
		})
	}
}

func TestObserveDRAFailsClosedOnEveryReadFailure(t *testing.T) {
	ctx := context.Background()
	// No dynamic client (a Platform built outside New), and a nil Platform.
	bare := &Platform{client: fake.NewSimpleClientset()}
	if got := bare.ObserveDRA(ctx, "n1"); got.State != topology.DRANotObserved || got.Reason != draReasonClientUnavailable {
		t.Fatalf("no dynamic client = %+v", got)
	}
	var nilPlatform *Platform
	if got := nilPlatform.ObserveDRA(ctx, "n1"); got.State != topology.DRANotObserved || got.Reason != draReasonClientUnavailable {
		t.Fatalf("nil platform = %+v", got)
	}

	gr := schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"}
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"forbidden":   {err: apierrors.NewForbidden(gr, "", errors.New("denied")), reason: draReasonForbidden},
		"not found":   {err: apierrors.NewNotFound(gr, ""), reason: draReasonAPIUnavailable},
		"timeout":     {err: apierrors.NewTimeoutError("too slow", 1), reason: draReasonTimeout},
		"deadline":    {err: context.DeadlineExceeded, reason: draReasonTimeout},
		"other error": {err: errors.New("connection reset"), reason: draReasonListFailed},
	} {
		t.Run(name, func(t *testing.T) {
			p, dyn := draTestPlatform(resourceSlice("n1-nvidia", "n1", "gpu.nvidia.com", 8))
			dyn.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			got := p.ObserveDRA(ctx, "n1")
			if got.State != topology.DRANotObserved || got.Reason != tc.reason {
				t.Fatalf("observation = %+v, want not-observed/%s", got, tc.reason)
			}
			if got.ResourceSlices != 0 || got.Devices != 0 || got.Drivers != nil {
				t.Fatalf("a failed read must carry no counts: %+v", got)
			}
			if c := topology.Classify(topology.Snapshot{Node: "n1", EvaluatedAt: time.Now(), DRA: got}); c.Eligible() {
				t.Fatalf("classified = %s, want a refusal", c.Summary)
			}
		})
	}

}

// TestObserveDRARefusesMalformedSlices: one object whose spec does not have
// the v1 shape poisons the whole observation rather than being skipped — a
// partial count could read as "nothing for this node".
func TestObserveDRARefusesMalformedSlices(t *testing.T) {
	for name, mutate := range map[string]func(*unstructured.Unstructured){
		"nodeName is not a string": func(u *unstructured.Unstructured) {
			u.Object["spec"].(map[string]any)["nodeName"] = map[string]any{"name": "n1"}
		},
		"devices is not a list": func(u *unstructured.Unstructured) {
			u.Object["spec"].(map[string]any)["devices"] = "eight"
		},
		"driver is not a string": func(u *unstructured.Unstructured) {
			u.Object["spec"].(map[string]any)["driver"] = int64(7)
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := resourceSlice("n1-bad", "n1", "gpu.nvidia.com", 8)
			mutate(bad)
			p, _ := draTestPlatform(resourceSlice("n1-good", "n1", "gpu.nvidia.com", 8), bad)
			got := p.ObserveDRA(context.Background(), "n1")
			if got.State != topology.DRANotObserved || got.Reason != draReasonMalformed {
				t.Fatalf("observation = %+v, want not-observed/%s", got, draReasonMalformed)
			}
		})
	}
	// A slice explicitly named for another node is never read past its
	// locality: a malformed devices list or driver on it cannot poison this
	// node's observation.
	for name, mutate := range map[string]func(*unstructured.Unstructured){
		"devices is not a list":  func(u *unstructured.Unstructured) { u.Object["spec"].(map[string]any)["devices"] = "eight" },
		"driver is not a string": func(u *unstructured.Unstructured) { u.Object["spec"].(map[string]any)["driver"] = int64(7) },
		"driver is hostile":      func(u *unstructured.Unstructured) { u.Object["spec"].(map[string]any)["driver"] = "../../etc\x00" },
	} {
		t.Run("other node "+name, func(t *testing.T) {
			other := resourceSlice("n2-bad", "n2", "gpu.nvidia.com", 8)
			mutate(other)
			p, _ := draTestPlatform(resourceSlice("n1-good", "n1", "gpu.nvidia.com", 8), other)
			got := p.ObserveDRA(context.Background(), "n1")
			want := topology.DRAObservation{State: topology.DRAObserved, APIVersion: "resource.k8s.io/v1", ResourceSlices: 1, Devices: 8, Drivers: []string{"gpu.nvidia.com"}, PodsInspected: true}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("observation = %+v, want %+v", got, want)
			}
		})
	}
}

// TestObserveDRAFailsClosedOnUnmodelledLocality: Kubernetes v1 requires one
// of spec.nodeName, nodeSelector, allNodes or perDeviceNodeSelection on every
// ResourceSlice, and only an explicit nodeName is modelled. A slice scoped any
// other way may well include the node, so it is reported as not observed with
// a stable reason — never filtered out into an "empty" observation. A slice
// explicitly for another node is the one shape that is ignored.
func TestObserveDRAFailsClosedOnUnmodelledLocality(t *testing.T) {
	spec := func(u *unstructured.Unstructured) map[string]any { return u.Object["spec"].(map[string]any) }
	unmodelled := func(mutate func(map[string]any)) *unstructured.Unstructured {
		u := resourceSlice("pool-wide", "", "gpu.nvidia.com", 8)
		delete(spec(u), "nodeName")
		mutate(spec(u))
		return u
	}
	selector := map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{
		map[string]any{"key": "kubernetes.io/hostname", "operator": "In", "values": []any{"n1"}},
	}}}}
	for name, tc := range map[string]struct {
		slice  *unstructured.Unstructured
		reason string
	}{
		"allNodes":               {unmodelled(func(s map[string]any) { s["allNodes"] = true }), draReasonLocalityUnsupported},
		"nodeSelector":           {unmodelled(func(s map[string]any) { s["nodeSelector"] = selector }), draReasonLocalityUnsupported},
		"empty nodeSelector":     {unmodelled(func(s map[string]any) { s["nodeSelector"] = map[string]any{} }), draReasonLocalityUnsupported},
		"perDeviceNodeSelection": {unmodelled(func(s map[string]any) { s["perDeviceNodeSelection"] = true }), draReasonLocalityUnsupported},
		"no locality at all":     {unmodelled(func(map[string]any) {}), draReasonLocalityInvalid},
		"allNodes false only":    {unmodelled(func(s map[string]any) { s["allNodes"] = false }), draReasonLocalityInvalid},
		"empty nodeName":         {unmodelled(func(s map[string]any) { s["nodeName"] = "" }), draReasonLocalityInvalid},
		// A nodeName that is not a valid node name is not "some other node":
		// it is a malformed locality and must fail closed, not be ignored.
		"nodeName with whitespace":    {unmodelled(func(s map[string]any) { s["nodeName"] = "n 2" }), draReasonLocalityInvalid},
		"nodeName with slash":         {unmodelled(func(s map[string]any) { s["nodeName"] = "n2/gpu" }), draReasonLocalityInvalid},
		"uppercase nodeName":          {unmodelled(func(s map[string]any) { s["nodeName"] = "N2" }), draReasonLocalityInvalid},
		"nodeName leading hyphen":     {unmodelled(func(s map[string]any) { s["nodeName"] = "-n2" }), draReasonLocalityInvalid},
		"nodeName trailing hyphen":    {unmodelled(func(s map[string]any) { s["nodeName"] = "n2-" }), draReasonLocalityInvalid},
		"nodeName leading dot":        {unmodelled(func(s map[string]any) { s["nodeName"] = ".n2" }), draReasonLocalityInvalid},
		"nodeName trailing dot":       {unmodelled(func(s map[string]any) { s["nodeName"] = "n2." }), draReasonLocalityInvalid},
		"nodeName over 253 bytes":     {unmodelled(func(s map[string]any) { s["nodeName"] = strings.Repeat("a", 254) }), draReasonLocalityInvalid},
		"whitespace-only nodeName":    {unmodelled(func(s map[string]any) { s["nodeName"] = " " }), draReasonLocalityInvalid},
		"target name with whitespace": {unmodelled(func(s map[string]any) { s["nodeName"] = "n1 " }), draReasonLocalityInvalid},
		"nodeName is null":            {unmodelled(func(s map[string]any) { s["nodeName"] = nil }), draReasonMalformed},
		"this node and allNodes":      {unmodelled(func(s map[string]any) { s["nodeName"] = "n1"; s["allNodes"] = true }), draReasonLocalityInvalid},
		"other node and selector": {unmodelled(func(s map[string]any) {
			s["nodeName"] = "n2"
			s["nodeSelector"] = selector
		}), draReasonLocalityInvalid},
		"selector and allNodes": {unmodelled(func(s map[string]any) {
			s["nodeSelector"] = selector
			s["allNodes"] = true
		}), draReasonLocalityInvalid},
		"allNodes is not a bool":    {unmodelled(func(s map[string]any) { s["allNodes"] = "yes" }), draReasonMalformed},
		"nodeSelector is not a map": {unmodelled(func(s map[string]any) { s["nodeSelector"] = "n1" }), draReasonMalformed},
		"perDevice is not a bool":   {unmodelled(func(s map[string]any) { s["perDeviceNodeSelection"] = int64(1) }), draReasonMalformed},
		"other node's allNodes": {unmodelled(func(s map[string]any) {
			// Named for n2 but also allNodes: cannot be placed, so it is not
			// "n2's problem".
			s["nodeName"] = "n2"
			s["allNodes"] = true
		}), draReasonLocalityInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			// Beside a perfectly good slice for the node, so a partial count is
			// visible if the bad slice were merely skipped.
			p, dyn := draTestPlatformWithPods(
				[]*corev1.Pod{claimPod("ml", "train-0", "n1", directClaim("gpu", "train-0-gpu"))},
				resourceSlice("n1-good", "n1", "gpu.nvidia.com", 8), tc.slice)
			got := p.ObserveDRA(context.Background(), "n1")
			want := topology.DRAObservation{State: topology.DRANotObserved, Reason: tc.reason, APIVersion: "resource.k8s.io/v1"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("observation = %+v, want %+v", got, want)
			}
			// Still read-only: one list of v1 resourceslices, nothing else.
			for _, action := range dyn.Actions() {
				if action.GetVerb() != "list" || action.GetResource() != draResourceSlices {
					t.Fatalf("unexpected dynamic action %s %v", action.GetVerb(), action.GetResource())
				}
			}
			c := topology.Classify(topology.Snapshot{Node: "n1", EvaluatedAt: time.Now(), DRA: got})
			if c.Eligible() || c.Topology != topology.TopologyUnknown || !c.Has(topology.ReasonTopologyUnknown) {
				t.Fatalf("classified = %s, want unknown/refused", c.Summary)
			}
			if c.DRA.State != topology.DRANotObserved || c.DRA.Reason != tc.reason {
				t.Fatalf("rendered dra = %+v, want not-observed/%s", c.DRA, tc.reason)
			}
		})
	}

	// The shapes that remain evidence or are ignored: nodeName equal to the
	// target (an explicit allNodes: false beside it is "not set", as the API
	// reads it), and nodeName naming another node.
	target := resourceSlice("n1-explicit", "n1", "gpu.nvidia.com", 4)
	spec(target)["allNodes"] = false
	spec(target)["perDeviceNodeSelection"] = false
	p, _ := draTestPlatform(target, resourceSlice("n2-nvidia", "n2", "gpu.nvidia.com", 8), resourceSlice("n3-nvidia", "n3", "accel.example.com", 2))
	got := p.ObserveDRA(context.Background(), "n1")
	want := topology.DRAObservation{State: topology.DRAObserved, APIVersion: "resource.k8s.io/v1", ResourceSlices: 1, Devices: 4, Drivers: []string{"gpu.nvidia.com"}, PodsInspected: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observation = %+v, want %+v", got, want)
	}
	// Other nodes' slices alone leave this node observed-empty.
	if got := p.ObserveDRA(context.Background(), "n4"); got.State != topology.DRAObserved || got.ResourceSlices != 0 || got.Drivers != nil || !got.PodsInspected {
		t.Fatalf("only other nodes' slices = %+v, want an empty observation", got)
	}
}

// TestObserveDRABoundsCountsAndValidatesDrivers: spec.driver is the only
// string taken from a slice and leaves the adapter only as a DNS subdomain;
// every count stops at the topology package's bounds. Anything else is
// not-observed with a stable reason and no partial count.
func TestObserveDRABoundsCountsAndValidatesDrivers(t *testing.T) {
	for name, driver := range map[string]any{
		"empty":          "",
		"whitespace":     " gpu.nvidia.com ",
		"uppercase":      "GPU.nvidia.com",
		"control chars":  "gpu.nvidia.com\x00\x1b[31m",
		"too long":       strings.Repeat("a", topology.MaxDRADriverLength+1),
		"path-like":      "../gpu.nvidia.com",
		"with namespace": "ml/train-0",
	} {
		t.Run("driver "+name, func(t *testing.T) {
			bad := resourceSlice("n1-bad", "n1", "", 1)
			bad.Object["spec"].(map[string]any)["driver"] = driver
			p, _ := draTestPlatform(resourceSlice("n1-good", "n1", "gpu.nvidia.com", 8), bad)
			got := p.ObserveDRA(context.Background(), "n1")
			want := topology.DRAObservation{State: topology.DRANotObserved, Reason: draReasonDriverInvalid, APIVersion: "resource.k8s.io/v1"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("observation = %+v, want %+v", got, want)
			}
			if rendered := fmt.Sprintf("%+v", got); strings.Contains(rendered, fmt.Sprint(driver)) && driver != "" {
				t.Fatalf("hostile driver string leaves the adapter: %s", rendered)
			}
		})
	}
	// A missing driver is invalid too, not "no driver".
	missing := resourceSlice("n1-nodriver", "n1", "", 1)
	delete(missing.Object["spec"].(map[string]any), "driver")
	p, _ := draTestPlatform(missing)
	if got := p.ObserveDRA(context.Background(), "n1"); got.State != topology.DRANotObserved || got.Reason != draReasonDriverInvalid {
		t.Fatalf("missing driver = %+v, want not-observed/%s", got, draReasonDriverInvalid)
	}
	// The longest valid driver name passes unchanged.
	longest := strings.Repeat("a", topology.MaxDRADriverLength-4) + ".com"
	p, _ = draTestPlatform(resourceSlice("n1-long", "n1", longest, 1))
	if got := p.ObserveDRA(context.Background(), "n1"); got.State != topology.DRAObserved || !reflect.DeepEqual(got.Drivers, []string{longest}) {
		t.Fatalf("longest valid driver = %+v, want it kept", got)
	}

	var (
		tooManyDrivers = make([]runtime.Object, 0, topology.MaxDRADrivers+1)
		tooManySlices  = make([]runtime.Object, 0, topology.MaxDRACount+1)
	)
	for i := 0; i <= topology.MaxDRADrivers; i++ {
		tooManyDrivers = append(tooManyDrivers, resourceSlice(fmt.Sprintf("n1-driver-%d", i), "n1", fmt.Sprintf("driver-%d.example.com", i), 1))
	}
	for i := 0; i <= topology.MaxDRACount; i++ {
		tooManySlices = append(tooManySlices, resourceSlice(fmt.Sprintf("n1-slice-%d", i), "n1", "gpu.nvidia.com", 0))
	}
	tooManyPods := make([]*corev1.Pod, 0, topology.MaxDRACount+1)
	for i := 0; i <= topology.MaxDRACount; i++ {
		tooManyPods = append(tooManyPods, claimPod("ml", fmt.Sprintf("pod-%d", i), "n1", directClaim("gpu", "c")))
	}
	for name, tc := range map[string]struct {
		slices []runtime.Object
		pods   []*corev1.Pod
	}{
		"too many drivers": {slices: tooManyDrivers},
		"too many devices": {slices: []runtime.Object{resourceSlice("n1-huge", "n1", "gpu.nvidia.com", topology.MaxDRACount+1)}},
		"devices across slices": {slices: []runtime.Object{
			resourceSlice("n1-a", "n1", "gpu.nvidia.com", topology.MaxDRACount),
			resourceSlice("n1-b", "n1", "gpu.nvidia.com", 1),
		}},
		"too many slices":     {slices: tooManySlices},
		"too many claim pods": {pods: tooManyPods},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := draTestPlatformWithPods(tc.pods, tc.slices...)
			got := p.ObserveDRA(context.Background(), "n1")
			want := topology.DRAObservation{State: topology.DRANotObserved, Reason: draReasonBoundsExceeded, APIVersion: "resource.k8s.io/v1"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("observation = %+v, want %+v", got, want)
			}
			if c := topology.Classify(topology.Snapshot{Node: "n1", EvaluatedAt: time.Now(), DRA: got}); c.Eligible() || c.DRA.Reason != draReasonBoundsExceeded {
				t.Fatalf("classified = %s dra=%+v, want a refusal carrying the reason", c.Summary, c.DRA)
			}
		})
	}
	// Exactly at the bounds is still an observation.
	atBound, _ := draTestPlatformWithPods(tooManyPods[:topology.MaxDRACount], tooManyDrivers[:topology.MaxDRADrivers]...)
	got := atBound.ObserveDRA(context.Background(), "n1")
	if got.State != topology.DRAObserved || len(got.Drivers) != topology.MaxDRADrivers || got.ClaimPods != topology.MaxDRACount || got.DirectClaims != topology.MaxDRACount {
		t.Fatalf("at the bounds = %+v, want observed", got)
	}
	if c := topology.Classify(topology.Snapshot{Node: "n1", EvaluatedAt: time.Now(), DRA: got}); c.Has(topology.ReasonDraObservationInvalid) || !c.Has(topology.ReasonDraManaged) {
		t.Fatalf("classified = %s, want a valid DRA-managed observation", c.Summary)
	}
}

func TestNodeResourcesReportsTheNodeAdvertisement(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	node.Status.Capacity = corev1.ResourceList{
		"nvidia.com/gpu":        resource.MustParse("8"),
		"nvidia.com/mig-1g.5gb": resource.MustParse("7"),
		"cpu":                   resource.MustParse("96"),
		"memory":                resource.MustParse("1Ti"),
		"hugepages-2Mi":         resource.MustParse("0"),
	}
	node.Status.Allocatable = corev1.ResourceList{
		"nvidia.com/gpu": resource.MustParse("8"),
		"cpu":            resource.MustParse("94"),
	}
	p := &Platform{client: fake.NewSimpleClientset(node)}
	got, found, err := p.NodeResources(context.Background(), "n1")
	if err != nil || !found {
		t.Fatalf("NodeResources = %+v %v %v", got, found, err)
	}
	want := topology.Resources{
		Source:      topology.ResourceSourceNode,
		Capacity:    map[string]int64{"nvidia.com/gpu": 8, "nvidia.com/mig-1g.5gb": 7},
		Allocatable: map[string]int64{"nvidia.com/gpu": 8},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resources = %+v, want %+v", got, want)
	}
	if _, found, err := p.NodeResources(context.Background(), "ghost"); err != nil || found {
		t.Fatalf("missing node = found=%v err=%v, want (false, nil)", found, err)
	}
	p.client.(*fake.Clientset).PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	if _, found, err := p.NodeResources(context.Background(), "n1"); err == nil || found {
		t.Fatalf("read failure = found=%v err=%v, want an error", found, err)
	}
}

// TestObserveDRACountsClaimReferencesFromNodePods: a Pod scheduled on the
// node that references a ResourceClaim directly or through a template is DRA
// evidence even when no ResourceSlice is published for the node. Only
// bounded counts leave the observation, and Pods on other nodes (which a
// server or fake ignoring the field selector would return) are never
// attributed to this one.
func TestObserveDRACountsClaimReferencesFromNodePods(t *testing.T) {
	pods := []*corev1.Pod{
		claimPod("ml", "train-0", "n1", directClaim("gpu", "train-0-gpu")),
		claimPod("ml", "train-1", "n1", templateClaim("gpu", "gpu-template")),
		claimPod("ml", "both", "n1", directClaim("a", "claim-a"), templateClaim("b", "tmpl-b")),
		claimPod("infra", "plain", "n1"),
		claimPod("ml", "elsewhere", "n2", directClaim("gpu", "other-node-claim")),
	}
	p, dyn := draTestPlatformWithPods(pods)
	got := p.ObserveDRA(context.Background(), "n1")
	want := topology.DRAObservation{State: topology.DRAObserved, APIVersion: "resource.k8s.io/v1", PodsInspected: true, ClaimPods: 3, DirectClaims: 2, TemplateClaims: 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observation = %+v, want %+v", got, want)
	}
	// Nothing in the observation names a namespace, pod, claim or template.
	rendered := fmt.Sprintf("%+v", got)
	for _, name := range []string{"ml", "infra", "train", "gpu-template", "claim-a", "tmpl-b", "other-node"} {
		if strings.Contains(rendered, name) {
			t.Fatalf("observation carries the name %q: %s", name, rendered)
		}
	}
	// Zero slices plus a claim refuses as DRA-managed / mixed.
	now := time.Now().UTC()
	snapshot := topology.Snapshot{
		Node: "n1", EvaluatedAt: now,
		Resources: topology.Resources{Source: topology.ResourceSourceNode, Capacity: map[string]int64{"nvidia.com/gpu": 1}},
		Report:    &topology.Report{ObservedAt: now.Add(-time.Second), TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned, Readiness: types.AcceleratorReadinessReady, PhysicalDevices: 1},
		DRA:       got,
	}
	if c := topology.Classify(snapshot); c.Eligible() || c.Topology != topology.TopologyMixed || !c.Has(topology.ReasonDraManaged) {
		t.Fatalf("classified = %s, want mixed/DraManaged from claims alone", c.Summary)
	}
	snapshot.Resources.Capacity = map[string]int64{}
	if c := topology.Classify(snapshot); c.Eligible() || c.Topology != topology.TopologyDRAManaged || !c.Has(topology.ReasonDraManaged) {
		t.Fatalf("classified = %s, want dra-managed from claims alone", c.Summary)
	}
	// Only the other node's Pod is ignored; a node with Pods but no claims is
	// an empty observation.
	if got := p.ObserveDRA(context.Background(), "n2"); got.ClaimPods != 1 || got.DirectClaims != 1 || got.TemplateClaims != 0 {
		t.Fatalf("n2 = %+v, want one direct claim", got)
	}
	plain, _ := draTestPlatformWithPods([]*corev1.Pod{claimPod("infra", "plain", "n1")})
	if got := plain.ObserveDRA(context.Background(), "n1"); !reflect.DeepEqual(got, topology.DRAObservation{State: topology.DRAObserved, APIVersion: "resource.k8s.io/v1", PodsInspected: true}) {
		t.Fatalf("pods without claims = %+v, want an empty inspected observation", got)
	}
	// A completed Pod that still references a claim is still evidence.
	done := claimPod("ml", "done", "n1", directClaim("gpu", "done-gpu"))
	done.Status.Phase = corev1.PodSucceeded
	finished, _ := draTestPlatformWithPods([]*corev1.Pod{done})
	if got := finished.ObserveDRA(context.Background(), "n1"); got.ClaimPods != 1 {
		t.Fatalf("succeeded pod with a claim = %+v, want it counted", got)
	}
	_ = dyn
}

// TestObserveDRAUsesOnlyReadsAndNeverTouchesClaimObjects is the audit of
// every API action one observation takes: exactly one list of resourceslices
// at v1 on the dynamic client and exactly one list of core Pods on the core
// client. No discovery call, no write verb, no resourceclaims,
// resourceclaimtemplates or deviceclasses resource, nothing else on
// resource.k8s.io.
func TestObserveDRAUsesOnlyReadsAndNeverTouchesClaimObjects(t *testing.T) {
	pods := []*corev1.Pod{claimPod("ml", "train-0", "n1", directClaim("gpu", "train-0-gpu"), templateClaim("t", "tmpl"))}
	p, dyn := draTestPlatformWithPods(pods, resourceSlice("n1-nvidia", "n1", "gpu.nvidia.com", 8))
	if got := p.ObserveDRA(context.Background(), "n1"); got.State != topology.DRAObserved || got.ResourceSlices != 1 || got.ClaimPods != 1 {
		t.Fatalf("observation = %+v", got)
	}
	readVerbs := map[string]bool{"get": true, "list": true}
	podsGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	var coreLists, sliceLists int
	for _, action := range p.client.(*fake.Clientset).Actions() {
		if !readVerbs[action.GetVerb()] {
			t.Fatalf("core client took a non-read action %s %v", action.GetVerb(), action.GetResource())
		}
		if action.GetResource().Group == "resource.k8s.io" {
			t.Fatalf("core client touched resource.k8s.io: %s %v", action.GetVerb(), action.GetResource())
		}
		// The only core action is the Pod list. A discovery get would
		// surface here as a non-pods resource and fail the audit.
		if action.GetVerb() != "list" || action.GetResource() != podsGVR {
			t.Fatalf("unexpected core action %s %v, want only a list of %v", action.GetVerb(), action.GetResource(), podsGVR)
		}
		coreLists++
	}
	if coreLists != 1 {
		t.Fatalf("core pod lists = %d, want exactly one", coreLists)
	}
	for _, action := range dyn.Actions() {
		if action.GetVerb() != "list" || action.GetResource() != draResourceSlices {
			t.Fatalf("dynamic client took %s %v, want only a list of %v", action.GetVerb(), action.GetResource(), draResourceSlices)
		}
		// No spec.nodeName field selector: a server-side selector would hide
		// slices scoped by nodeSelector/allNodes/perDeviceNodeSelection and
		// let them read as "no DRA on this node".
		if restrictions := action.(k8stesting.ListAction).GetListRestrictions(); restrictions.Fields != nil && !restrictions.Fields.Empty() {
			t.Fatalf("slice list carries a field selector %q; locality must be decided client-side", restrictions.Fields)
		}
		sliceLists++
	}
	if sliceLists != 1 {
		t.Fatalf("dynamic slice lists = %d, want exactly one", sliceLists)
	}
	for _, forbidden := range []string{"resourceclaims", "resourceclaimtemplates", "deviceclasses"} {
		if draResourceSlices.Resource == forbidden {
			t.Fatalf("the slice resource is %q", forbidden)
		}
	}
}

// TestObserveDRAFailsClosedWhenPodsCannotBeListed: a slice read that
// succeeded is never reported as observed when the Pod list fails. Inability
// to inspect Pods is not "no claims".
func TestObserveDRAFailsClosedWhenPodsCannotBeListed(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"forbidden":   {err: apierrors.NewForbidden(gr, "", errors.New("denied")), reason: draReasonPodsForbidden},
		"timeout":     {err: apierrors.NewTimeoutError("too slow", 1), reason: draReasonTimeout},
		"deadline":    {err: context.DeadlineExceeded, reason: draReasonTimeout},
		"other error": {err: errors.New("connection reset"), reason: draReasonPodsListFailed},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := draTestPlatformWithPods(
				[]*corev1.Pod{claimPod("ml", "train-0", "n1", directClaim("gpu", "train-0-gpu"))},
				resourceSlice("n1-nvidia", "n1", "gpu.nvidia.com", 8))
			p.client.(*fake.Clientset).PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			got := p.ObserveDRA(context.Background(), "n1")
			if got.State != topology.DRANotObserved || got.Reason != tc.reason {
				t.Fatalf("observation = %+v, want not-observed/%s", got, tc.reason)
			}
			if got.PodsInspected || got.ResourceSlices != 0 || got.ClaimPods != 0 || got.DirectClaims != 0 || got.TemplateClaims != 0 {
				t.Fatalf("a failed pod read must carry no counts at all: %+v", got)
			}
			if c := topology.Classify(topology.Snapshot{Node: "n1", EvaluatedAt: time.Now(), DRA: got}); c.Eligible() || !c.Has(topology.ReasonTopologyUnknown) {
				t.Fatalf("classified = %s, want a refusal", c.Summary)
			}
		})
	}
}

// TestObserveDRARefusesMalformedClaimReferences: a spec.resourceClaims entry
// that is not the shape the apiserver admits poisons the observation rather
// than being skipped. A skipped entry could read as "no claim on this Pod".
func TestObserveDRARefusesMalformedClaimReferences(t *testing.T) {
	empty := ""
	for name, claims := range map[string][]corev1.PodResourceClaim{
		"neither name set":           {{Name: "gpu"}},
		"both names set":             {{Name: "gpu", ResourceClaimName: ptrTo("c"), ResourceClaimTemplateName: ptrTo("t")}},
		"empty claim name":           {{Name: "gpu", ResourceClaimName: &empty}},
		"empty template name":        {{Name: "gpu", ResourceClaimTemplateName: &empty}},
		"claim name not a subdomain": {{Name: "gpu", ResourceClaimName: ptrTo("Not Valid!")}},
		"entry name not a label":     {{Name: "", ResourceClaimName: ptrTo("c")}},
		"entry name with a slash":    {{Name: "a/b", ResourceClaimName: ptrTo("c")}},
		"duplicate entry names":      {directClaim("gpu", "a"), directClaim("gpu", "b")},
	} {
		t.Run(name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "bad"}, Spec: corev1.PodSpec{NodeName: "n1", ResourceClaims: claims}}
			p, _ := draTestPlatformWithPods([]*corev1.Pod{claimPod("ml", "good", "n1", directClaim("gpu", "good-gpu")), pod})
			got := p.ObserveDRA(context.Background(), "n1")
			if got.State != topology.DRANotObserved || got.Reason != draReasonMalformedClaim {
				t.Fatalf("observation = %+v, want not-observed/%s", got, draReasonMalformedClaim)
			}
			if got.PodsInspected || got.ClaimPods != 0 {
				t.Fatalf("a malformed reference must leave no partial count: %+v", got)
			}
			if c := topology.Classify(topology.Snapshot{Node: "n1", EvaluatedAt: time.Now(), DRA: got}); c.Eligible() {
				t.Fatalf("classified = %s, want a refusal", c.Summary)
			}
		})
	}
	// A container-level claim naming an entry the Pod does not declare is
	// unexpected data too, whatever container carries it.
	for name, mutate := range map[string]func(*corev1.Pod){
		"container": func(p *corev1.Pod) { p.Spec.Containers[0].Resources.Claims = []corev1.ResourceClaim{{Name: "ghost"}} },
		"init container": func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "init", Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "ghost"}}}}}
		},
		"ephemeral container": func(p *corev1.Pod) {
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "ghost"}}}}}}
		},
	} {
		t.Run("dangling "+name, func(t *testing.T) {
			pod := claimPod("ml", "dangling", "n1", directClaim("gpu", "c"))
			mutate(pod)
			p, _ := draTestPlatformWithPods([]*corev1.Pod{pod})
			if got := p.ObserveDRA(context.Background(), "n1"); got.State != topology.DRANotObserved || got.Reason != draReasonMalformedClaim {
				t.Fatalf("observation = %+v, want not-observed/%s", got, draReasonMalformedClaim)
			}
		})
	}
	// A malformed Pod on ANOTHER node does not poison this node's observation.
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "bad"}, Spec: corev1.PodSpec{NodeName: "n2", ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu"}}}}
	p, _ := draTestPlatformWithPods([]*corev1.Pod{other})
	if got := p.ObserveDRA(context.Background(), "n1"); got.State != topology.DRAObserved || !got.PodsInspected || got.ClaimPods != 0 {
		t.Fatalf("other node's malformed pod changed n1 = %+v", got)
	}
}

func ptrTo(s string) *string { return &s }
