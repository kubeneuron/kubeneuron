// Package kubernetes implements platform.Platform for Kubernetes clusters.
// Inventory comes from watching Nodes that advertise a GPU extended resource
// from a recognised vendor domain (see acceleratorDomains);
// workload control uses cordon (spec.unschedulable) and the Eviction API so
// PodDisruptionBudgets are respected.
package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
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
	"strings"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// acceleratorDomains are the extended-resource domains whose GPU resources
// KubeNeuron will treat as evidence that a node belongs to the GPU fleet.
//
// This list is deliberately an allow-list rather than a shape test. Fleet
// membership is a blast-radius decision: a node in the inventory can be
// cordoned, drained, tainted and rebooted by a playbook, so a resource named
// "example.com/gpu-licence" — a licence count, not a device — must not drag
// a CPU node into the set of machines KubeNeuron may act on.
var acceleratorDomains = map[string]struct{}{
	"nvidia.com":    {},
	"amd.com":       {},
	"intel.com":     {},
	"gpu.intel.com": {},
	"habana.ai":     {},
	"aliyun.com":    {}, // gpu-mem / gpu-count, fractional-GPU scheduling
}

// isAcceleratorResource reports whether an extended-resource name names a GPU
// or GPU partition, for ANY vendor. This is the PERMISSIVE matcher: it
// decides which pods are holding a device, and is used only where a false
// positive is cheap.
//
// Hardcoding "nvidia.com/gpu" made two failures silent rather than loud: an
// AMD or Intel node inventoried as ZERO GPUs (invisible to the whole control
// plane), and `evict_gpu_workload` reported "evicted 0 GPU workloads"
// successfully while leaving live jobs on a device about to be reset — the
// exact disruption the step exists to prevent. Both are wrong in the
// dangerous direction, so this matcher errs toward recognising an
// accelerator: a false positive costs one unnecessary eviction on a node that
// is being drained anyway, while a false negative destroys somebody's
// training run.
//
// MIG partitions (nvidia.com/mig-1g.5gb under the device plugin's mixed
// strategy) are included for the same reason: a pod holding a partition is a
// pod on the physical device being remediated.
//
// Do NOT use this to decide fleet membership — see
// nodeAdvertisesAccelerator, where the asymmetry runs the other way.
func isAcceleratorResource(name corev1.ResourceName) bool {
	_, ok := acceleratorResourceKind(name)
	return ok
}

// acceleratorResourceKind splits a resource name and reports whether its kind
// has an accelerator shape, returning the domain that claimed it.
func acceleratorResourceKind(name corev1.ResourceName) (domain string, ok bool) {
	resource := string(name)
	// A core resource (cpu, memory, ephemeral-storage) has no domain prefix;
	// only domain-qualified extended resources can name an accelerator.
	domain, kind, qualified := strings.Cut(resource, "/")
	if !qualified {
		return "", false
	}
	switch {
	case kind == "gpu" || strings.HasPrefix(kind, "gpu-") || strings.HasPrefix(kind, "gpu."):
		// nvidia.com/gpu, amd.com/gpu, nvidia.com/gpu.shared, and any future
		// <vendor>/gpu.
		return domain, true
	case strings.HasPrefix(kind, "mig-"):
		// NVIDIA MIG compute instances.
		return domain, true
	case domain == "gpu.intel.com":
		// Intel advertises the device family as the kind (i915, xe).
		return domain, true
	}
	return "", false
}

// unknownAcceleratorDomains remembers which GPU-shaped resources from
// unrecognised domains we have already complained about, so the warning is
// one line per domain rather than one per List call.
var unknownAcceleratorDomains sync.Map

// nodeAdvertisesAccelerator reports whether a node's capacity names a GPU
// from a domain KubeNeuron recognises. Used for fleet membership: a node this
// returns false for is not a GPU node as far as KubeNeuron is concerned, and
// no playbook can touch it.
//
// The asymmetry with isAcceleratorResource is intentional. On the eviction
// side an over-match costs one extra eviction on a node already being
// drained. Here an over-match admits a machine to the set KubeNeuron may
// cordon, drain and reboot, so this side must be sure rather than generous.
// An accelerator we fail to recognise is logged loudly instead of silently
// dropped, because an invisible GPU node is the other failure worth catching.
func nodeAdvertisesAccelerator(capacity corev1.ResourceList) bool {
	found := false
	for name := range capacity {
		domain, shaped := acceleratorResourceKind(name)
		if !shaped {
			continue
		}
		if _, known := acceleratorDomains[domain]; known {
			found = true
			continue
		}
		if _, warned := unknownAcceleratorDomains.LoadOrStore(domain, struct{}{}); !warned {
			slog.Warn("ignoring GPU-shaped resource from an unrecognised vendor domain; "+
				"this node will not be treated as part of the GPU fleet",
				"resource", string(name), "domain", domain)
		}
	}
	return found
}

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
var _ platform.NodeLabeler = (*Platform)(nil)

// New builds a Platform from an in-cluster config, falling back to the given
// kubeconfig path (empty means default loading rules).
// clientQPS and clientBurst replace client-go's 5/10 defaults. See New.
const (
	clientQPS   = 50
	clientBurst = 100
)

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
	// client-go defaults to 5 QPS / 10 burst, which this controller cannot
	// live within.
	//
	// A janitor pass does a live Get plus a Patch per marked node and a full
	// List; a drain issues one Evict per pod plus a List every five seconds.
	// On a fifty-node degraded set that is a hundred serialised requests, and
	// twenty seconds of pure client-side throttling per pass — which presents
	// as unexplained reconcile latency, never as an error, because the limiter
	// simply waits.
	//
	// The apiserver's own priority-and-fairness is the real limit; this one was
	// a default nobody chose.
	cfg.QPS = clientQPS
	cfg.Burst = clientBurst
	// Named, so this controller is identifiable in an apiserver audit log
	// rather than appearing as an anonymous Go client.
	cfg.UserAgent = "kubeneuron-controller"

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
				if !nodeAdvertisesAccelerator(n.Status.Capacity) {
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
		if !nodeAdvertisesAccelerator(n.Status.Capacity) {
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

// NodeLabels implements platform.NodeLabeler by reading the Kubernetes Node
// object itself, never the GPU-filtered inventory — see the interface for why
// that distinction is load-bearing for confinement.
func (p *Platform) NodeLabels(ctx context.Context, node string) (map[string]string, bool, error) {
	if lp := p.nodeLister.Load(); lp != nil {
		// The watch-maintained cache: fresher than a periodic sync and cheaper
		// than an apiserver round trip.
		if n, err := (*lp).Get(node); err == nil {
			if n.Labels == nil {
				return map[string]string{}, true, nil
			}
			return n.Labels, true, nil
		} else if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		// Any other lister error falls through to the live read.
	}
	n, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if n.Labels == nil {
		return map[string]string{}, true, nil
	}
	return n.Labels, true, nil
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
				if !nodeAdvertisesAccelerator(n.Status.Capacity) {
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

// cordonRestoreAnnotation records what the node looked like before we cordoned
// it, so Uncordon puts it back rather than clearing marks it did not place.
const cordonRestoreAnnotation = "kubeneuron.io/cordon-restore"

// Cordon marks the node unschedulable, records the reason, and protects it from
// being replaced while the remediation runs.
func (p *Platform) Cordon(ctx context.Context, node string, reason string) error {
	// Record what was already there, so Uncordon restores rather than clears.
	//
	// Both of the things this writes may already be somebody else's. A human
	// may have run `kubectl cordon` on the node an hour ago; an operator may
	// have pinned it with karpenter.sh/do-not-disrupt because a long training
	// job is on it — which is exactly what that annotation is for. Clearing
	// them on the way out handed Karpenter a node its owner had deliberately
	// pinned, and returned to the scheduler a node a human had deliberately
	// removed from it.
	prior, err := p.priorCordonState(ctx, node)
	if err != nil {
		return err
	}
	patch, _ := json.Marshal(map[string]any{
		"spec": map[string]any{"unschedulable": true},
		"metadata": map[string]any{"annotations": map[string]string{
			cordonReasonAnnotation:  reason,
			cordonRestoreAnnotation: prior,
			doNotDisruptAnnotation:  "true",
		}},
	})
	_, err = p.client.CoreV1().Nodes().Patch(ctx, node, k8stypes.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}

// priorCordonState captures what a later Uncordon must put back. It is written
// only on the FIRST cordon: re-cordoning a node we already hold must not
// overwrite the original record with our own marks.
func (p *Platform) priorCordonState(ctx context.Context, node string) (string, error) {
	current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading %s before cordoning it: %w", node, err)
	}
	if existing, held := current.Annotations[cordonRestoreAnnotation]; held {
		return existing, nil
	}
	var parts []string
	if current.Spec.Unschedulable {
		parts = append(parts, "unschedulable")
	}
	if _, pinned := current.Annotations[doNotDisruptAnnotation]; pinned {
		parts = append(parts, "do-not-disrupt")
	}
	return strings.Join(parts, ","), nil
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
	// Restore, do not clear. See Cordon: either mark may have been somebody
	// else's before we touched the node, and the record says which.
	current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading %s before uncordoning it: %w", node, err)
	}
	prior := strings.Split(current.Annotations[cordonRestoreAnnotation], ",")
	keepUnschedulable := slices.Contains(prior, "unschedulable")
	keepPinned := slices.Contains(prior, "do-not-disrupt")

	annotations := map[string]*string{
		cordonReasonAnnotation:  nil,
		cordonRestoreAnnotation: nil,
	}
	if !keepPinned {
		annotations[doNotDisruptAnnotation] = nil
	}
	patch, _ := json.Marshal(map[string]any{
		"spec":     map[string]any{"unschedulable": keepUnschedulable},
		"metadata": map[string]any{"annotations": annotations},
	})
	_, err = p.client.CoreV1().Nodes().Patch(ctx, node, k8stypes.StrategicMergePatchType, patch, metav1.PatchOptions{})
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
	// Pods this drain cannot evict and did not: reported, never silently
	// dropped. See the refusal below.
	var unmanaged []string
	for i := range pods {
		pod := &pods[i]
		if skipDuringDrain(pod, opts.Force) {
			if unmanagedPod(pod) && !opts.Force {
				unmanaged = append(unmanaged, pod.Namespace+"/"+pod.Name)
			}
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
	if err := p.waitDrained(ctx, node, opts, time.NewTicker(drainPollInterval)); err != nil {
		return err
	}

	// A pod with no controller was never evicted, and the caller must not be
	// told the node is drained.
	//
	// This branch used to `continue` silently and Drain returned nil, so a
	// bare pod — kubectl run, a researcher's job, an orphan — sat on a machine
	// the ladder then rebooted. The comment claimed kubectl drain semantics;
	// kubectl does the opposite, aborting with an error naming the pods unless
	// --force is given, and that refusal is the whole point of the flag.
	//
	// Failing here does escalate the incident, which is the cost. It is the
	// right cost: escalation is visible and reversible, and a human reads the
	// names. Rebooting a node with somebody's unmanaged job still on it is
	// neither.
	if len(unmanaged) > 0 {
		sort.Strings(unmanaged)
		return fmt.Errorf("drain of %s: %d pod(s) have no controller and were not evicted (%s); "+
			"nothing will reschedule them, so this node is not drained",
			node, len(unmanaged), strings.Join(unmanaged, ", "))
	}
	return nil
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
//
// It handles a 404 and a 429 the way Drain does, and for the same reason Drain
// spells out: a step failure here escalates the incident to a MORE destructive
// rung — workload-restart's on_failure is drain-and-reset — so returning the
// raw error turned a deliberate availability guard, or a pod that had already
// exited, into draining and resetting the whole GPU.
//
// The 404 is the likelier of the two: an XID 94 kills the workload, its pod
// exits between listing and evicting, and a targeted restart escalates to a
// node-wide one for work that is already gone.
func (p *Platform) EvictWorkload(ctx context.Context, w platform.Workload) error {
	tick := time.NewTicker(drainPollInterval)
	defer tick.Stop()
	for {
		err := p.client.PolicyV1().Evictions(w.Namespace).Evict(ctx, &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: w.Name, Namespace: w.Namespace},
		})
		switch {
		case err == nil, apierrors.IsNotFound(err):
			return nil
		case apierrors.IsTooManyRequests(err):
			select {
			case <-ctx.Done():
				return fmt.Errorf("evicting %s/%s: still blocked by a PodDisruptionBudget: %w",
					w.Namespace, w.Name, ctx.Err())
			case <-tick.C:
			}
		default:
			return fmt.Errorf("evicting %s/%s: %w", w.Namespace, w.Name, err)
		}
	}
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
// unmanagedPod reports a pod with no controller — the one skipDuringDrain
// class that is somebody's live work rather than infrastructure.
func unmanagedPod(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}
	if _, isMirror := pod.Annotations[corev1.MirrorPodAnnotationKey]; isMirror {
		return false
	}
	return metav1.GetControllerOf(pod) == nil
}

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

// Init containers are walked too, not only regular ones.
//
// A native sidecar is an initContainer with restartPolicy: Always — GA since
// 1.29 — and it runs for the whole pod lifetime, so it can hold an accelerator
// for as long as the workload does. Missing it meant evict_gpu_workload
// reported "no accelerator workloads to evict" for a pod that was using the
// GPU, which is the silent failure the vendor-neutral matcher exists to
// eliminate.
//
// Limits alone is deliberate and stays: the apiserver rejects an extended
// resource declared with requests and no limit.
func podUsesGPU(pod *corev1.Pod) bool {
	for _, containers := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range containers {
			for name := range containers[i].Resources.Limits {
				if isAcceleratorResource(name) {
					return true
				}
			}
		}
	}
	return false
}

// acceleratorCount reads the device count off a node's capacity for whichever
// vendor advertises it.
//
// Reading only nvidia.com/gpu inventoried every AMD and Intel node as ZERO
// GPUs, which then surfaced as the report's AssumedSingleGPU fallback: the
// degraded-GPU-hours a non-NVIDIA fleet recovered were charged at one device
// per incident regardless of how many were really affected.
//
// Whole-device counts win over partition counts, and the maximum is taken
// rather than the sum: under the device plugin's mixed strategy one physical
// GPU is advertised twice (nvidia.com/gpu and nvidia.com/mig-1g.5gb), so
// summing would double-count the same silicon.
// maxAdvertisedAccelerators bounds a node-advertised device count. The largest
// real machines today carry a few dozen; a thousand leaves room for hardware
// nobody has shipped yet while keeping the allocation it drives trivial.
const maxAdvertisedAccelerators = 1024

func acceleratorCount(capacity corev1.ResourceList) int {
	whole, partitioned := 0, 0
	implausibleCapacity := false
	for name, q := range capacity {
		domain, shaped := acceleratorResourceKind(name)
		if !shaped {
			continue
		}
		if _, known := acceleratorDomains[domain]; !known {
			continue
		}
		_, kind, _ := strings.Cut(string(name), "/")
		switch {
		case kind == "gpu", domain == "gpu.intel.com":
			// Intel names the device family as the kind (i915, xe) and that
			// count IS whole devices, not partitions.
			// Clamped, because this number is written by the NODE, not by us.
			//
			// A kubelet may set its own status.capacity (NodeRestriction
			// permits it) and Quantity.Value() saturates at MaxInt64, so a
			// malformed or hostile value reached `make([]GPUInfo, n)` in
			// nodeFromK8s and panicked the controller — on the reconcile tick
			// and on the destructive-admission path, with the same object
			// waiting in the informer cache after the restart. There is no
			// recover() anywhere in this program, so one bad node crash-looped
			// the whole control plane.
			//
			// A count past the bound is not knowable capacity, so it takes the
			// path this function already has for that: zero, meaning unknown,
			// with the agent's own registration supplying the real number.
			if n := q.Value(); n > 0 && n <= maxAdvertisedAccelerators {
				whole = max(whole, int(n))
			} else if n > maxAdvertisedAccelerators {
				implausibleCapacity = true
			}
		default:
			// Everything else counts something that is not a physical device:
			// MIG instances, time-sliced replicas (nvidia.com/gpu.shared),
			// aliyun.com/gpu-mem's MiB. Only note that such a resource exists.
			partitioned++
		}
	}
	if whole > 0 {
		return whole
	}
	if implausibleCapacity {
		return 0 // unknowable, same as the partition case below
	}
	if partitioned > 0 {
		// The node advertises partitions or replicas but no whole devices, so
		// the physical device count is genuinely NOT KNOWABLE from capacity: a
		// fully-MIG'd A100 pair advertises 56 instances, and a time-sliced node
		// advertises whatever replica factor somebody chose. Reporting those as
		// GPUs put a fabricated capacity number in front of whoever pays for
		// the fleet.
		//
		// Zero here means "unknown", and it is short-lived: the agent's
		// self-registration reports the real devices, which is where every
		// number that matters comes from. Until then the fleet view shows the
		// node with no device count rather than a wrong one, and
		// affectedGPUs's own fallback charges one — undercounting, the honest
		// direction.
		return 0
	}
	return 0
}

func nodeFromK8s(n *corev1.Node) types.Node {
	gpus := acceleratorCount(n.Status.Capacity)
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
