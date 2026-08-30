package kubernetes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

func gpuNode(name string, gpus string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if gpus != "" {
		n.Status.Capacity = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse(gpus)}
	}
	return n
}

func pod(name string, owner *metav1.OwnerReference, phase corev1.PodPhase, mirror bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: "n1"},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if owner != nil {
		p.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	if mirror {
		p.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "true"}
	}
	return p
}

func controllerRef(kind string) *metav1.OwnerReference {
	yes := true
	return &metav1.OwnerReference{Kind: kind, Name: "owner", Controller: &yes}
}

func TestListNodesFiltersOnGPUCapacity(t *testing.T) {
	gpu := gpuNode("gpu-1", "8")
	gpu.UID = k8stypes.UID("gpu-node-uid")
	client := fake.NewSimpleClientset(gpu, gpuNode("cpu-1", ""))
	p := &Platform{client: client}

	nodes, err := p.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "gpu-1" {
		t.Fatalf("nodes = %+v, want only gpu-1", nodes)
	}
	if len(nodes[0].GPUs) != 8 {
		t.Fatalf("gpu count = %d, want 8", len(nodes[0].GPUs))
	}
	if nodes[0].Platform != "kubernetes" {
		t.Fatalf("platform = %q", nodes[0].Platform)
	}
	if nodes[0].UID != "gpu-node-uid" {
		t.Fatalf("node UID = %q, want immutable Kubernetes UID", nodes[0].UID)
	}
}

// Once StartNodeCache syncs, ListNodes serves from the informer cache — same
// filtering, same shape — and tracks watch updates without touching the
// apiserver List path again (admission checks must not pay a round trip per
// destructive step).
func TestListNodesServesFromSyncedNodeCache(t *testing.T) {
	gpu := gpuNode("gpu-1", "8")
	client := fake.NewSimpleClientset(gpu, gpuNode("cpu-1", ""))
	p := &Platform{client: client}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.StartNodeCache(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for p.nodeLister.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("node cache never synced")
		}
		time.Sleep(10 * time.Millisecond)
	}

	nodes, err := p.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "gpu-1" {
		t.Fatalf("cached nodes = %+v, want only gpu-1", nodes)
	}

	// A label change arriving through the watch is visible without a List.
	fresh := gpuNode("gpu-1", "8")
	fresh.Labels = map[string]string{"blast-radius": "yes"}
	if _, err := client.CoreV1().Nodes().Update(context.Background(), fresh, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		nodes, err = p.ListNodes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) == 1 && nodes[0].Labels["blast-radius"] == "yes" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache never observed the label update; nodes = %+v", nodes)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCordonSetsUnschedulableAndReason(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	p := &Platform{client: client}
	ctx := context.Background()

	if err := p.Cordon(ctx, "gpu-1", "xid-79"); err != nil {
		t.Fatal(err)
	}
	n, err := client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !n.Spec.Unschedulable {
		t.Fatal("node must be unschedulable after Cordon")
	}
	if got := n.Annotations[cordonReasonAnnotation]; got != "xid-79" {
		t.Fatalf("cordon reason = %q, want xid-79", got)
	}

	if err := p.Uncordon(ctx, "gpu-1"); err != nil {
		t.Fatal(err)
	}
	n, err = client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n.Spec.Unschedulable {
		t.Fatal("node must be schedulable after Uncordon")
	}
	if _, ok := n.Annotations[cordonReasonAnnotation]; ok {
		t.Fatal("cordon reason annotation must be cleared by Uncordon")
	}
}

func TestSkipDuringDrainMirrorsKubectl(t *testing.T) {
	for name, tc := range map[string]struct {
		pod   *corev1.Pod
		force bool
		skip  bool
	}{
		"replicaset pod is evicted":        {pod("p", controllerRef("ReplicaSet"), corev1.PodRunning, false), false, false},
		"daemonset pod is left alone":      {pod("p", controllerRef("DaemonSet"), corev1.PodRunning, false), false, true},
		"mirror pod is left alone":         {pod("p", nil, corev1.PodRunning, true), false, true},
		"succeeded pod is left alone":      {pod("p", nil, corev1.PodSucceeded, false), false, true},
		"unmanaged pod needs force":        {pod("p", nil, corev1.PodRunning, false), false, true},
		"unmanaged pod evicted with force": {pod("p", nil, corev1.PodRunning, false), true, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := skipDuringDrain(tc.pod, tc.force); got != tc.skip {
				t.Fatalf("skipDuringDrain = %v, want %v", got, tc.skip)
			}
		})
	}
}

func TestDrainEvictsOnlyEvictablePods(t *testing.T) {
	rsPod := pod("app", controllerRef("ReplicaSet"), corev1.PodRunning, false)
	dsPod := pod("ds", controllerRef("DaemonSet"), corev1.PodRunning, false)
	bare := pod("bare", nil, corev1.PodRunning, false)
	client := fake.NewSimpleClientset(rsPod, dsPod, bare)

	var evicted []string
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := action.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		evicted = append(evicted, ev.Name)
		if err := client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), ev.Namespace, ev.Name); err != nil {
			return true, nil, err
		}
		return true, nil, nil
	})

	p := &Platform{client: client}
	err := p.Drain(context.Background(), "n1", platform.DrainOptions{
		Timeout: 5 * time.Second, GracePeriod: platform.DrainUsePodGracePeriod,
	})
	// A pod with no controller is not evicted AND the drain must say so.
	//
	// This assertion used to be `err != nil -> fatal`, which encoded the
	// defect: the bare pod was skipped, Drain returned nil, and the ladder
	// rebooted a node with somebody's unmanaged job still on it. kubectl drain
	// aborts in exactly this case unless --force is given.
	if err == nil {
		t.Fatal("Drain reported success while a pod with no controller was left running; " +
			"nothing reschedules that pod, and the ladder's next rung reboots the node")
	}
	if !strings.Contains(err.Error(), "default/bare") {
		t.Fatalf("the refusal does not name the pod a human has to deal with: %v", err)
	}
	// And NOTHING was evicted. The refusal is a pre-flight, like kubectl's:
	// the first version collected the names inside the eviction loop, so it
	// destroyed the customer's managed pods, waited for them to terminate, and
	// only then failed — after which the ladder escalated and did it again at
	// every remaining rung, for a node it could never drain.
	if len(evicted) != 0 {
		t.Fatalf("evicted = %v; a drain that cannot succeed must not disrupt anything first", evicted)
	}
	// DaemonSet and unmanaged pods must survive a non-forced drain.
	for _, name := range []string{"ds", "bare"} {
		if _, err := client.CoreV1().Pods("default").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Fatalf("pod %s must survive drain: %v", name, err)
		}
	}
}

// TestDrainDoesNotTruncateThePodGracePeriod covers the workload with the most
// to lose on the node we are about to reset.
//
// DeleteOptions.GracePeriodSeconds overrides the pod spec in BOTH directions,
// and the drain passed a hardcoded 30s — so a job declaring
// terminationGracePeriodSeconds: 600 precisely so it could checkpoint on
// SIGTERM was SIGKILLed at 30 instead.
func TestDrainDoesNotTruncateThePodGracePeriod(t *testing.T) {
	long := int64(600)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "trainer", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs",
				Controller: func() *bool { b := true; return &b }(),
			}},
		},
		Spec:   corev1.PodSpec{NodeName: "n1", TerminationGracePeriodSeconds: &long},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewSimpleClientset(pod)

	var override *int64
	var sawEviction bool
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := action.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		sawEviction = true
		if ev.DeleteOptions != nil {
			override = ev.DeleteOptions.GracePeriodSeconds
		}
		return true, nil, client.Tracker().Delete(
			corev1.SchemeGroupVersion.WithResource("pods"), ev.Namespace, ev.Name)
	})

	p := &Platform{client: client}
	// A step budget that comfortably fits the pod's 600s: the pod keeps its own.
	if err := p.Drain(context.Background(), "n1", platform.DrainOptions{
		Timeout: 30 * time.Minute, GracePeriod: platform.DrainUsePodGracePeriod,
	}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !sawEviction {
		t.Fatal("no eviction was issued, so this test proves nothing")
	}
	if override != nil {
		t.Fatalf("the eviction overrode the pod's grace period with %ds; a job that asked for "+
			"%ds to checkpoint gets SIGKILL instead", *override, long)
	}
}

// TestDrainClampsAGracePeriodItCannotHonour covers the other side of the same
// decision.
//
// terminationGracePeriodSeconds is tenant-writable and unbounded. Honouring it
// unconditionally hands the drain's duration to whoever wrote the pod: a spec
// declaring far more than the step's budget cannot terminate inside it, so the
// drain times out, the step fails, and the incident climbs the whole ladder to
// NEEDS_HUMAN having repaired nothing. A tenant must not be able to deny
// remediation of the node they are running on.
func TestDrainClampsAGracePeriodItCannotHonour(t *testing.T) {
	day := int64(86400)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "squatter", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs",
				Controller: func() *bool { b := true; return &b }(),
			}},
		},
		Spec:   corev1.PodSpec{NodeName: "n1", TerminationGracePeriodSeconds: &day},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewSimpleClientset(pod)

	var override *int64
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := action.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		if ev.DeleteOptions != nil {
			override = ev.DeleteOptions.GracePeriodSeconds
		}
		return true, nil, client.Tracker().Delete(
			corev1.SchemeGroupVersion.WithResource("pods"), ev.Namespace, ev.Name)
	})

	p := &Platform{client: client}
	if err := p.Drain(context.Background(), "n1", platform.DrainOptions{
		Timeout: 10 * time.Minute, GracePeriod: platform.DrainUsePodGracePeriod,
	}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if override == nil {
		t.Fatal("a pod asking for 24 hours against a 10-minute step was left unclamped; the drain " +
			"cannot finish, and the ladder escalates to a human having repaired nothing")
	}
	if *override <= 0 || *override >= day {
		t.Fatalf("clamped to %ds, want something inside the step budget and above zero", *override)
	}
}

// A PodDisruptionBudget rejection (HTTP 429) must be retried until the
// budget frees up, not surfaced as a step failure that escalates the ladder.
func TestDrainRetriesPDBBlockedEvictions(t *testing.T) {
	old := drainPollInterval
	drainPollInterval = 10 * time.Millisecond
	defer func() { drainPollInterval = old }()

	rsPod := pod("app", controllerRef("ReplicaSet"), corev1.PodRunning, false)
	client := fake.NewSimpleClientset(rsPod)

	attempts := 0
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		attempts++
		if attempts <= 2 {
			return true, nil, apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 1)
		}
		ev := action.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		if err := client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), ev.Namespace, ev.Name); err != nil {
			return true, nil, err
		}
		return true, nil, nil
	})

	p := &Platform{client: client}
	if err := p.Drain(context.Background(), "n1", platform.DrainOptions{Timeout: 5 * time.Second, GracePeriod: 30 * time.Second}); err != nil {
		t.Fatalf("Drain must retry PDB rejections: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("eviction attempts = %d, want 3 (two 429s then success)", attempts)
	}
}

// A permanently blocking PDB fails the drain only at the timeout, with an
// error naming the budget as the cause.
func TestDrainPermanentPDBBlockTimesOut(t *testing.T) {
	old := drainPollInterval
	drainPollInterval = 10 * time.Millisecond
	defer func() { drainPollInterval = old }()

	rsPod := pod("app", controllerRef("ReplicaSet"), corev1.PodRunning, false)
	client := fake.NewSimpleClientset(rsPod)
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewTooManyRequests("blocked by PDB", 1)
	})

	p := &Platform{client: client}
	err := p.Drain(context.Background(), "n1", platform.DrainOptions{Timeout: 100 * time.Millisecond, GracePeriod: 30 * time.Second})
	if err == nil {
		t.Fatal("permanently blocked drain must fail at the timeout")
	}
	if !strings.Contains(err.Error(), "PodDisruptionBudget") {
		t.Fatalf("error must name the blocking budget: %v", err)
	}
}

func TestNodeWorkloadsReportsGPUUsage(t *testing.T) {
	gpuPod := pod("training", controllerRef("ReplicaSet"), corev1.PodRunning, false)
	gpuPod.Spec.Containers = []corev1.Container{{
		Name: "main",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
		},
	}}
	client := fake.NewSimpleClientset(gpuPod, pod("web", controllerRef("ReplicaSet"), corev1.PodRunning, false))
	p := &Platform{client: client}

	workloads, err := p.NodeWorkloads(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]bool{}
	for _, w := range workloads {
		byName[w.Name] = w.UsesGPU
	}
	if !byName["training"] || byName["web"] {
		t.Fatalf("GPU usage detection wrong: %v", byName)
	}
}

// A hardcoded nvidia.com/gpu made two failures silent: an AMD or Intel node
// inventoried as zero GPUs, and evict_gpu_workload reporting success while
// leaving live jobs on a device about to be reset. The matcher is the fix, so
// it carries the regression.
func TestAcceleratorResourceMatchingIsVendorNeutral(t *testing.T) {
	accelerators := []string{
		"nvidia.com/gpu",
		"amd.com/gpu",
		"gpu.intel.com/i915",
		"gpu.intel.com/xe",
		"nvidia.com/mig-1g.5gb",
	}
	for _, name := range accelerators {
		if !isAcceleratorResource(corev1.ResourceName(name)) {
			t.Errorf("%s must be recognised as an accelerator — an unrecognised one silently skips eviction", name)
		}
	}
	notAccelerators := []string{
		"cpu", "memory", "ephemeral-storage",
		"hugepages-2Mi",
		"example.com/fpga",
		"example.com/gpumemory",
	}
	for _, name := range notAccelerators {
		if isAcceleratorResource(corev1.ResourceName(name)) {
			t.Errorf("%s must not be treated as an accelerator", name)
		}
	}
}

// TestFleetMembershipRejectsLookalikeResources pins the blast-radius side of
// the accelerator matcher. A resource that merely has a GPU-ish shape must
// not admit a machine to the fleet: everything in the fleet is a machine a
// playbook may cordon, drain and reboot.
func TestFleetMembershipRejectsLookalikeResources(t *testing.T) {
	cases := []struct {
		name     string
		capacity corev1.ResourceList
		want     bool
		wantGPUs int
	}{
		{"nvidia whole device", corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}, true, 8},
		{"amd whole device", corev1.ResourceList{"amd.com/gpu": resource.MustParse("4")}, true, 4},
		{"intel family", corev1.ResourceList{"gpu.intel.com/i915": resource.MustParse("2")}, true, 2},
		// A partition count is not a device count: reporting it would put a
		// fabricated capacity number in front of whoever pays for the fleet.
		// Zero means unknown until the agent registers the real devices.
		{"mig partitions only", corev1.ResourceList{"nvidia.com/mig-1g.5gb": resource.MustParse("7")}, true, 0},
		{
			"mixed strategy counts silicon once",
			corev1.ResourceList{
				"nvidia.com/gpu":        resource.MustParse("2"),
				"nvidia.com/mig-1g.5gb": resource.MustParse("14"),
			},
			true, 2,
		},
		{
			"gpu memory is not a device count",
			corev1.ResourceList{"aliyun.com/gpu-mem": resource.MustParse("16384")},
			true, 0,
		},
		{
			"third-party licence counter is not a GPU",
			corev1.ResourceList{"example.com/gpu-licence": resource.MustParse("100")},
			false, 0,
		},
		{"cpu only", corev1.ResourceList{"cpu": resource.MustParse("64")}, false, 0},
		{"unqualified gpu name", corev1.ResourceList{"gpu": resource.MustParse("1")}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeAdvertisesAccelerator(tc.capacity); got != tc.want {
				t.Errorf("nodeAdvertisesAccelerator = %v, want %v", got, tc.want)
			}
			if got := acceleratorCount(tc.capacity, nil); got != tc.wantGPUs {
				t.Errorf("acceleratorCount = %d, want %d", got, tc.wantGPUs)
			}
		})
	}
}

// TestEvictionMatcherStaysGenerous is the other half of the asymmetry: on the
// eviction path an unrecognised vendor must still count as holding a device,
// because failing to evict leaves a live job on hardware about to be reset.
func TestEvictionMatcherStaysGenerous(t *testing.T) {
	unknownVendor := pod("training", controllerRef("ReplicaSet"), corev1.PodRunning, false)
	unknownVendor.Spec.Containers = []corev1.Container{{
		Name: "main",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{"newvendor.example/gpu": resource.MustParse("1")},
		},
	}}
	if !podUsesGPU(unknownVendor) {
		t.Fatal("pod holding an unrecognised vendor's GPU was not seen as a GPU workload; " +
			"it would be left running on a device under remediation")
	}
	if nodeAdvertisesAccelerator(corev1.ResourceList{"newvendor.example/gpu": resource.MustParse("1")}) {
		t.Fatal("the same unrecognised resource admitted a node to the fleet; " +
			"fleet membership must be the conservative side")
	}
}

// TestUncordonRestoresMarksItDidNotPlace covers the node somebody else was
// already holding.
//
// Both things Cordon writes may already belong to another party: a human who
// ran `kubectl cordon`, or an operator who pinned the node with
// karpenter.sh/do-not-disrupt because a long training job is on it — which is
// precisely what that annotation is for. Uncordon used to null both
// unconditionally, so a KubeNeuron cordon/uncordon cycle handed Karpenter a
// node its owner had deliberately pinned and returned to the scheduler a node
// a human had deliberately removed from it.
func TestUncordonRestoresMarksItDidNotPlace(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "n1",
			Annotations: map[string]string{doNotDisruptAnnotation: "true"},
		},
		Spec: corev1.NodeSpec{Unschedulable: true}, // a human cordoned it
	}
	client := fake.NewSimpleClientset(node)
	p := &Platform{client: client}
	ctx := context.Background()

	if err := p.Cordon(ctx, "n1", "incident inc-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.Uncordon(ctx, "n1"); err != nil {
		t.Fatal(err)
	}

	got, err := client.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Unschedulable {
		t.Fatal("a node a human had cordoned came back schedulable after our cordon/uncordon cycle")
	}
	if _, pinned := got.Annotations[doNotDisruptAnnotation]; !pinned {
		t.Fatal("karpenter.sh/do-not-disrupt was deleted; Karpenter is now free to consolidate a " +
			"node its owner pinned, and that annotation exists to stop exactly that")
	}
	if _, held := got.Annotations[cordonReasonAnnotation]; held {
		t.Fatal("our own cordon record survived the uncordon")
	}
}

// TestUncordonReleasesAMarkItDidPlace: the ordinary case must still work, or
// the fix above would strand every node this product cordons.
func TestUncordonReleasesAMarkItDidPlace(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}})
	p := &Platform{client: client}
	ctx := context.Background()

	if err := p.Cordon(ctx, "n2", "incident inc-2"); err != nil {
		t.Fatal(err)
	}
	if err := p.Uncordon(ctx, "n2"); err != nil {
		t.Fatal(err)
	}
	got, err := client.CoreV1().Nodes().Get(ctx, "n2", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Unschedulable {
		t.Fatal("a node we cordoned ourselves was not released")
	}
	if _, pinned := got.Annotations[doNotDisruptAnnotation]; pinned {
		t.Fatal("our own do-not-disrupt pin survived the uncordon")
	}
}

// TestUncordonWithNoRecordDoesNotClearSomebodysPin covers the node cordoned by
// a build that predates the cordon-restore record and still held across the
// upgrade — the annotations live on the Node and survive the controller.
//
// An empty record means "we looked, and the node was clean". An ABSENT one
// means "we do not know". Splitting a missing key yields the same thing for
// both, which resolves the unknown case in the fail-open direction and deletes
// a karpenter.sh/do-not-disrupt somebody else placed — the exact harm the
// record was added to prevent.
func TestUncordonWithNoRecordDoesNotClearSomebodysPin(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n1",
			Annotations: map[string]string{
				cordonReasonAnnotation: "kubeneuron: fell-off-bus (inc-1)",
				doNotDisruptAnnotation: "true", // a human's pin
				// deliberately NO cordonRestoreAnnotation
			},
		},
		Spec: corev1.NodeSpec{Unschedulable: true},
	}
	client := fake.NewSimpleClientset(node)
	p := &Platform{client: client}

	if err := p.Uncordon(context.Background(), "n1"); err != nil {
		t.Fatal(err)
	}
	got, err := client.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, pinned := got.Annotations[doNotDisruptAnnotation]; !pinned {
		t.Fatal("with no record of what was there before, the uncordon deleted a do-not-disrupt " +
			"annotation it cannot know it placed; Karpenter is now free to consolidate the node")
	}
	// And the cordon is kept, for the same reason as the pin.
	//
	// This assertion is the reverse of what it said a round ago, and the
	// reversal is the point. The first version released the cordon so that a
	// node cordoned by a pre-record build would not be stranded across the
	// upgrade — a real cost, and the reason it was written that way. But it
	// applied the two marks asymmetrically for no stated reason, and the
	// asymmetry ran toward the worse outcome: the single most common way a
	// node is already cordoned when a remediation opens on it is that an
	// engineer typed kubectl cordon, and releasing that puts tenant work back
	// on a machine somebody is physically working on, with nothing in the
	// audit trail naming who released it.
	//
	// Stranded capacity is recoverable by one kubectl command and is visible
	// in kubectl get nodes. The other direction is neither.
	if !got.Spec.Unschedulable {
		t.Fatal("with no record of what was there before, the uncordon released a cordon it " +
			"cannot know it placed; if a human cordoned this node, tenant work is now " +
			"scheduling onto a machine they deliberately took out of service")
	}
	// Our own annotations must go, so the node reads as a plain human cordon
	// rather than a KubeNeuron cordon nothing will ever clean up.
	if _, ours := got.Annotations[cordonReasonAnnotation]; ours {
		t.Fatal("the node kept a KubeNeuron cordon reason it will never be released by; it is " +
			"now stranded AND invisible, which is the worst of both")
	}
}

// --- shared cordons: one node, several remediations ---------------------------
//
// A node has many GPUs and an incident is per (target, class), so two of them
// can be remediating two GPUs of one machine at the same time. Everything below
// covers what that does to a cordon whose state used to be a single reason and a
// single restore snapshot.

// applyAPIServerPatchSemantics makes the fake client answer a failed JSON-Patch
// `test` operation the way a real apiserver does.
//
// The fake applies patches in-process and hands back the patch library's own
// error; an apiserver turns the same failure into HTTP 422, which client-go
// surfaces as apierrors.Invalid — and "the compare-and-swap lost" is the only
// thing the production code is allowed to read out of that error. Without this,
// every test below would exercise a failure mode that cannot happen against a
// cluster, and the retry paths would never be reached at all.
func applyAPIServerPatchSemantics(client *fake.Clientset) {
	react := k8stesting.ObjectReaction(client.Tracker())
	client.PrependReactor("patch", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		handled, obj, err := react(action)
		if err != nil && strings.Contains(err.Error(), "test failed") {
			return handled, nil, apierrors.NewInvalid(
				schema.GroupKind{Kind: "Node"}, action.(k8stesting.PatchAction).GetName(), nil)
		}
		return handled, obj, err
	})
}

func nodeState(t *testing.T, client *fake.Clientset, name string) *corev1.Node {
	t.Helper()
	got, err := client.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestOneOwnerLeavingDoesNotReleaseANodeAnotherIsStillRemediating is the defect
// this whole mechanism exists for.
//
// Two incidents on two GPUs of one node both cordon it. Under a single reason
// annotation the second cordon overwrote the first, and the first incident's
// uncordon then released the machine outright: the scheduler put tenant work
// onto a node whose other GPU was about to be reset.
func TestOneOwnerLeavingDoesNotReleaseANodeAnotherIsStillRemediating(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()

	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-2", "kubeneuron: xid-79 (inc-2)"); err != nil {
		t.Fatal(err)
	}

	released, remaining, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if released || remaining != 1 {
		t.Fatalf("released=%v remaining=%d; inc-2 is still remediating a GPU on this node", released, remaining)
	}
	got := nodeState(t, client, "gpu-1")
	if !got.Spec.Unschedulable {
		t.Fatal("the node came back schedulable when the FIRST of two remediations finished; the " +
			"scheduler puts tenant work onto a machine whose other GPU is about to be reset")
	}
	if _, pinned := got.Annotations[doNotDisruptAnnotation]; !pinned {
		t.Fatal("the autoscaler pin was lifted while a remediation is still running, so Karpenter " +
			"is free to consolidate the node out from under it")
	}
	if got.Annotations[cordonOwnersAnnotation] != `["inc-2"]` {
		t.Fatalf("owner set = %q, want only the remediation that has not finished",
			got.Annotations[cordonOwnersAnnotation])
	}

	// And the last one out does return the node to service, or this would strand
	// every node the product ever cordons.
	released, remaining, err = p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-2"})
	if err != nil {
		t.Fatal(err)
	}
	if !released || remaining != 0 {
		t.Fatalf("released=%v remaining=%d, want the last owner out to release the node", released, remaining)
	}
	got = nodeState(t, client, "gpu-1")
	if got.Spec.Unschedulable {
		t.Fatal("the last remediation finished and the node stayed cordoned: a healthy GPU node is " +
			"out of the fleet with nothing left running to put it back")
	}
	for _, ann := range []string{cordonOwnersAnnotation, cordonReasonAnnotation, cordonRestoreAnnotation, doNotDisruptAnnotation} {
		if _, present := got.Annotations[ann]; present {
			t.Fatalf("annotation %s survived the last release; the janitor reads it as a cordon this "+
				"product still owns", ann)
		}
	}
}

// TestTheRestoreSnapshotIsWrittenOnceByTheFirstOwner covers what a second
// cordon must NOT do to the record of what the node looked like before any of
// this started.
//
// A human cordoned this node and pinned it with karpenter.sh/do-not-disrupt
// because a long training job is on it. If the second incident to arrive
// overwrites the snapshot, it records OUR cordon and OUR pin as the node's
// original state — and then the last release either hands a human's cordon back
// to the scheduler or leaves the node pinned and cordoned with nobody's name on
// it.
// Each case starts in a state our own cordon does NOT produce, so a snapshot
// taken by the second owner — which would record our cordon and our pin as the
// node's original state — cannot pass by coincidence.
func TestTheRestoreSnapshotIsWrittenOnceByTheFirstOwner(t *testing.T) {
	for name, tc := range map[string]struct {
		cordoned, pinned bool
		want             string
	}{
		// A human ran kubectl cordon on a node nobody had pinned.
		"cordoned by a human": {cordoned: true, want: "unschedulable"},
		// An operator pinned a node around a long training job, but left it
		// schedulable.
		"pinned by an operator": {pinned: true, want: "do-not-disrupt"},
	} {
		t.Run(name, func(t *testing.T) {
			start := gpuNode("gpu-1", "8")
			start.Spec.Unschedulable = tc.cordoned
			start.Annotations = map[string]string{}
			if tc.pinned {
				start.Annotations[doNotDisruptAnnotation] = "true"
			}
			client := fake.NewSimpleClientset(start)
			applyAPIServerPatchSemantics(client)
			p := &Platform{client: client}
			ctx := context.Background()

			if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
				t.Fatal(err)
			}
			if err := p.CordonForOwner(ctx, "gpu-1", "inc-2", "kubeneuron: xid-79 (inc-2)"); err != nil {
				t.Fatal(err)
			}
			if got := nodeState(t, client, "gpu-1").Annotations[cordonRestoreAnnotation]; got != tc.want {
				t.Fatalf("restore record = %q after a second owner joined, want %q — the state the "+
					"FIRST owner found. Anything else records our own cordon and our own pin as the "+
					"node's original state, and the last release then either leaves the machine "+
					"cordoned with nobody's name on it or hands back one a human took out of service",
					got, tc.want)
			}

			if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-2"}); err != nil {
				t.Fatal(err)
			}
			got := nodeState(t, client, "gpu-1")
			if got.Spec.Unschedulable != tc.cordoned {
				t.Fatalf("unschedulable = %v after two remediations came and went, want %v: a cordon "+
					"a human placed must survive, and one we placed must not",
					got.Spec.Unschedulable, tc.cordoned)
			}
			if _, pinned := got.Annotations[doNotDisruptAnnotation]; pinned != tc.pinned {
				t.Fatalf("do-not-disrupt present = %v, want %v: a pin its owner placed around a long "+
					"training job must survive, and ours must be lifted or the autoscaler is blocked "+
					"on this node forever", pinned, tc.pinned)
			}
		})
	}
}

// TestReleasingAnOwnerThatIsNotThereIsANoOp covers the replay.
//
// Steps are retried and re-driven after a controller restart, and the janitor
// asks about the same node on every reconcile tick. A release that already
// happened must be silent: an error here fails a playbook that has nothing left
// to do and escalates an incident that was finished.
func TestReleasingAnOwnerThatIsNotThereIsANoOp(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-2", "kubeneuron: xid-79 (inc-2)"); err != nil {
		t.Fatal(err)
	}

	// An incident that never held this node at all.
	released, remaining, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-elsewhere"})
	if err != nil {
		t.Fatalf("releasing an owner that is not on the node returned %v; a replayed or retried step "+
			"must not fail the playbook that owns it", err)
	}
	if released || remaining != 2 {
		t.Fatalf("released=%v remaining=%d; a stranger's release must change nothing", released, remaining)
	}

	// The same owner leaving twice.
	if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"}); err != nil {
		t.Fatal(err)
	}
	released, remaining, err = p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"})
	if err != nil {
		t.Fatalf("a repeated release returned %v, want silence", err)
	}
	if released || remaining != 1 {
		t.Fatalf("released=%v remaining=%d on a repeated release", released, remaining)
	}
	if !nodeState(t, client, "gpu-1").Spec.Unschedulable {
		t.Fatal("a repeated release of an owner that had already left uncordoned a node inc-2 is " +
			"still remediating")
	}
}

// TestALostRaceOnTheOwnerSetDoesNotReleaseTheNode covers two controllers, or a
// step and a janitor pass, writing the set at the same moment.
//
// The owner set is one annotation, so a read-modify-write drops whatever landed
// between the read and the write. Here inc-1 reads the node a moment before
// inc-2 joins: without the compare-and-swap it computes "I was the only owner",
// restores the node, and hands the scheduler a machine inc-2 is about to reset a
// GPU on.
func TestALostRaceOnTheOwnerSetDoesNotReleaseTheNode(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-2", "kubeneuron: xid-79 (inc-2)"); err != nil {
		t.Fatal(err)
	}

	// The first read of the release below sees the node as it was before inc-2
	// joined; the live object already has both owners.
	react := k8stesting.ObjectReaction(client.Tracker())
	stale := true
	client.PrependReactor("get", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		handled, obj, err := react(action)
		if err != nil || !stale {
			return handled, obj, err
		}
		stale = false
		node := obj.(*corev1.Node).DeepCopy()
		node.Annotations[cordonOwnersAnnotation] = `["inc-1"]`
		return handled, node, nil
	})

	released, remaining, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if released || remaining != 1 {
		t.Fatalf("released=%v remaining=%d; the release was computed from a set that inc-2 had "+
			"already joined", released, remaining)
	}
	got := nodeState(t, client, "gpu-1")
	if !got.Spec.Unschedulable {
		t.Fatal("a release that lost the race on the owner set uncordoned the node anyway; the " +
			"scheduler puts tenant work onto a machine whose GPU is about to be reset")
	}
	if got.Annotations[cordonOwnersAnnotation] != `["inc-2"]` {
		t.Fatalf("owner set = %q; the retry must recompute against the set that is actually there, "+
			"not force the stale one over it", got.Annotations[cordonOwnersAnnotation])
	}
}

// TestTwoOwnersJoiningAtOnceKeepBothHolds is the same race on the way in. A
// cordon that silently drops the other incident's entry is a node that will be
// released while that incident is still working on it — the defect, just one
// step earlier.
func TestTwoOwnersJoiningAtOnceKeepBothHolds(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}

	// inc-2 reads the node before inc-1 cordoned it.
	react := k8stesting.ObjectReaction(client.Tracker())
	stale := true
	client.PrependReactor("get", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		handled, obj, err := react(action)
		if err != nil || !stale {
			return handled, obj, err
		}
		stale = false
		node := obj.(*corev1.Node).DeepCopy()
		delete(node.Annotations, cordonOwnersAnnotation)
		return handled, node, nil
	})
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-2", "kubeneuron: xid-79 (inc-2)"); err != nil {
		t.Fatal(err)
	}
	if got := nodeState(t, client, "gpu-1").Annotations[cordonOwnersAnnotation]; got != `["inc-1","inc-2"]` {
		t.Fatalf("owner set = %q, want both holds; an entry dropped here is a node released while "+
			"the remediation that lost it is still resetting a GPU", got)
	}
}

// TestACordonFromABuildWithoutOwnerSetsIsNotForgotten covers the upgrade.
//
// The annotations live on the Node and survive the controller, so an upgrade
// finds nodes cordoned by the previous build: a reason, a restore record, and no
// owner set at all. The remediation that placed one may still be running, and if
// the next incident to cordon that node simply starts a fresh owner set, the
// older incident becomes invisible — and the newcomer's release hands back a
// machine that is still being worked on.
func TestACordonFromABuildWithoutOwnerSetsIsNotForgotten(t *testing.T) {
	const oldReason = "kubeneuron: ecc-dbe (inc-old)"
	previousBuild := gpuNode("gpu-1", "8")
	previousBuild.Spec.Unschedulable = true
	previousBuild.Annotations = map[string]string{
		cordonReasonAnnotation:  oldReason,
		cordonRestoreAnnotation: "", // we looked, the node was clean
		doNotDisruptAnnotation:  "true",
	}
	client := fake.NewSimpleClientset(previousBuild)
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()

	if err := p.CordonForOwner(ctx, "gpu-1", "inc-new", "kubeneuron: xid-79 (inc-new)"); err != nil {
		t.Fatal(err)
	}
	owners := nodeState(t, client, "gpu-1").Annotations[cordonOwnersAnnotation]
	if !strings.Contains(owners, platform.LegacyCordonOwner(oldReason)) {
		t.Fatalf("owner set = %q; the remediation that cordoned this node before the upgrade is not "+
			"in it, so the next release will hand back a machine it is still working on", owners)
	}

	released, remaining, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-new", platform.LegacyCordonOwner("kubeneuron: xid-79 (inc-new)")})
	if err != nil {
		t.Fatal(err)
	}
	if released || remaining != 1 {
		t.Fatalf("released=%v remaining=%d, want the pre-upgrade remediation still holding the node",
			released, remaining)
	}
	if !nodeState(t, client, "gpu-1").Spec.Unschedulable {
		t.Fatal("the incident that cordoned this node before the upgrade lost its hold, and the node " +
			"went back into service in the middle of its remediation")
	}

	// The pre-upgrade incident reaches its own uncordon step on the new build.
	// It knows itself by its incident ID and by the reason its cordon carried.
	released, _, err = p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-old", platform.LegacyCordonOwner(oldReason)})
	if err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("a node cordoned before the upgrade could not be released by the very incident that " +
			"cordoned it; it is stranded out of the fleet until a human notices")
	}
	if nodeState(t, client, "gpu-1").Spec.Unschedulable {
		t.Fatal("the last hold was dropped and the node stayed cordoned")
	}
}

// TestAnUntrackedCordonIsReleasedOnlyByItsOwnReason: the same upgrade case with
// nothing else on the node. The old build's rule was one reason, one owner — so
// a matching reason means the caller IS the sole owner, and a reason that does
// not match belongs to somebody else.
func TestAnUntrackedCordonIsReleasedOnlyByItsOwnReason(t *testing.T) {
	const reason = "kubeneuron: ecc-dbe (inc-old)"
	for name, tc := range map[string]struct {
		release []string
		want    bool
	}{
		"its own incident releases it": {[]string{"inc-old", platform.LegacyCordonOwner(reason)}, true},
		"a stranger does not":          {[]string{"inc-other", platform.LegacyCordonOwner("kubeneuron: xid-79 (inc-other)")}, false},
	} {
		t.Run(name, func(t *testing.T) {
			node := gpuNode("gpu-1", "8")
			node.Spec.Unschedulable = true
			node.Annotations = map[string]string{
				cordonReasonAnnotation:  reason,
				cordonRestoreAnnotation: "",
				doNotDisruptAnnotation:  "true",
			}
			client := fake.NewSimpleClientset(node)
			applyAPIServerPatchSemantics(client)
			p := &Platform{client: client}

			released, _, err := p.ReleaseCordonOwners(context.Background(), "gpu-1", tc.release)
			if err != nil {
				t.Fatal(err)
			}
			if released != tc.want {
				t.Fatalf("released = %v, want %v: a pre-upgrade cordon is released by the incident "+
					"whose reason is on the node and by nobody else", released, tc.want)
			}
			if nodeState(t, client, "gpu-1").Spec.Unschedulable == tc.want {
				t.Fatalf("node unschedulable = %v after release=%v", !tc.want, tc.want)
			}
		})
	}
}

// TestASharedCordonIsNotReleasedByReasonAlone guards the janitor's older route
// into the same node. The reason annotation belongs to whichever incident
// cordoned LAST, so releasing on it alone throws away every other hold on the
// machine at once.
func TestASharedCordonIsNotReleasedByReasonAlone(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-2", "kubeneuron: xid-79 (inc-2)"); err != nil {
		t.Fatal(err)
	}

	released, err := p.UncordonIfReason(ctx, "gpu-1", "kubeneuron: xid-79 (inc-2)")
	if err != nil {
		t.Fatal(err)
	}
	if released || !nodeState(t, client, "gpu-1").Spec.Unschedulable {
		t.Fatal("a reason-scoped release emptied a cordon two remediations were holding; inc-1 is " +
			"still working on a GPU on this node and the scheduler has just been handed it")
	}
}

// TestMarkCordonHeldIsScopedToOneHold: the held mark deliberately outlives the
// incident row, so stamping it from a verdict about a hold that has since gone
// keeps a GPU node out of the fleet permanently.
func TestMarkCordonHeldIsScopedToOneHold(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-2", "kubeneuron: xid-79 (inc-2)"); err != nil {
		t.Fatal(err)
	}

	// A verdict about a hold that is still there lands, even though the REASON on
	// the node belongs to the other incident — which is the whole reason this
	// exists beside MarkCordonHeldIfReason.
	marked, err := p.MarkCordonHeldIfOwner(ctx, "gpu-1", "inc-1")
	if err != nil {
		t.Fatal(err)
	}
	if !marked || nodeState(t, client, "gpu-1").Annotations[cordonHeldAnnotation] != "true" {
		t.Fatal("a human's verdict on inc-1 was not recorded because the node's reason belongs to " +
			"inc-2; when retention prunes inc-1's row the janitor will put a node a human took " +
			"charge of straight back into service")
	}

	// A verdict about a hold that has gone does not.
	if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"}); err != nil {
		t.Fatal(err)
	}
	marked, err = p.MarkCordonHeldIfOwner(ctx, "gpu-1", "inc-1")
	if err != nil {
		t.Fatal(err)
	}
	if marked {
		t.Fatal("a held mark decided about a hold that has since been released was stamped onto the " +
			"node anyway; the mark outlives the incident row, so this node never returns to the fleet")
	}
}

// TestTheRestoreSnapshotIsGuardedAgainstAMarkPlacedWhileWeRead.
//
// The snapshot is decided from the object this call READ, but the compare-and-
// swap on the way out pins only the owner-set annotation. Neither of the two
// things the snapshot is about is that annotation: `kubectl cordon` writes
// spec.unschedulable and touches nothing else, and pinning a node writes
// karpenter.sh/do-not-disrupt. Either can land between our read and our write
// while the owner set sits perfectly still, and the patch then records "the node
// was clean" over the top of a mark an engineer had just placed deliberately.
//
// It surfaces at the LAST release, long after the window closed: the node is
// handed back to the scheduler although a human took it out of service, or its
// pin is lifted although somebody put it there to keep a week-long training run
// off the autoscaler's list.
func TestTheRestoreSnapshotIsGuardedAgainstAMarkPlacedWhileWeRead(t *testing.T) {
	for name, tc := range map[string]struct {
		cordoned, pinned bool
		want             string
	}{
		"an engineer cordons the node while we read it": {cordoned: true, want: "unschedulable"},
		"an operator pins the node while we read it":    {pinned: true, want: "do-not-disrupt"},
	} {
		t.Run(name, func(t *testing.T) {
			// The live node already carries the human's mark. A real Node always
			// has annotations, so this does not take the create-the-map path.
			live := gpuNode("gpu-1", "8")
			live.Annotations = map[string]string{"node.alpha.kubernetes.io/ttl": "0"}
			live.Spec.Unschedulable = tc.cordoned
			if tc.pinned {
				live.Annotations[doNotDisruptAnnotation] = "true"
			}
			client := fake.NewSimpleClientset(live)
			applyAPIServerPatchSemantics(client)

			// ...but our read happened a moment before they made it.
			react := k8stesting.ObjectReaction(client.Tracker())
			stale := true
			client.PrependReactor("get", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
				handled, obj, err := react(action)
				if err != nil || !stale {
					return handled, obj, err
				}
				stale = false
				node := obj.(*corev1.Node).DeepCopy()
				node.Spec.Unschedulable = false
				delete(node.Annotations, doNotDisruptAnnotation)
				return handled, node, nil
			})

			p := &Platform{client: client}
			ctx := context.Background()
			if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
				t.Fatal(err)
			}
			if got := nodeState(t, client, "gpu-1").Annotations[cordonRestoreAnnotation]; got != tc.want {
				t.Fatalf("restore record = %q, want %q: the snapshot was taken from a read that "+
					"predates the mark a human placed, and nothing in the patch noticed", got, tc.want)
			}

			if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"}); err != nil {
				t.Fatal(err)
			}
			got := nodeState(t, client, "gpu-1")
			if got.Spec.Unschedulable != tc.cordoned {
				t.Fatalf("unschedulable = %v, want %v: a cordon an engineer placed by hand was "+
					"handed back to the scheduler by our release", got.Spec.Unschedulable, tc.cordoned)
			}
			if _, pinned := got.Annotations[doNotDisruptAnnotation]; pinned != tc.pinned {
				t.Fatalf("do-not-disrupt present = %v, want %v: a pin somebody placed around a long "+
					"training job was lifted by our release, and the autoscaler is now free to "+
					"consolidate the node out from under it", pinned, tc.pinned)
			}
		})
	}
}

// TestTheRestoreSnapshotIsGuardedOnAnnotationlessNodes covers the OTHER half of
// the guard, which the test above deliberately excludes.
//
// A node with no annotations at all takes a different path: the patch has to
// CREATE the annotations map before it can test a member of it, because a JSON
// Patch `test` against a member of a missing map fails as missing rather than as
// a failed test — an error the retry loop cannot clear. So this branch emits a
// mutation before the snapshot's test ops, and it is worth pinning that the
// guard still bites through it.
//
// It also pins what `test` means for an ABSENT value, which the whole guard
// rests on: spec.unschedulable is omitempty, so it vanishes from the object when
// false, and testing it for null is the only way to assert "still not cordoned".
// If that ever stopped meaning what it means here, the snapshot would silently
// record a human's cordon as our own and the last release would hand their node
// back to the scheduler.
func TestTheRestoreSnapshotIsGuardedOnAnnotationlessNodes(t *testing.T) {
	for name, tc := range map[string]struct {
		cordoned, pinned bool
		want             string
	}{
		"an engineer cordons an annotationless node while we read it": {cordoned: true, want: "unschedulable"},
		"an operator pins an annotationless node while we read it":    {pinned: true, want: "do-not-disrupt"},
	} {
		t.Run(name, func(t *testing.T) {
			live := gpuNode("gpu-1", "8")
			live.Spec.Unschedulable = tc.cordoned
			if tc.pinned {
				live.Annotations = map[string]string{doNotDisruptAnnotation: "true"}
			}
			client := fake.NewSimpleClientset(live)
			applyAPIServerPatchSemantics(client)

			// ...but our read happened a moment before they made that mark, and
			// saw a node with nothing on it at all.
			react := k8stesting.ObjectReaction(client.Tracker())
			stale := true
			client.PrependReactor("get", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
				handled, obj, err := react(action)
				if err != nil || !stale {
					return handled, obj, err
				}
				stale = false
				node := obj.(*corev1.Node).DeepCopy()
				node.Spec.Unschedulable = false
				node.Annotations = nil
				return handled, node, nil
			})

			p := &Platform{client: client}
			ctx := context.Background()
			if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
				t.Fatal(err)
			}
			if got := nodeState(t, client, "gpu-1").Annotations[cordonRestoreAnnotation]; got != tc.want {
				t.Fatalf("restore record = %q, want %q: the snapshot was taken from a read that "+
					"predates the mark a human placed, and the map-creating branch of the guard "+
					"did not notice", got, tc.want)
			}
			if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"}); err != nil {
				t.Fatal(err)
			}
			got := nodeState(t, client, "gpu-1")
			if got.Spec.Unschedulable != tc.cordoned {
				t.Fatalf("unschedulable = %v, want %v: a cordon an engineer placed by hand was "+
					"handed back to the scheduler by our release", got.Spec.Unschedulable, tc.cordoned)
			}
			if _, pinned := got.Annotations[doNotDisruptAnnotation]; pinned != tc.pinned {
				t.Fatalf("do-not-disrupt present = %v, want %v: a pin somebody placed around a long "+
					"training job was lifted by our release", pinned, tc.pinned)
			}
		})
	}
}

// TestAHeldVerdictNamesOneHoldAndLeavesWithIt.
//
// The held mark is the one thing that stops the janitor releasing a hold whose
// incident row retention has swept, and it used to be recorded against the NODE.
// On a machine with two remediations that answers "a human owns this" for the
// other one too: its abandoned hold can never be dropped, the owner set can
// never empty, and the mark blocking it is only cleared when the set DOES empty.
// Eight GPUs deadlock out of the fleet with no incident row left to explain it.
func TestAHeldVerdictNamesOneHoldAndLeavesWithIt(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-2", "kubeneuron: xid-79 (inc-2)"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.MarkCordonHeldIfOwner(ctx, "gpu-1", "inc-1"); err != nil {
		t.Fatal(err)
	}

	listed, err := p.CordonedNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("cordoned nodes = %+v, want the one node we cordoned", listed)
	}
	if !listed[0].HeldBy("inc-1") {
		t.Fatalf("held owners = %v; the human's verdict about inc-1 was not reported against inc-1, "+
			"so once its row is pruned the janitor releases a node a human took charge of",
			listed[0].HeldOwners)
	}
	if listed[0].HeldBy("inc-2") {
		t.Fatalf("held owners = %v; a verdict about inc-1 also answers for inc-2, so inc-2's hold "+
			"can never be released, the owner set can never empty, and the machine is out of the "+
			"fleet for good", listed[0].HeldOwners)
	}

	// The verdict goes when its hold goes. One that outlives its hold is exactly
	// as permanent as one written about the wrong hold.
	if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"}); err != nil {
		t.Fatal(err)
	}
	listed, err = p.CordonedNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].HeldBy("inc-1") || listed[0].HeldBy("inc-2") {
		t.Fatalf("cordoned nodes = %+v; the verdict about inc-1 survived inc-1's departure", listed)
	}

	// And the last hold out still takes every annotation with it.
	if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-2"}); err != nil {
		t.Fatal(err)
	}
	for _, ann := range cordonAnnotations {
		if _, present := nodeState(t, client, "gpu-1").Annotations[ann]; present {
			t.Fatalf("annotation %s survived the last release; the janitor reads it as a cordon this "+
				"product still owns", ann)
		}
	}
}

// TestAPreUpgradeHeldVerdictIsNotLostWhenANewOwnerJoins.
//
// A node cordoned by the previous build carries a node-scoped held mark, which
// is correct there because it has exactly one holder. The moment a second
// remediation joins, that mark stops answering for anybody in particular — so
// the holder it WAS about has to be named, or the very next janitor pass reads
// its hold as unexplained and hands back a machine a human took charge of before
// the upgrade.
func TestAPreUpgradeHeldVerdictIsNotLostWhenANewOwnerJoins(t *testing.T) {
	const oldReason = "kubeneuron: ecc-dbe (inc-old)"
	previousBuild := gpuNode("gpu-1", "8")
	previousBuild.Spec.Unschedulable = true
	previousBuild.Annotations = map[string]string{
		cordonReasonAnnotation:  oldReason,
		cordonRestoreAnnotation: "",
		cordonHeldAnnotation:    "true", // a human owns this cordon
		doNotDisruptAnnotation:  "true",
	}
	client := fake.NewSimpleClientset(previousBuild)
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()

	if err := p.CordonForOwner(ctx, "gpu-1", "inc-new", "kubeneuron: xid-79 (inc-new)"); err != nil {
		t.Fatal(err)
	}
	listed, err := p.CordonedNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("cordoned nodes = %+v, want the one node", listed)
	}
	if !listed[0].HeldBy(platform.LegacyCordonOwner(oldReason)) {
		t.Fatalf("held owners = %v; the human's verdict on the pre-upgrade cordon was dropped when "+
			"the owner set was created, so the janitor will return this machine to service",
			listed[0].HeldOwners)
	}
	if listed[0].HeldBy("inc-new") {
		t.Fatalf("held owners = %v; the newcomer inherited a verdict that was never about it, and "+
			"its abandoned hold can now never be cleaned up", listed[0].HeldOwners)
	}
}

// TestAnUnreadableOwnerSetLeavesTheNodeAlone: a hand-edited annotation must fail
// closed. Reporting the node with no owners sends the janitor down the
// single-owner path and releases a machine several remediations may be holding.
func TestAnUnreadableOwnerSetLeavesTheNodeAlone(t *testing.T) {
	node := gpuNode("gpu-1", "8")
	node.Spec.Unschedulable = true
	node.Annotations = map[string]string{
		cordonReasonAnnotation: "kubeneuron: ecc-dbe (inc-1)",
		cordonOwnersAnnotation: "inc-1, inc-2", // somebody edited it by hand
	}
	client := fake.NewSimpleClientset(node)
	p := &Platform{client: client}
	ctx := context.Background()

	listed, err := p.CordonedNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("cordoned nodes = %+v; a node whose owner set cannot be read must be left for a "+
			"human rather than judged by its reason alone", listed)
	}
	if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"}); err == nil {
		t.Fatal("releasing against an unreadable owner set reported success; the caller cannot know " +
			"whether it was the last hold on the machine")
	}
	if !nodeState(t, client, "gpu-1").Spec.Unschedulable {
		t.Fatal("a node with an unreadable owner set was uncordoned")
	}
}

// TestAnUnreadableHeldSetLeavesTheNodeAlone: the same rule for the record of
// which holds a HUMAN owns, and for a sharper reason.
//
// Reporting the node without that record tells the janitor nobody owns any of
// its holds, and the first pass that finds a pruned incident row then hands back
// a machine somebody deliberately took out of service — which is the exact
// inversion the held mark was added to prevent.
func TestAnUnreadableHeldSetLeavesTheNodeAlone(t *testing.T) {
	node := gpuNode("gpu-1", "8")
	node.Spec.Unschedulable = true
	node.Annotations = map[string]string{
		cordonReasonAnnotation:     "kubeneuron: ecc-dbe (inc-1)",
		cordonOwnersAnnotation:     `["inc-1","inc-2"]`,
		cordonHeldOwnersAnnotation: "inc-1", // somebody edited it by hand
		cordonRestoreAnnotation:    "",
	}
	client := fake.NewSimpleClientset(node)
	p := &Platform{client: client}
	ctx := context.Background()

	listed, err := p.CordonedNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("cordoned nodes = %+v; a node whose held-hold record cannot be read must be left "+
			"for a human, not reported as one nobody has taken charge of", listed)
	}
	if !nodeState(t, client, "gpu-1").Spec.Unschedulable {
		t.Fatal("a node with an unreadable held-hold record was uncordoned")
	}
}

// TestCordonedNodesReportsEveryHolder: the janitor evaluates one incident per
// hold, so a listing that carries only the reason cannot see the abandoned hold
// whose reason was overwritten by a later cordon.
func TestCordonedNodesReportsEveryHolder(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-2", "kubeneuron: xid-79 (inc-2)"); err != nil {
		t.Fatal(err)
	}

	listed, err := p.CordonedNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("cordoned nodes = %+v, want the one node we cordoned", listed)
	}
	if len(listed[0].Owners) != 2 {
		t.Fatalf("owners = %v; the janitor can only decide about the holds it is told about, and an "+
			"abandoned hold it never sees keeps a GPU node out of the fleet forever", listed[0].Owners)
	}
}

// TestUncordonTakesTheHeldMarkWithIt: the mark is designed to outlive the
// incident row, so one left on a node that is back in service is read by the
// next janitor pass as "a human owns this cordon" — and the next incident to
// cordon this machine can then never have its cordon cleaned up.
func TestUncordonTakesTheHeldMarkWithIt(t *testing.T) {
	node := gpuNode("gpu-1", "8")
	node.Spec.Unschedulable = true
	node.Annotations = map[string]string{
		cordonReasonAnnotation:  "kubeneuron: ecc-dbe (inc-1)",
		cordonRestoreAnnotation: "",
		cordonHeldAnnotation:    "true",
		doNotDisruptAnnotation:  "true",
	}
	client := fake.NewSimpleClientset(node)
	p := &Platform{client: client}

	if err := p.Uncordon(context.Background(), "gpu-1"); err != nil {
		t.Fatal(err)
	}
	if _, held := nodeState(t, client, "gpu-1").Annotations[cordonHeldAnnotation]; held {
		t.Fatal("a held mark survived the uncordon that put this node back into service; the next " +
			"remediation to cordon it inherits a mark saying a human owns the cordon, and the " +
			"janitor will never release it")
	}
}

// TestAnUnownedUncordonRefusesASharedCordon: Uncordon carries no owner, so on a
// reference-counted cordon it cannot know whether it is the last one out.
func TestAnUnownedUncordonRefusesASharedCordon(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-2", "kubeneuron: xid-79 (inc-2)"); err != nil {
		t.Fatal(err)
	}
	if err := p.Uncordon(ctx, "gpu-1"); err != nil {
		t.Fatal(err)
	}
	if !nodeState(t, client, "gpu-1").Spec.Unschedulable {
		t.Fatal("an unowned uncordon released a node two remediations were holding; whichever of " +
			"them is mid-reset now shares the machine with freshly scheduled tenant work")
	}
}

// TestAReleaseDoesNotDiscardAVerdictReachedWhileItRead.
//
// The owner set and the held-owner record are two annotations that must move
// together, and each is written by a path that touches only one of them.
// MarkCordonHeldIfOwner adds a verdict without changing the owner set;
// ReleaseCordonOwners rewrites BOTH but pinned only the owner set on the way
// out. The owner-set test op therefore cannot see a verdict that landed since
// the read, and the release overwrote it from a stale value.
//
// The bill is not paid in the window. It is paid weeks later, when retention
// prunes the halted incident's row: the janitor sees a hold nothing explains,
// releases it, and the last release hands the machine back to the scheduler —
// with the GPU an engineer deliberately withdrew (a pending RMA, a card that
// fails ECC under load) back in the pool. At 3am the SRE sees a node they took
// out of service running tenant jobs and no audit line saying who put it back.
func TestAReleaseDoesNotDiscardAVerdictReachedWhileItRead(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-2", "kubeneuron: xid-79 (inc-2)"); err != nil {
		t.Fatal(err)
	}
	// A human has already taken charge of inc-1's hold.
	if _, err := p.MarkCordonHeldIfOwner(ctx, "gpu-1", "inc-1"); err != nil {
		t.Fatal(err)
	}

	// The view inc-1's release is about to be handed: taken before the janitor
	// pass below, which is what makes its read stale.
	stale := nodeState(t, client, "gpu-1").DeepCopy()

	// A janitor pass now reaches a verdict about inc-2. It moves the held-owner
	// record and leaves the owner set exactly as it was — precisely the write the
	// release's own compare-and-swap on the owner set cannot detect.
	if _, err := p.MarkCordonHeldIfOwner(ctx, "gpu-1", "inc-2"); err != nil {
		t.Fatal(err)
	}

	// inc-1's playbook hands its hold back, having read the node a moment before
	// that verdict landed. Only the FIRST read is stale; a retry sees the truth,
	// which is the whole point of a compare-and-swap that pins the right thing.
	react := k8stesting.ObjectReaction(client.Tracker())
	client.PrependReactor("get", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if stale == nil {
			return react(action)
		}
		was := stale
		stale = nil
		return true, was, nil
	})
	if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"}); err != nil {
		t.Fatal(err)
	}

	listed, err := p.CordonedNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("cordoned nodes = %+v, want the one node inc-2 still holds", listed)
	}
	if !listed[0].HeldBy("inc-2") {
		t.Fatalf("held owners = %v; inc-1's release discarded the verdict a human reached about "+
			"inc-2 while it was reading. Once retention prunes inc-2's row the janitor sees an "+
			"unexplained hold, releases it, and a GPU somebody deliberately withdrew is back in "+
			"the schedulable pool", listed[0].HeldOwners)
	}
}

// TestAHeldVerdictByReasonRefusesACountedCordon.
//
// MarkCordonHeldIfReason writes the NODE-scoped held mark, and that mark is the
// one thing CordonedNode.HeldBy will not consult on a counted cordon — HeldBy
// answers from the held-OWNER record there, because a node-scoped answer strands
// every other hold on the machine. So a verdict written by this path onto a
// counted cordon is invisible to every reader of it: written, and worth nothing.
//
// The janitor picks between the two by CordonedNode.Tracked(), decided from an
// informer listing. A node listed before its first owner joined is tracked by
// the time the write lands, and the verdict evaporates. Weeks later retention
// prunes the halted incident's row, the janitor finds a hold nothing explains,
// and hands a machine a human took out of service back to the scheduler.
func TestAHeldVerdictByReasonRefusesACountedCordon(t *testing.T) {
	const reason = "kubeneuron: ecc-dbe (inc-1)"
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", reason); err != nil {
		t.Fatal(err)
	}

	if _, err := p.MarkCordonHeldIfReason(ctx, "gpu-1", reason); err != nil {
		t.Fatal(err)
	}
	listed, err := p.CordonedNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("cordoned nodes = %+v, want the one node we cordoned", listed)
	}
	_, stamped := nodeState(t, client, "gpu-1").Annotations[cordonHeldAnnotation]
	if stamped && !listed[0].HeldBy("inc-1") {
		t.Fatalf("a human's verdict was stamped onto the node but named no hold, and HeldBy "+
			"(held owners = %v) cannot see it. The janitor reads the hold as unexplained the "+
			"moment retention prunes the incident row and puts a machine a human took out of "+
			"service back into the fleet", listed[0].HeldOwners)
	}
}

// TestAnUnreadableCordonStateIsCountedNotJustLogged.
//
// Refusing to act on a cordon whose annotations do not parse is right: guessing
// at an owner set releases a machine several remediations may be holding
// mid-reset. But the refusal costs a GPU node held out of the fleet by nothing
// that can be asked to let go, and the only trace was a single log line,
// deduplicated forever after. Nobody greps for a warning they have never seen.
//
// At 3am the SRE has a cluster short of capacity, an ordinary-looking cordon in
// kubectl, no incident row (retention swept it long ago) and no owner to ask.
// The count says how many machines are in that state right now, and — because it
// is recomputed over the whole listing — falls back to zero on its own once the
// annotation is fixed, which a counter could never do.
func TestAnUnreadableCordonStateIsCountedNotJustLogged(t *testing.T) {
	node := gpuNode("gpu-1", "8")
	node.Spec.Unschedulable = true
	node.Annotations = map[string]string{
		cordonReasonAnnotation: "kubeneuron: ecc-dbe (inc-1)",
		cordonOwnersAnnotation: "inc-1, inc-2", // somebody edited it by hand
	}
	// A second, healthy cordon: the count must be of stranded machines, not of
	// every node this product holds.
	other := gpuNode("gpu-2", "8")
	other.Spec.Unschedulable = true
	other.Annotations = map[string]string{
		cordonReasonAnnotation:  "kubeneuron: xid-79 (inc-9)",
		cordonOwnersAnnotation:  `["inc-9"]`,
		cordonRestoreAnnotation: "",
	}
	client := fake.NewSimpleClientset(node, other)
	p := &Platform{client: client}
	ctx := context.Background()

	if _, err := p.CordonedNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(metrics.CordonsUnreadable); got != 1 {
		t.Fatalf("unreadable cordon nodes = %v, want 1: a GPU node is held out of the fleet by an "+
			"annotation nothing can release, and the only signal is one log line printed once", got)
	}

	// And it clears itself when a human fixes the annotation, or the alert it
	// backs stays lit for good and gets silenced.
	node.Annotations[cordonOwnersAnnotation] = `["inc-1"]`
	if _, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CordonedNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(metrics.CordonsUnreadable); got != 0 {
		t.Fatalf("unreadable cordon nodes = %v after the annotation was repaired, want 0: an alert "+
			"that never clears is an alert somebody silences", got)
	}
}

// TestEvictionGraceNeverForceDeletes is the guard for the defect the clamp
// introduced: a grace of zero on the wire is a force-delete, and the platform
// contract states there is deliberately no way to express it.
//
// Driven across the whole gradient rather than one case, because the clamp
// reached zero by ARITHMETIC, not by a branch anybody wrote — the PDB retry
// loop recomputes it every 5s, so a contended drain walks the budget down
// through every value below on its way to the deadline.
func TestEvictionGraceNeverForceDeletes(t *testing.T) {
	pod := &corev1.Pod{}
	long := int64(600) // a job that checkpoints on SIGTERM
	pod.Spec.TerminationGracePeriodSeconds = &long

	for _, remaining := range []time.Duration{
		30 * time.Minute, 11 * time.Minute, 10 * time.Minute, time.Minute,
		35 * time.Second, 30 * time.Second, 10 * time.Second, time.Second, 0,
	} {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(remaining))
		grace, capped := evictionGrace(ctx, pod, platform.DrainOptions{})
		cancel()

		if !capped {
			continue // the pod keeps its own period; nothing goes on the wire
		}
		if grace <= 0 {
			t.Fatalf("with %s left the clamp emitted GracePeriodSeconds=%d: the pod is removed "+
				"from etcd at once and SIGKILLed with no SIGTERM, so a tenant who asked for 600s "+
				"to checkpoint before a GPU reset gets none — and waitDrained then reports the "+
				"node drained, letting the ladder reset a GPU whose process may still hold the "+
				"device", remaining, int64(grace/time.Second))
		}
		if grace < corev1DefaultGracePeriod {
			t.Fatalf("with %s left the clamp emitted %s, below Kubernetes' own default of %s; "+
				"below that this is a force-delete in all but name", remaining, grace,
				corev1DefaultGracePeriod)
		}
	}
}

// TestEvictionGraceStillClampsWhenItCan: the guard above must not be satisfied
// by never clamping at all. A tenant-declared period is unbounded, and honouring
// it unconditionally hands the drain's duration to whoever wrote the pod — a
// spec declaring 24h cannot terminate inside a 30-minute step, so the drain
// times out and the incident climbs the whole ladder having repaired nothing.
func TestEvictionGraceStillClampsWhenItCan(t *testing.T) {
	pod := &corev1.Pod{}
	day := int64(86400)
	pod.Spec.TerminationGracePeriodSeconds = &day

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(10*time.Minute))
	defer cancel()
	grace, capped := evictionGrace(ctx, pod, platform.DrainOptions{})
	if !capped {
		t.Fatal("a pod declaring 24h was left unclamped inside a 10m step; the tenant can deny " +
			"remediation of the node they are running on")
	}
	if grace <= 0 || grace >= 10*time.Minute {
		t.Fatalf("clamped to %s, which is not inside the step's remaining budget", grace)
	}
}

// TestAcceleratorCountVendorMatrix pins what each vendor's resources MEAN.
//
// The count used to be decided by a switch that read "kind == gpu, or the
// domain is Intel's" — so every Intel resource was whole devices, and one card
// advertising millicores: 1000 was reported as a thousand GPUs. That number
// reaches kubeneuron_degraded_gpu_seconds_total and the degraded-GPU gauge, so
// a single node-scoped incident on one card billed a thousand GPU-hours of
// degraded capacity and tripped every fleet-fraction alert. A clamp added a
// round earlier masked it at two or more cards and left the single-card case
// exposed — doing real work, for the wrong reason.
//
// Driven as a matrix, and asserting BOTH directions, because the over-count had
// three siblings that reported too many and four that reported none at all.
func TestAcceleratorCountVendorMatrix(t *testing.T) {
	q := resource.MustParse
	for _, tc := range []struct {
		name     string
		capacity corev1.ResourceList
		labels   map[string]string
		want     int
		fleet    bool
		why      string
	}{
		{
			name: "one Intel card with its full advertisement",
			capacity: corev1.ResourceList{
				"gpu.intel.com/i915": q("1"), "gpu.intel.com/millicores": q("1000"),
				"gpu.intel.com/memory.max": q("17179869184"), "gpu.intel.com/tiles": q("1"),
			},
			want: 1, fleet: true,
			why: "millicores are a fraction of ONE card; it reported 1000",
		},
		{
			name:     "one Ponte Vecchio card with two tiles",
			capacity: corev1.ResourceList{"gpu.intel.com/i915": q("1"), "gpu.intel.com/tiles": q("2")},
			want:     1, fleet: true,
			why: "tiles are within a card; a silent 2x, well under the clamp",
		},
		{
			name:     "Intel monitoring handle alone",
			capacity: corev1.ResourceList{"gpu.intel.com/i915_monitoring": q("1")},
			want:     0, fleet: false,
			why: "a bookkeeping handle is not hardware, and must not admit a node to the " +
				"set of machines a playbook may cordon, drain and reboot",
		},
		{
			name:     "Habana Gaudi",
			capacity: corev1.ResourceList{"habana.ai/gaudi": q("8")},
			want:     8, fleet: true,
			why: "habana.ai was in the domain allow-list but unreachable — no gaudi kind is " +
				"GPU-shaped — so a whole Gaudi fleet was invisible, and silently: the " +
				"unrecognised-domain warning only fires for GPU-shaped names",
		},
		{
			name:     "AWS Neuron",
			capacity: corev1.ResourceList{"aws.amazon.com/neuron": q("16"), "aws.amazon.com/neuroncore": q("32")},
			want:     16, fleet: true,
			why: "Inf2/Trn1 were invisible entirely, in a repo that ships an AWS cloud provider",
		},
		{
			name:     "Aliyun fractional scheduling",
			capacity: corev1.ResourceList{"aliyun.com/gpu-count": q("4"), "aliyun.com/gpu-mem": q("65536")},
			want:     4, fleet: true,
			why: "gpu-count is devices; gpu-mem is MiB. Claimed support that reported 0",
		},
		{
			name:     "NVIDIA plain",
			capacity: corev1.ResourceList{"nvidia.com/gpu": q("8")},
			want:     8, fleet: true,
		},
		{
			name:     "NVIDIA time-sliced, four replicas per card",
			capacity: corev1.ResourceList{"nvidia.com/gpu": q("32")},
			labels:   map[string]string{"nvidia.com/gpu.replicas": "4"},
			want:     8, fleet: true,
			why: "the same multiplication as Intel's millicores, wearing the resource name " +
				"everything else trusts, and small enough to stay under the clamp",
		},
		{
			name:     "NVIDIA fully MIG'd",
			capacity: corev1.ResourceList{"nvidia.com/mig-1g.5gb": q("56")},
			want:     0, fleet: true,
			why: "partitions never yield a device count; 0 means unknown",
		},
		{
			name:     "AMD",
			capacity: corev1.ResourceList{"amd.com/gpu": q("4")},
			want:     4, fleet: true,
		},
		{
			name:     "a hostile capacity",
			capacity: corev1.ResourceList{"nvidia.com/gpu": q("9223372036854775807")},
			want:     0, fleet: true,
			why: "node-written; unknowable is 0, and it must not reach make([]GPUInfo, n)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceleratorCount(tc.capacity, tc.labels); got != tc.want {
				t.Errorf("acceleratorCount = %d, want %d — %s", got, tc.want, tc.why)
			}
			if got := nodeAdvertisesAccelerator(tc.capacity); got != tc.fleet {
				t.Errorf("nodeAdvertisesAccelerator = %v, want %v — %s", got, tc.fleet, tc.why)
			}
		})
	}
}

// TestLocalScratchIsNamedNotRefused pins the shape of the answer to kubectl's
// second abort condition.
//
// kubectl drain refuses on local data and makes you type
// --delete-emptydir-data. Refusing here would be wrong: nearly every serious
// GPU workload mounts an emptyDir for /dev/shm, so a refusal would make almost
// every GPU node in a real fleet undrainable. The pod reschedules; only the
// data does not follow it. So the drain proceeds and says what it destroyed.
func TestLocalScratchIsNamedNotRefused(t *testing.T) {
	withScratch := func(name string, medium corev1.StorageMedium) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					Kind: "ReplicaSet", Name: "rs", Controller: ptrTrue(),
				}},
			},
			Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
				Name:         "scratch",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: medium}},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}

	got := podsWithLocalData([]corev1.Pod{
		withScratch("checkpointer", ""),                     // disk-backed: real data
		withScratch("shm-only", corev1.StorageMediumMemory), // tmpfs: nothing survives a restart anyway
	})

	if len(got) != 1 || got[0] != "default/checkpointer" {
		t.Fatalf("pods with destroyable scratch = %v, want exactly [default/checkpointer]; a "+
			"training job's checkpoints are lost here and nothing recorded that it happened", got)
	}
}

func ptrTrue() *bool { b := true; return &b }

// setHandoff models the one thing a person can do that our own state can never
// contain: claim a node KubeNeuron cordoned.
func setHandoff(t *testing.T, client *fake.Clientset, node, why string) {
	t.Helper()
	patch := []byte(`{"metadata":{"annotations":{"kubeneuron.io/cordon-handoff":"` + why + `"}}}`)
	if _, err := client.CoreV1().Nodes().Patch(context.Background(), node,
		k8stypes.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
}

// TestTheLastReleaseRestoresANodeNobodyClaimed is the control: without a
// takeover the machine goes back into the fleet exactly as before.
func TestTheLastReleaseRestoresANodeNobodyClaimed(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()

	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"}); err != nil {
		t.Fatal(err)
	}
	got, err := client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Unschedulable {
		t.Fatal("a node nobody claimed stayed cordoned after its last owner left")
	}
	for _, ann := range cordonAnnotations {
		if _, present := got.Annotations[ann]; present {
			t.Fatalf("%s survived the restore; the next janitor pass reports a stuck cordon "+
				"nobody can act on", ann)
		}
	}
}

// TestAHumanTakeoverSurvivesTheLastRelease is the defect.
//
// An engineer looks at a cordoned node at 3am and decides to keep it out of
// service. They set the same unschedulable flag we had — because that is the
// only flag there is — so the last release compared against its snapshot,
// concluded the marks were its own, and put the machine back in front of the
// scheduler over their decision. The takeover annotation is the one signal our
// own state can never contain.
func TestAHumanTakeoverSurvivesTheLastRelease(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()

	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}
	setHandoff(t, client, "gpu-1", "bad riser, RMA raised — leave it down")

	if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"}); err != nil {
		t.Fatal(err)
	}

	got, err := client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Unschedulable {
		t.Fatal("the last release returned a node an engineer had explicitly taken over; tenant " +
			"work is now scheduling onto a machine somebody is working on")
	}
	if got.Annotations[cordonHandoffAnnotation] == "" {
		t.Fatal("the controller removed the takeover annotation; only the person who set it may")
	}
	// Our own bookkeeping still comes off, or the node collects stuck-cordon
	// reports for an incident that finished.
	for _, ann := range cordonAnnotations {
		if _, present := got.Annotations[ann]; present {
			t.Fatalf("%s survived a handed-off release", ann)
		}
	}
}

// TestATakeoverDoesNotAffectANonFinalRelease: the handoff governs the moment
// the node would go back to the scheduler, and nothing else. A release that is
// not the last one changes no node state either way.
func TestATakeoverDoesNotAffectANonFinalRelease(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()

	for _, id := range []string{"inc-1", "inc-2"} {
		if err := p.CordonForOwner(ctx, "gpu-1", id, "kubeneuron: ecc-dbe ("+id+")"); err != nil {
			t.Fatal(err)
		}
	}
	setHandoff(t, client, "gpu-1", "leave it down")

	released, remaining, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if released {
		t.Fatal("a non-final release reported the node released")
	}
	if remaining != 1 {
		t.Fatalf("remaining holders = %d, want 1", remaining)
	}
	got, err := client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Unschedulable || got.Annotations[cordonHandoffAnnotation] == "" {
		t.Fatal("a non-final release disturbed the node or the takeover")
	}
}

// TestATakeoverPlacedMidReleaseIsNotRacedPast is the concurrency requirement.
//
// A GET, a check and a PATCH is not a check: the window between reading the
// node and writing the restore is exactly long enough for somebody to claim it.
// So the restore carries a `test` op asserting no takeover, and the apiserver —
// not our own re-read — is what rejects a write computed before the claim.
//
// Modelled by building the restore from the node as it was BEFORE the takeover
// and then applying it after, which is precisely the interleaving. Injecting
// the claim from inside a fake-client reactor would deadlock: the reactor runs
// under the tracker's lock and setting the annotation calls back into it.
func TestATakeoverPlacedMidReleaseIsNotRacedPast(t *testing.T) {
	client := fake.NewSimpleClientset(gpuNode("gpu-1", "8"))
	applyAPIServerPatchSemantics(client)
	p := &Platform{client: client}
	ctx := context.Background()

	if err := p.CordonForOwner(ctx, "gpu-1", "inc-1", "kubeneuron: ecc-dbe (inc-1)"); err != nil {
		t.Fatal(err)
	}

	// What the release sees when it reads: no takeover yet.
	before, err := client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ops := p.restoreOps("gpu-1", before)

	// An engineer claims the node in the window.
	setHandoff(t, client, "gpu-1", "claimed mid-release")

	patch, err := json.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CoreV1().Nodes().Patch(ctx, "gpu-1", k8stypes.JSONPatchType, patch, metav1.PatchOptions{})
	if err == nil {
		t.Fatal("a restore computed before the takeover was accepted after it; the node went " +
			"back to the scheduler while an engineer was claiming it")
	}
	if !apierrors.IsInvalid(err) && !apierrors.IsConflict(err) {
		t.Fatalf("the write failed for the wrong reason: %v", err)
	}

	got, err := client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Unschedulable || got.Annotations[cordonHandoffAnnotation] == "" {
		t.Fatal("the rejected write still changed the node")
	}

	// And the retry that follows such a rejection does the right thing: it
	// re-reads, sees the takeover, and leaves the node down.
	if _, _, err := p.ReleaseCordonOwners(ctx, "gpu-1", []string{"inc-1"}); err != nil {
		t.Fatal(err)
	}
	if got, err = client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Unschedulable {
		t.Fatal("the retry after the rejected write released the node anyway")
	}
}
