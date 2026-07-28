// Package kubernetes implements platform.Platform for Kubernetes clusters.
// Inventory comes from watching Nodes with an nvidia.com/gpu capacity;
// workload control uses cordon (spec.unschedulable) and the Eviction API so
// PodDisruptionBudgets are respected.
package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
	client kubernetes.Interface
}

var _ platform.Platform = (*Platform)(nil)

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

// ListNodes returns all nodes advertising nvidia.com/gpu capacity.
func (p *Platform) ListNodes(ctx context.Context) ([]types.Node, error) {
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

// Cordon marks the node unschedulable and records the reason.
func (p *Platform) Cordon(ctx context.Context, node string, reason string) error {
	patch, _ := json.Marshal(map[string]any{
		"spec":     map[string]any{"unschedulable": true},
		"metadata": map[string]any{"annotations": map[string]string{cordonReasonAnnotation: reason}},
	})
	_, err := p.client.CoreV1().Nodes().Patch(ctx, node, k8stypes.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}

// Uncordon makes the node schedulable again and clears the reason.
func (p *Platform) Uncordon(ctx context.Context, node string) error {
	patch, _ := json.Marshal(map[string]any{
		"spec":     map[string]any{"unschedulable": false},
		"metadata": map[string]any{"annotations": map[string]*string{cordonReasonAnnotation: nil}},
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
