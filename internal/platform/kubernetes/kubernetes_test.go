package kubernetes

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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
			if got := acceleratorCount(tc.capacity); got != tc.wantGPUs {
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
	if got.Spec.Unschedulable {
		t.Fatal("the node was never released, so a cordon from before the upgrade is stranded forever")
	}
}
