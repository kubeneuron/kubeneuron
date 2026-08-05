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
		n.Status.Capacity = corev1.ResourceList{gpuResource: resource.MustParse(gpus)}
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
	err := p.Drain(context.Background(), "n1", platform.DrainOptions{Timeout: 5 * time.Second, GracePeriod: 30 * time.Second})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != "app" {
		t.Fatalf("evicted = %v, want only the ReplicaSet pod", evicted)
	}
	// DaemonSet and unmanaged pods must survive a non-forced drain.
	for _, name := range []string{"ds", "bare"} {
		if _, err := client.CoreV1().Pods("default").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Fatalf("pod %s must survive drain: %v", name, err)
		}
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
			Limits: corev1.ResourceList{gpuResource: resource.MustParse("1")},
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
