// Package kubernetes implements platform.Platform for Kubernetes clusters.
// Inventory comes from watching Nodes with an nvidia.com/gpu capacity;
// workload control uses cordon (spec.unschedulable) and the Eviction API so
// PodDisruptionBudgets are respected.
package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// gpuResource is the extended resource GPU nodes advertise.
const gpuResource = "nvidia.com/gpu"

// cordonReasonAnnotation records why KubeNeuron cordoned a node.
const cordonReasonAnnotation = "kubeneuron.io/cordon-reason"

// drainPollInterval paces PDB-blocked eviction retries and the
// pods-terminated wait. Variable for tests.
var drainPollInterval = 5 * time.Second

// Platform implements platform.Platform on Kubernetes.
type Platform struct {
	client   kubernetes.Interface
	recycler platform.InstanceRecycler
	// nodeLister, once published by StartNodeCache after its informer syncs,
	// serves ListNodes from the watch-maintained cache instead of a per-call
	// apiserver List. Nil until synced (and always nil when the cache was
	// never started): callers then get the live List path.
	nodeLister atomic.Pointer[corev1listers.NodeLister]
}

var _ platform.Platform = (*Platform)(nil)
var _ platform.NodePresence = (*Platform)(nil)

// New builds a Platform from an in-cluster config, falling back to the given
// kubeconfig path (empty means default loading rules).
func New(kubeconfig string) (*Platform, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("kubernetes: no in-cluster config and no kubeconfig: %w", err)
		}
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Platform{client: client}, nil
}

// Name implements platform.Platform.
func (p *Platform) Name() string { return "kubernetes" }

// Client exposes the authenticated in-cluster client to controller subsystems
// that must validate Kubernetes-native workload identity, such as the agent
// TokenReview authenticator. Callers must still use narrowly scoped RBAC.
func (p *Platform) Client() kubernetes.Interface { return p.client }

// StartNodeCache begins an informer-backed watch of Nodes and, once the cache
// has synced, serves ListNodes from it instead of a per-call apiserver List.
// ListNodes sits on the controller's destructive-admission path (blast-radius
// confinement resolves node labels through it), so before this cache every
// admission check paid an apiserver round trip and apiserver latency fed
// straight back into remediation. Returns immediately; until the sync
// completes (or if ctx ends first) ListNodes keeps using live Lists, so a
// controller that cannot watch degrades to the old behavior rather than
// failing.
func (p *Platform) StartNodeCache(ctx context.Context) {
	factory := informers.NewSharedInformerFactory(p.client, 0)
	nodes := factory.Core().V1().Nodes()
	informer := nodes.Informer()
	lister := nodes.Lister()
	go func() {
		factory.Start(ctx.Done())
		if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
			return // shutdown before sync; live Lists remain in force
		}
		p.nodeLister.Store(&lister)
	}()
}

// ListNodes returns all nodes advertising nvidia.com/gpu capacity, from the
// informer cache when StartNodeCache has synced, else from a live List.
func (p *Platform) ListNodes(ctx context.Context) ([]types.Node, error) {
	if lp := p.nodeLister.Load(); lp != nil {
		cached, err := (*lp).List(labels.Everything())
		if err == nil {
			out := make([]types.Node, 0, len(cached))
			for _, n := range cached {
				if _, ok := n.Status.Capacity[gpuResource]; !ok {
					continue
				}
				out = append(out, nodeFromK8s(n))
			}
			// The cache iterates in map order; keep the inventory stable for
			// callers and logs.
			sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
			return out, nil
		}
		// A lister failure is unexpected; fall through to the live path.
	}
	list, err := p.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []types.Node
	for i := range list.Items {
		n := &list.Items[i]
		if _, ok := n.Status.Capacity[gpuResource]; !ok {
			continue
		}
		out = append(out, nodeFromK8s(n))
	}
	return out, nil
}

// NodeExists checks the Kubernetes Node object itself rather than the filtered
// GPU inventory. A lost device-plugin capacity must not close an active
// remediation as though the machine had been deleted.
func (p *Platform) NodeExists(ctx context.Context, node string) (bool, error) {
	_, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// WatchNodes streams GPU node inventory changes.
func (p *Platform) WatchNodes(ctx context.Context) (<-chan platform.NodeEvent, error) {
	w, err := p.client.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	ch := make(chan platform.NodeEvent, 16)
	go func() {
		defer close(ch)
		defer w.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.ResultChan():
				if !ok {
					return
				}
				n, ok := ev.Object.(*corev1.Node)
				if !ok {
					continue
				}
				if _, hasGPU := n.Status.Capacity[gpuResource]; !hasGPU {
					continue
				}
				var t platform.NodeEventType
				switch ev.Type {
				case "ADDED":
					t = platform.NodeAdded
				case "DELETED":
					t = platform.NodeRemoved
				default:
					t = platform.NodeUpdated
				}
				ch <- platform.NodeEvent{Type: t, Node: nodeFromK8s(n)}
			}
		}
	}()
	return ch, nil
}

// doNotDisruptAnnotation asks Karpenter to leave the node alone.
//
// A node autoscaler that consolidates or replaces a node in the middle of a
// remediation turns a controlled repair into an orphaned incident: the agent
// disappears, the evidence is bound to a node UID that no longer exists, and
// whatever the playbook had already done to the node goes with it. Cordoning is
// not enough — consolidation targets underutilised nodes, and a cordoned,
// drained node is the most underutilised node in the cluster.
const doNotDisruptAnnotation = "karpenter.sh/do-not-disrupt"

// Cordon marks the node unschedulable, records the reason, and protects it from
// being replaced while the remediation runs.
func (p *Platform) Cordon(ctx context.Context, node string, reason string) error {
	patch, _ := json.Marshal(map[string]any{
		"spec": map[string]any{"unschedulable": true},
		"metadata": map[string]any{"annotations": map[string]string{
			cordonReasonAnnotation: reason,
			doNotDisruptAnnotation: "true",
		}},
	})
	_, err := p.client.CoreV1().Nodes().Patch(ctx, node, k8stypes.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}

// CordonedNodes lists the nodes this product cordoned and has not released.
//
// The record lives on the node, so it survives a controller that dies between
// cordoning a node and finishing the playbook that cordoned it.
func (p *Platform) CordonedNodes(ctx context.Context) ([]platform.CordonedNode, error) {
	// Serve from the informer cache once synced: this runs on every reconcile
	// tick, and a live List per tick is exactly the apiserver load the node
	// cache exists to remove. The janitor tolerates the cache's staleness — a
	// cordon it misses this tick it sees the next, and Uncordon itself is a
	// live patch.
	if lp := p.nodeLister.Load(); lp != nil {
		cached, err := (*lp).List(labels.Everything())
		if err == nil {
			var out []platform.CordonedNode
			for _, node := range cached {
				reason, ours := node.Annotations[cordonReasonAnnotation]
				if !ours || !node.Spec.Unschedulable {
					continue
				}
				out = append(out, platform.CordonedNode{Name: node.Name, Reason: reason})
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
			return out, nil
		}
	}
	nodes, err := p.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []platform.CordonedNode
	for i := range nodes.Items {
		node := &nodes.Items[i]
		reason, ours := node.Annotations[cordonReasonAnnotation]
		if !ours || !node.Spec.Unschedulable {
			continue
		}
		out = append(out, platform.CordonedNode{Name: node.Name, Reason: reason})
	}
	return out, nil
}

// Uncordon makes the node schedulable again, clears the reason, and lifts the
// autoscaler protection the cordon put in place.
func (p *Platform) Uncordon(ctx context.Context, node string) error {
	patch, _ := json.Marshal(map[string]any{
		"spec": map[string]any{"unschedulable": false},
		"metadata": map[string]any{"annotations": map[string]*string{
			cordonReasonAnnotation: nil,
			doNotDisruptAnnotation: nil,
		}},
	})
	_, err := p.client.CoreV1().Nodes().Patch(ctx, node, k8stypes.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}

// Drain evicts all evictable pods from the node via the Eviction API,
// respecting PodDisruptionBudgets, then waits for them to terminate.
func (p *Platform) Drain(ctx context.Context, node string, opts platform.DrainOptions) error {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	pods, err := p.nodePods(ctx, node)
	if err != nil {
		return err
	}
	// Pods whose eviction a PodDisruptionBudget currently blocks (HTTP 429).
	// kubectl drain retries those until the budget frees up; failing the
	// step instead would escalate the incident to a more destructive rung,
	// which is exactly backwards for a deliberate availability guard.
	var blocked []*corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if skipDuringDrain(pod, opts.Force) {
			continue
		}
		switch err := p.evictPod(ctx, pod, opts); {
		case err == nil, apierrors.IsNotFound(err):
		case apierrors.IsTooManyRequests(err):
			blocked = append(blocked, pod)
		default:
			return fmt.Errorf("evicting %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	retry := time.NewTicker(drainPollInterval)
	defer retry.Stop()
	for len(blocked) > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("drain of %s: %d evictions still blocked by PodDisruptionBudgets: %w",
				node, len(blocked), ctx.Err())
		case <-retry.C:
		}
		still := blocked[:0]
		for _, pod := range blocked {
			switch err := p.evictPod(ctx, pod, opts); {
			case err == nil, apierrors.IsNotFound(err):
			case apierrors.IsTooManyRequests(err):
				still = append(still, pod)
			default:
				return fmt.Errorf("evicting %s/%s: %w", pod.Namespace, pod.Name, err)
			}
		}
		blocked = still
	}

	// Wait until evictable pods are gone.
	return p.waitDrained(ctx, node, opts, time.NewTicker(drainPollInterval))
}

// evictPod issues one Eviction API call for the pod.
func (p *Platform) evictPod(ctx context.Context, pod *corev1.Pod, opts platform.DrainOptions) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
	}
	if opts.GracePeriod >= 0 {
		secs := int64(opts.GracePeriod / time.Second)
		eviction.DeleteOptions = &metav1.DeleteOptions{GracePeriodSeconds: &secs}
	}
	return p.client.PolicyV1().Evictions(pod.Namespace).Evict(ctx, eviction)
}

// waitDrained polls until no evictable pod remains on the node.
func (p *Platform) waitDrained(ctx context.Context, node string, opts platform.DrainOptions, tick *time.Ticker) error {
	defer tick.Stop()
	for {
		remaining, err := p.nodePods(ctx, node)
		if err != nil {
			return err
		}
		n := 0
		for i := range remaining {
			if !skipDuringDrain(&remaining[i], opts.Force) {
				n++
			}
		}
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("drain of %s timed out with %d pods remaining: %w", node, n, ctx.Err())
		case <-tick.C:
		}
	}
}

// NodeWorkloads lists pods on the node.
func (p *Platform) NodeWorkloads(ctx context.Context, node string) ([]platform.Workload, error) {
	pods, err := p.nodePods(ctx, node)
	if err != nil {
		return nil, err
	}
	out := make([]platform.Workload, 0, len(pods))
	for i := range pods {
		out = append(out, platform.Workload{
			Name:      pods[i].Name,
			Namespace: pods[i].Namespace,
			Kind:      "Pod",
			UsesGPU:   podUsesGPU(&pods[i]),
		})
	}
	return out, nil
}

// EvictWorkload evicts a single pod (targeted restart, e.g. XID 94).
func (p *Platform) EvictWorkload(ctx context.Context, w platform.Workload) error {
	return p.client.PolicyV1().Evictions(w.Namespace).Evict(ctx, &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: w.Name, Namespace: w.Namespace},
	})
}

func (p *Platform) nodePods(ctx context.Context, node string) ([]corev1.Pod, error) {
	list, err := p.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node,
	})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// skipDuringDrain mirrors kubectl drain semantics: leave DaemonSet pods and
// mirror pods alone; leave unmanaged pods alone unless force is set.
func skipDuringDrain(pod *corev1.Pod, force bool) bool {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true
	}
	if _, isMirror := pod.Annotations[corev1.MirrorPodAnnotationKey]; isMirror {
		return true
	}
	ref := metav1.GetControllerOf(pod)
	if ref != nil && ref.Kind == "DaemonSet" {
		return true
	}
	if ref == nil && !force {
		return true
	}
	return false
}

func podUsesGPU(pod *corev1.Pod) bool {
	for i := range pod.Spec.Containers {
		if _, ok := pod.Spec.Containers[i].Resources.Limits[gpuResource]; ok {
			return true
		}
	}
	return false
}

func nodeFromK8s(n *corev1.Node) types.Node {
	gpus := 0
	if q, ok := n.Status.Capacity[gpuResource]; ok {
		gpus = int(q.Value())
	}
	// GPU UUIDs are not in the Node object; the agent's self-registration
	// fills them in. Capacity gives us the count for early display.
	infos := make([]types.GPUInfo, gpus)
	for i := range infos {
		infos[i] = types.GPUInfo{Index: i}
	}
	_, paused := n.Labels["kubeneuron.io/pause"]
	return types.Node{
		Name:     n.Name,
		UID:      string(n.UID),
		Platform: "kubernetes",
		Labels:   n.Labels,
		GPUs:     infos,
		BootID:   n.Status.NodeInfo.BootID,
		Paused:   paused,
	}
}
