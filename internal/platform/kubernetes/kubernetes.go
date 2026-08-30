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
	"strconv"
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

	"github.com/kubeneuron/kubeneuron/internal/metrics"
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
	"habana.ai":     {}, // gaudi/gaudi2/gaudi3/greco/goya
	"aliyun.com":    {}, // gpu-mem / gpu-count, fractional-GPU scheduling
	// Inf1/Inf2/Trn1. Absent until now, in a repo that ships an AWS cloud
	// provider: a Neuron node was invisible to the whole control plane, and
	// silently — the unrecognised-domain warning only fires for GPU-SHAPED
	// names, and "neuron" is not one.
	"aws.amazon.com": {},
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

// acceleratorResourceClass says what an extended resource actually COUNTS.
//
// Three different questions used to be answered from one boolean and a switch
// spelled out at each call site — is this node in the fleet, how many devices
// does it have, does this pod hold a GPU — and they drifted apart exactly the
// way every other inline-enumerated set in this codebase has. The table below
// is the single answer all three read.
type acceleratorResourceClass int

const (
	// notAnAccelerator: not a GPU resource at all.
	notAnAccelerator acceleratorResourceClass = iota
	// wholeDevice: the quantity IS a count of physical accelerators.
	wholeDevice
	// devicePartition: the quantity counts slices, replicas, cores,
	// millicores or megabytes of a device — never devices. Its presence marks
	// the node as an accelerator node; its VALUE must never become a count.
	devicePartition
	// monitoringHandle: an operator bookkeeping resource that is not hardware
	// and must not confer fleet membership on its own.
	monitoringHandle
)

// wholeDeviceKinds names, per vendor domain, the resources whose quantity is a
// physical device count. Anything accelerator-shaped and absent from here is a
// partition, which is the safe direction: a partition contributes existence
// but never a number, and an unknown resource reported as N devices is how one
// Intel card came to be reported as a thousand.
var wholeDeviceKinds = map[string]map[string]struct{}{
	"nvidia.com": {"gpu": {}},
	"amd.com":    {"gpu": {}},
	// Intel names the device family as the kind. Everything else it advertises
	// — millicores, tiles, memory.max — describes ONE card.
	"gpu.intel.com": {"i915": {}, "xe": {}},
	"intel.com":     {"gpu": {}},
	"habana.ai":     {"gaudi": {}, "gaudi2": {}, "gaudi3": {}, "greco": {}, "goya": {}},
	// Inf1/Inf2/Trn1: neuron and neurondevice count devices, neuroncore counts
	// cores within them.
	"aws.amazon.com": {"neuron": {}, "neurondevice": {}},
	// Fractional-GPU scheduling: gpu-count is devices, gpu-mem is MiB.
	"aliyun.com": {"gpu-count": {}},
}

// classifyAcceleratorResource is the one table. Callers ask it what a resource
// means rather than re-deciding per site.
func classifyAcceleratorResource(name corev1.ResourceName) (domain string, class acceleratorResourceClass) {
	resource := string(name)
	// A core resource (cpu, memory, ephemeral-storage) has no domain prefix;
	// only domain-qualified extended resources can name an accelerator.
	domain, kind, qualified := strings.Cut(resource, "/")
	if !qualified {
		return "", notAnAccelerator
	}
	// Bookkeeping first: gpu.intel.com/i915_monitoring is a handle the device
	// plugin advertises for its own exporter. Counted as a device it reported
	// a GPU that does not exist, and — worse — a node advertising ONLY it was
	// admitted to the set of machines playbooks may cordon, drain and reboot.
	if strings.HasSuffix(kind, "_monitoring") {
		return domain, monitoringHandle
	}
	if kinds, ok := wholeDeviceKinds[domain]; ok {
		if _, whole := kinds[kind]; whole {
			return domain, wholeDevice
		}
	}
	switch {
	case kind == "gpu" || strings.HasPrefix(kind, "gpu-") || strings.HasPrefix(kind, "gpu."):
		// A GPU-shaped kind from a domain with no entry above, plus the
		// known partition spellings: nvidia.com/gpu.shared (time-sliced
		// replicas), aliyun.com/gpu-mem (MiB), aliyun.com/gpu-core.percentage.
		return domain, devicePartition
	case strings.HasPrefix(kind, "mig-"):
		// NVIDIA MIG compute instances.
		return domain, devicePartition
	case domain == "gpu.intel.com":
		// millicores, tiles, memory.max: all describe one card.
		return domain, devicePartition
	case domain == "aws.amazon.com" && strings.HasPrefix(kind, "neuron"):
		// neuroncore and friends.
		return domain, devicePartition
	case domain == "habana.ai":
		return domain, devicePartition
	}
	return "", notAnAccelerator
}

// acceleratorResourceKind reports whether a resource is accelerator-shaped at
// all, for callers that only need membership rather than meaning.
func acceleratorResourceKind(name corev1.ResourceName) (domain string, ok bool) {
	domain, class := classifyAcceleratorResource(name)
	return domain, class != notAnAccelerator
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
		domain, class := classifyAcceleratorResource(name)
		switch class {
		case notAnAccelerator:
			continue
		case monitoringHandle:
			// Real hardware always advertises a device resource beside this
			// one, so ignoring it costs nothing; counting it admitted a node
			// with no accelerator at all to the set playbooks may reboot.
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

// cordonOwnersAnnotation records every remediation currently holding this node
// cordoned, as a JSON array of owner names (incident IDs).
//
// A node has many GPUs and an incident is per (target, class), so two of them
// can be remediating two GPUs of one machine at the same time. With only
// cordonReasonAnnotation to go on, the second cordon overwrote the first and the
// first incident's uncordon then released the whole node: the scheduler put
// tenant work onto a machine whose other GPU was about to be reset, and the
// restore ran from whichever snapshot happened to survive, so a human's own
// cordon and their karpenter.sh/do-not-disrupt pin could be wiped by an incident
// that never saw them.
//
// Holding the set on the NODE rather than in the controller is what makes it
// survive a controller restart mid-remediation: the owners are still there when
// the process comes back, and an owner whose incident died without releasing it
// is the cordon janitor's job, exactly as before.
const cordonOwnersAnnotation = "kubeneuron.io/cordon-owners"

// cordonPatchAttempts bounds the compare-and-swap retry on the owner set. Each
// attempt re-reads and recomputes; the race being retried is another
// remediation on the SAME node joining or leaving the set between our read and
// our write, which is rare and self-clearing.
const cordonPatchAttempts = 4

// cordonHeldAnnotation records that a human owns this cordon. It lives on the
// NODE because the incident row does not live forever: retention prunes
// RESOLVED and EXPIRED incidents, and without this an expired incident's
// deliberately-held cordon was released the moment its row was swept, which
// inverts the janitor's whole stated rule.
const cordonHeldAnnotation = "kubeneuron.io/cordon-held"

// cordonHeldOwnersAnnotation records WHICH holds on this node a human has taken
// charge of, as a JSON array of owner names, alongside the owner set itself.
//
// cordonHeldAnnotation above is node-scoped, and on a multi-GPU node that answers
// for remediations it was never about. The held mark is the one thing that stops
// the janitor releasing a hold whose incident row retention has swept, so one
// halted remediation made every OTHER hold on the same machine permanent: the
// abandoned hold beside it could never be dropped, the owner set could never
// empty, and the node-scoped mark blocking it is only cleared when the set DOES
// empty. Eight GPUs deadlock out of the fleet with no incident row left to
// explain it and no notification, since the incident behind the surviving hold
// is gone — the only trace is an Info log line on every reconcile tick.
//
// The node-scoped mark stays for untracked cordons, where there is exactly one
// holder and it cannot be wrong. See platform.CordonedNode.HeldBy.
const cordonHeldOwnersAnnotation = "kubeneuron.io/cordon-held-owners"

// cordonAnnotations is everything this product writes to hold a node cordoned,
// listed once. Every release path removes the whole list: an annotation left
// behind on a node that is back in service makes the next janitor pass read it
// as a cordon we still own, and a held mark left behind makes that cordon
// unreleasable forever. This was three separate literal lists, and the third one
// added is how the held mark came to survive Uncordon.
// cordonHandoffAnnotation is a HUMAN's claim on a node KubeNeuron cordoned.
//
// The restore snapshot answers "what was here before us". Nothing answered
// "what did a person decide after us" — so an engineer who looked at a cordoned
// node at 3am and chose to keep it out of service had no way to say so that we
// could tell apart from our own doing. They set the same unschedulable flag we
// had; the last release compared against the snapshot, concluded the marks were
// ours, and handed the machine back to the scheduler over their decision.
//
// Deliberately a key KubeNeuron NEVER writes. That is the whole point: it is
// the one signal that cannot be confused with our own state, because our state
// can never contain it. The controller reads it and removes it never; only the
// person who set it takes it off.
//
// Not cordon-held. That one means "this incident halted and a human is dealing
// with it", which is a different event — here the incident may be finishing
// perfectly normally and the person has simply decided to keep the node.
// Folding them together would make an incident's completion drop a takeover.
const cordonHandoffAnnotation = "kubeneuron.io/cordon-handoff"

var cordonAnnotations = []string{
	cordonOwnersAnnotation,
	cordonReasonAnnotation,
	cordonRestoreAnnotation,
	cordonHeldAnnotation,
	cordonHeldOwnersAnnotation,
}

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
	return cordonSnapshot(current), nil
}

// cordonSnapshot renders what a later restore must put back for a node nobody
// has recorded yet. An EMPTY result means "we looked, and the node was clean" —
// which is a different statement from the annotation being absent, and priorMarks
// depends on the difference.
func cordonSnapshot(current *corev1.Node) string {
	var parts []string
	if current.Spec.Unschedulable {
		parts = append(parts, "unschedulable")
	}
	if _, pinned := current.Annotations[doNotDisruptAnnotation]; pinned {
		parts = append(parts, "do-not-disrupt")
	}
	return strings.Join(parts, ",")
}

var _ platform.CordonOwnership = (*Platform)(nil)

// CordonForOwner implements platform.CordonOwnership.
//
// The write is a JSON Patch whose first operation TESTS the owner set this call
// read — the same compare-and-swap discipline as UncordonIfReason and
// rewriteTaints, and needed here for the same reason: a plain read-modify-write
// of the annotation drops the entry another remediation on the same node added a
// millisecond earlier, and a dropped owner is a node released while that
// remediation is still resetting a GPU on it. Losing the race is not an error;
// the loser re-reads and re-applies.
//
// A node with no owner set yet but with a cordon reason on it was cordoned by a
// build that predates this, and that build's remediation may still be running.
// Its reason is seeded into the set as a legacy owner rather than being
// overwritten, so the upgrade does not make the older incident invisible.
func (p *Platform) CordonForOwner(ctx context.Context, node, owner, reason string) error {
	if owner == "" {
		// An unnamed owner can never be released by name, so it would hold the
		// node out of the fleet until a human noticed. Refuse to write it.
		return fmt.Errorf("cordoning %s: a cordon owner is required", node)
	}
	var lastErr error
	for range cordonPatchAttempts {
		// The live object, not the informer cache: a stale read here cannot
		// produce a wrong write (the test op rejects it) but does produce a
		// guaranteed retry.
		current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading %s before cordoning it: %w", node, err)
		}
		raw, hasOwners := current.Annotations[cordonOwnersAnnotation]
		owners, err := decodeCordonOwners(raw)
		if err != nil {
			return fmt.Errorf("reading the cordon owners of %s: %w", node, err)
		}
		var seedHeld []string
		if !hasOwners {
			// This node's cordon predates the owner set, so its single holder is
			// named by the reason on it. That holder is seeded into the new set,
			// and — since the janitor may already have decided a human owns it —
			// so is its held mark, which is node-scoped only while the cordon is
			// untracked. Dropping the mark here would let the very next janitor
			// pass release a cordon a human had taken charge of before the
			// upgrade.
			if prior, ours := current.Annotations[cordonReasonAnnotation]; ours {
				priorOwner := owner // a replay of our own cordon: same holder, new name
				if prior != reason {
					priorOwner = platform.LegacyCordonOwner(prior)
					owners = append(owners, priorOwner)
				}
				if _, held := current.Annotations[cordonHeldAnnotation]; held {
					seedHeld = []string{priorOwner}
				}
			}
		}
		if !slices.Contains(owners, owner) {
			owners = append(owners, owner)
		}

		ops := ownerSetGuard(current, raw, hasOwners)
		// Every test op has to come before the first mutation: JSON Patch applies
		// operations in order, so a test placed after the write it is guarding
		// reads back what we just wrote and always passes.
		_, recorded := current.Annotations[cordonRestoreAnnotation]
		if !recorded {
			ops = append(ops, snapshotGuard(current)...)
		}
		ops = append(ops,
			setAnnotationOp(cordonOwnersAnnotation, encodeCordonOwners(owners)),
			setAnnotationOp(cordonReasonAnnotation, reason),
			// See doNotDisruptAnnotation: a cordoned, drained node is the most
			// attractive consolidation target in the cluster, and losing the node
			// mid-remediation orphans the incident.
			setAnnotationOp(doNotDisruptAnnotation, "true"),
			// "add" rather than "replace": spec.unschedulable is omitted from the
			// object entirely when false, and replace on an absent member fails.
			map[string]any{"op": "add", "path": "/spec/unschedulable", "value": true},
		)
		if len(seedHeld) > 0 {
			ops = append(ops, setAnnotationOp(cordonHeldOwnersAnnotation, encodeCordonOwners(seedHeld)))
		}
		if !recorded {
			// Written exactly once, by whoever cordons first, and never touched
			// again. A later owner writing its own snapshot would record OUR
			// cordon and OUR pin as the node's original state, and the last
			// release would then leave the node cordoned and pinned forever.
			ops = append(ops, setAnnotationOp(cordonRestoreAnnotation, cordonSnapshot(current)))
		}

		patch, err := json.Marshal(ops)
		if err != nil {
			return err
		}
		if _, err := p.client.CoreV1().Nodes().Patch(
			ctx, node, k8stypes.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
			if !apierrors.IsInvalid(err) && !apierrors.IsConflict(err) {
				return err
			}
			lastErr = err // somebody else moved the set: re-read and recompute
			continue
		}
		return nil
	}
	return fmt.Errorf("the cordon owners of node %s changed under every attempt to join them: %w", node, lastErr)
}

// ReleaseCordonOwners implements platform.CordonOwnership.
//
// The node is only handed back to the scheduler when the set empties. Every
// other release just shrinks it, under the same compare-and-swap as the join, so
// a concurrent release cannot resurrect an owner that has already left or drop
// one that has just arrived.
func (p *Platform) ReleaseCordonOwners(ctx context.Context, node string, owners []string) (bool, int, error) {
	var lastErr error
	for range cordonPatchAttempts {
		current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			// The node object is gone (a spot reclaim, a node group roll). There
			// is nothing left to return to service and no annotation to clear,
			// and failing the step would only escalate an incident whose hardware
			// no longer exists.
			return false, 0, nil
		}
		if err != nil {
			return false, 0, fmt.Errorf("reading %s before releasing its cordon: %w", node, err)
		}
		raw, hasOwners := current.Annotations[cordonOwnersAnnotation]
		if !hasOwners {
			released, err := p.releaseUntrackedCordon(ctx, node, current, owners)
			return released, 0, err
		}
		held, err := decodeCordonOwners(raw)
		if err != nil {
			return false, 0, fmt.Errorf("reading the cordon owners of %s: %w", node, err)
		}
		remaining := slices.DeleteFunc(slices.Clone(held), func(o string) bool {
			return slices.Contains(owners, o)
		})
		if len(remaining) == len(held) {
			// None of these owners is on the node. A replayed step, a retried
			// janitor pass, or a release that already landed — all of them are
			// nothing to do, and none of them is a failure.
			return false, len(remaining), nil
		}

		ops := []map[string]any{
			annotationTestOp(cordonOwnersAnnotation, raw, true),
			// The held-owner record has to be pinned as well, and pinning the
			// owner set does not do it. A janitor pass recording that a human owns
			// SOME OTHER hold on this node writes that record and leaves the owner
			// set untouched, so the test above still passes — and both branches
			// below then rewrite the record from the value this call read a
			// moment earlier. The shrink removes a verdict that arrived since; the
			// restore leaves one behind on a node it is handing back.
			//
			// A discarded verdict does not hurt in the window. It hurts weeks
			// later, when retention prunes the halted incident's row: the janitor
			// finds a hold nothing explains, releases it, and the last release
			// puts a machine back into the schedulable pool with the GPU an
			// engineer deliberately withdrew still in it. Losing this test costs a
			// re-read.
			heldOwnerSetGuard(current.Annotations[cordonHeldOwnersAnnotation], current.Annotations),
		}
		if len(remaining) > 0 {
			// Somebody else is still remediating this machine. Shrink the set and
			// leave everything else exactly as it is: the node stays
			// unschedulable, stays pinned against the autoscaler, and keeps the
			// snapshot the first owner took.
			ops = append(ops, setAnnotationOp(cordonOwnersAnnotation, encodeCordonOwners(remaining)))
			dropped, err := p.dropHeldOwnersOps(node, current, owners)
			if err != nil {
				return false, 0, err
			}
			ops = append(ops, dropped...)
		} else {
			ops = append(ops, p.restoreOps(node, current)...)
		}
		patch, err := json.Marshal(ops)
		if err != nil {
			return false, 0, err
		}
		if _, err := p.client.CoreV1().Nodes().Patch(
			ctx, node, k8stypes.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
			if !apierrors.IsInvalid(err) && !apierrors.IsConflict(err) {
				return false, 0, err
			}
			lastErr = err // the set moved under us: re-read and recompute
			continue
		}
		return len(remaining) == 0, len(remaining), nil
	}
	return false, 0, fmt.Errorf("the cordon owners of node %s changed under every attempt to leave them: %w", node, lastErr)
}

// releaseUntrackedCordon handles the node that carries a cordon reason but no
// owner set: one this build's predecessor cordoned, still held across the
// upgrade.
//
// The old build's rule was "one reason, one owner", so a matching reason means
// the caller IS the sole owner and the node can be put back. A reason that does
// not match belongs to somebody else and is left alone — the same fail-closed
// answer UncordonIfReason gives, which is where the actual write happens so the
// live re-check and the restore rules stay in one place.
func (p *Platform) releaseUntrackedCordon(ctx context.Context, node string, current *corev1.Node, owners []string) (bool, error) {
	reason, ours := current.Annotations[cordonReasonAnnotation]
	if !ours {
		return false, nil // nothing of ours on this node at all
	}
	for _, owner := range owners {
		if owner == platform.LegacyCordonOwner(reason) {
			// A takeover binds here as well. This path serves a node cordoned
			// by a build that predates owner sets, and it reached the release
			// through a different door — so it checked the reason and the
			// counted state and nothing else. An operator who annotated an
			// upgrade-era cordon to keep a suspect node down would have watched
			// the finishing incident hand it back anyway, which is the exact
			// harm the annotation was added to prevent.
			if _, handedOff := current.Annotations[cordonHandoffAnnotation]; handedOff {
				slog.Warn("an upgrade-era cordon was taken over by hand; leaving the node down "+
					"and clearing only KubeNeuron's own annotations",
					"node", node, "annotation", cordonHandoffAnnotation)
				return false, p.clearOurAnnotations(ctx, node, current)
			}
			return p.UncordonIfReason(ctx, node, reason)
		}
	}
	return false, nil
}

// dropHeldOwnersOps removes the departing holds from the record of which holds a
// human owns, so the record shrinks with the owner set it describes.
//
// A verdict that outlives the hold it was about is exactly as bad as one written
// about the wrong hold: it is the thing that stops the janitor releasing a hold
// whose incident row has been pruned, so a stale entry keeps a machine out of
// the fleet for good. Its own compare-and-swap comes from the owner-set test op
// this is appended to — the two annotations only ever move together here.
func (p *Platform) dropHeldOwnersOps(node string, current *corev1.Node, leaving []string) ([]map[string]any, error) {
	rawHeld, present := current.Annotations[cordonHeldOwnersAnnotation]
	if !present {
		return nil, nil
	}
	held, err := decodeCordonOwners(rawHeld)
	if err != nil {
		return nil, fmt.Errorf("reading the held cordon owners of %s: %w", node, err)
	}
	kept := slices.DeleteFunc(slices.Clone(held), func(o string) bool { return slices.Contains(leaving, o) })
	if len(kept) == len(held) {
		return nil, nil
	}
	if len(kept) == 0 {
		// Removed rather than written empty. An empty set left behind is another
		// annotation the next release has to reason about, and "present but says
		// nothing" is the shape that keeps producing these defects.
		return []map[string]any{{
			"op": "remove", "path": "/metadata/annotations/" + escapeJSONPointer(cordonHeldOwnersAnnotation),
		}}, nil
	}
	return []map[string]any{setAnnotationOp(cordonHeldOwnersAnnotation, encodeCordonOwners(kept))}, nil
}

// restoreOps builds the patch operations that put a node back the way its first
// owner found it. Only ever appended after a test op that pins the owner set.
func (p *Platform) restoreOps(node string, current *corev1.Node) []map[string]any {
	// A human has taken this node over: strip our bookkeeping and leave the
	// machine exactly as they are holding it.
	//
	// The handoff forbids only one thing — returning the node to the scheduler.
	// Our annotations still come off, because a node that reads as
	// KubeNeuron-cordoned with no live incident collects stuck-cordon reports
	// nobody can act on.
	//
	// spec.unschedulable and the Karpenter pin are left ALONE, not restored
	// from the snapshot. Restoring would undo the takeover, and dropping the
	// pin while the node stays cordoned would expose a machine somebody is
	// working on to consolidation — we are also about to delete the record that
	// says whether the pin was ours, so leaving it is the only answer we can
	// still justify afterwards.
	if _, handedOff := current.Annotations[cordonHandoffAnnotation]; handedOff {
		ops := []map[string]any{
			// The takeover must still be there when the write lands. If the
			// person removed it in the meantime this op fails, the attempt is
			// retried, and the retry takes the ordinary restore below.
			annotationTestOp(cordonHandoffAnnotation, current.Annotations[cordonHandoffAnnotation], true),
		}
		ops = append(ops, removeOurAnnotationOps(current)...)
		slog.Warn("node was taken over by hand during remediation; leaving it cordoned and "+
			"removing only KubeNeuron's own annotations. It will not return to the fleet until "+
			"somebody removes the takeover annotation",
			"node", node, "annotation", cordonHandoffAnnotation,
			"value", current.Annotations[cordonHandoffAnnotation])
		return ops
	}

	keepUnschedulable, keepPinned, known := priorMarks(current.Annotations)
	if !known {
		unknownRestoreRecord(node)
	}
	// "add", not "replace": a human may have uncordoned this node by hand while
	// the remediation ran, and spec.unschedulable is then absent from the object
	// entirely — a replace would 422 on every attempt and leave our annotations
	// on the node forever.
	ops := []map[string]any{
		// No takeover as we write, either. Without this the window between the
		// read above and the patch is exactly long enough for somebody to claim
		// the node and have us hand it back anyway — a GET, a check and a PATCH
		// is not a check at all here.
		annotationTestOp(cordonHandoffAnnotation, "", false),
		{"op": "add", "path": "/spec/unschedulable", "value": keepUnschedulable},
	}
	// Everything we wrote comes off together. Leaving any of it behind makes the
	// node look cordoned by KubeNeuron to the next janitor pass, which is how a
	// node that is back in service acquires a stuck-cordon report nobody can act
	// on.
	ops = append(ops, removeOurAnnotationOps(current)...)
	if !keepPinned {
		if _, present := current.Annotations[doNotDisruptAnnotation]; present {
			ops = append(ops, map[string]any{
				"op": "remove", "path": "/metadata/annotations/" + escapeJSONPointer(doNotDisruptAnnotation),
			})
		}
	}
	return ops
}

// removeOurAnnotationOps strips every annotation KubeNeuron wrote, and only
// those. Shared by both restore paths so a sixth annotation cannot be added to
// cordonAnnotations and then be left behind by one of them.
func removeOurAnnotationOps(current *corev1.Node) []map[string]any {
	var ops []map[string]any
	for _, ann := range cordonAnnotations {
		if _, present := current.Annotations[ann]; present {
			ops = append(ops, map[string]any{
				"op": "remove", "path": "/metadata/annotations/" + escapeJSONPointer(ann),
			})
		}
	}
	return ops
}

// clearOurAnnotations removes KubeNeuron's own cordon bookkeeping and touches
// nothing else — the node's unschedulable flag and any Karpenter pin stay as
// the person holding it left them. Used when a takeover forbids the restore but
// the node must not keep looking KubeNeuron-cordoned to the next janitor pass.
func (p *Platform) clearOurAnnotations(ctx context.Context, node string, current *corev1.Node) error {
	ops := append([]map[string]any{
		annotationTestOp(cordonHandoffAnnotation, current.Annotations[cordonHandoffAnnotation], true),
	}, removeOurAnnotationOps(current)...)
	if len(ops) == 1 {
		return nil // nothing of ours is on the node
	}
	patch, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	_, err = p.client.CoreV1().Nodes().Patch(ctx, node, k8stypes.JSONPatchType, patch, metav1.PatchOptions{})
	if apierrors.IsInvalid(err) || apierrors.IsConflict(err) {
		return nil // the takeover moved under us; the next pass re-reads
	}
	return err
}

// MarkCordonHeldIfOwner implements platform.CordonOwnership.
//
// The mark names the HOLD, not the node. A node-scoped mark answers "a human
// owns this cordon" for every other remediation on the same machine, and since
// that answer is what stops the janitor releasing a hold whose incident row has
// been pruned, one halted remediation made every abandoned hold beside it
// permanent — the owner set could never empty, and the mark is only cleared when
// it does.
func (p *Platform) MarkCordonHeldIfOwner(ctx context.Context, node, owner string) (bool, error) {
	current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s before marking its cordon held: %w", node, err)
	}
	raw, hasOwners := current.Annotations[cordonOwnersAnnotation]
	if !hasOwners {
		return false, nil // untracked cordon; MarkCordonHeldIfReason owns that case
	}
	owners, err := decodeCordonOwners(raw)
	if err != nil {
		return false, fmt.Errorf("reading the cordon owners of %s: %w", node, err)
	}
	if !slices.Contains(owners, owner) {
		return false, nil // this owner has left since we listed it; not ours to mark
	}
	rawHeld := current.Annotations[cordonHeldOwnersAnnotation]
	held, err := decodeCordonOwners(rawHeld)
	if err != nil {
		return false, fmt.Errorf("reading the held cordon owners of %s: %w", node, err)
	}
	if slices.Contains(held, owner) {
		return true, nil // already recorded; nothing to write
	}
	ops := []map[string]any{{
		"op":    "test",
		"path":  "/metadata/annotations/" + escapeJSONPointer(cordonOwnersAnnotation),
		"value": raw,
	},
		// The held set moves independently of the owner set — two janitor passes
		// can reach a verdict about two different holds on one node without the
		// owner set changing at all — so a write that tested only the owner set
		// would drop the other verdict. A dropped verdict returns a node a human
		// took charge of to the scheduler once its incident row is pruned.
		heldOwnerSetGuard(rawHeld, current.Annotations),
		setAnnotationOp(cordonHeldOwnersAnnotation, encodeCordonOwners(append(held, owner))),
		// The node-scoped mark is kept in step for anything still reading it, and
		// it is what an operator greps for.
		setAnnotationOp(cordonHeldAnnotation, "true"),
	}
	patch, _ := json.Marshal(ops)
	if _, err := p.client.CoreV1().Nodes().Patch(
		ctx, node, k8stypes.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsConflict(err) {
			return false, nil // the test op lost the race; the next pass re-decides
		}
		return false, err
	}
	return true, nil
}

// ownerSetGuard returns the compare-and-swap prefix for a write to the owner
// set: the value we read must still be there when the patch lands.
//
// A node with no annotations at all needs the map created first, and a `test`
// against a member of a map that does not exist fails as MISSING rather than as
// a failed test — an error the retry loop cannot clear. Testing the map itself
// for null covers both the creation and the race.
//
// Keyed on the map being EMPTY rather than nil, because the two are the same
// thing on the wire: ObjectMeta.Annotations is omitempty, so an empty map is
// absent from the JSON the patch is applied to even when the decoded Go object
// carries one.
func ownerSetGuard(current *corev1.Node, raw string, hasOwners bool) []map[string]any {
	if len(current.Annotations) == 0 {
		return []map[string]any{
			{"op": "test", "path": "/metadata/annotations", "value": nil},
			{"op": "add", "path": "/metadata/annotations", "value": map[string]string{}},
		}
	}
	return []map[string]any{annotationTestOp(cordonOwnersAnnotation, raw, hasOwners)}
}

// countedCordon reports whether a node's cordon is reference-counted — held by a
// SET of remediations recorded in cordonOwnersAnnotation, rather than by the
// single holder named in its reason.
//
// One predicate, because every path that acts on the strength of the reason
// alone has to ask the same question, and each of them is only safe on an
// uncounted cordon. UncordonIfReason and Uncordon would hand back a machine
// whose other owners are still resetting GPUs on it. MarkCordonHeldIfReason
// would write a node-scoped human verdict that CordonedNode.HeldBy will not read
// on a counted cordon, so the verdict silently evaporates and the janitor
// releases the hold as soon as retention prunes its incident row. Spelling the
// test inline at each site is exactly how the third one came to be written
// without it.
func countedCordon(annotations map[string]string) bool {
	_, counted := annotations[cordonOwnersAnnotation]
	return counted
}

// heldOwnerSetGuard pins the record of which holds a human owns, so a write that
// adds one verdict cannot silently discard another that landed since the read.
// Only valid on a node that already has annotations — every caller has just read
// the owner set off it.
func heldOwnerSetGuard(raw string, annotations map[string]string) map[string]any {
	_, present := annotations[cordonHeldOwnersAnnotation]
	return annotationTestOp(cordonHeldOwnersAnnotation, raw, present)
}

// annotationTestOp is the compare-and-swap op for one annotation: it asserts the
// key still holds the value that was read, or is still absent.
//
// Absence is expressed as a null value rather than by omitting the op, because
// "there was nothing here" is a real thing to assert — the whole point of a
// join is that it must not overwrite a set somebody created a millisecond
// earlier. A `test` against a member of a map that does not EXIST fails as
// missing rather than as a failed test, which the retry loop cannot clear, so
// this must not be used on a node with no annotations at all; see ownerSetGuard.
func annotationTestOp(key, value string, present bool) map[string]any {
	var want any // null tests that the annotation is still ABSENT
	if present {
		want = value
	}
	return map[string]any{
		"op": "test", "path": "/metadata/annotations/" + escapeJSONPointer(key), "value": want,
	}
}

// snapshotGuard pins the two marks the restore record is a statement ABOUT, for
// the one patch that writes it.
//
// The record says what the node looked like before this product touched it, and
// it is decided from an object read some milliseconds earlier. Neither thing it
// describes is the owner-set annotation the rest of the patch tests: `kubectl
// cordon` writes spec.unschedulable and touches nothing else, and pinning a node
// writes karpenter.sh/do-not-disrupt. Either can land in that window while the
// owner set sits perfectly still, and the record then says "the node was clean"
// over the top of a mark an engineer had just placed deliberately.
//
// The bill arrives at the LAST release, hours later and with no connection to
// the window: a node a human took out of service is handed back to the
// scheduler, or a pin somebody put on to keep a week-long training run away from
// the autoscaler is lifted. Losing this test just means a re-read.
func snapshotGuard(current *corev1.Node) []map[string]any {
	var unschedulable any // null tests that the member is still ABSENT, i.e. false
	if current.Spec.Unschedulable {
		unschedulable = true
	}
	pin, pinned := current.Annotations[doNotDisruptAnnotation]
	return []map[string]any{
		{"op": "test", "path": "/spec/unschedulable", "value": unschedulable},
		annotationTestOp(doNotDisruptAnnotation, pin, pinned),
	}
}

// setAnnotationOp writes one annotation. "add" replaces an existing member
// (RFC 6902), so it works whether or not the key is already there.
func setAnnotationOp(key, value string) map[string]any {
	return map[string]any{
		"op": "add", "path": "/metadata/annotations/" + escapeJSONPointer(key), "value": value,
	}
}

// encodeCordonOwners renders the owner set: sorted and de-duplicated, so the
// same set always produces the same annotation value and the compare-and-swap
// tests above cannot fail on ordering alone. JSON, not a comma-separated list,
// because a legacy owner carries a whole reason string inside it and reasons are
// written by humans.
func encodeCordonOwners(owners []string) string {
	sorted := slices.Clone(owners)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	if len(sorted) == 0 {
		// json.Marshal of a nil slice is the literal `null`, and every reader here
		// decodes that back to an empty set without complaint — so an owner set
		// that emptied by this route would sit on the node saying "present, and
		// nobody holds it". The join and release paths both refuse to touch a node
		// whose owner annotation is PRESENT, so that node could never be released
		// by anything again. No caller writes an empty set today; this makes sure
		// one that starts to cannot create that state.
		return "[]"
	}
	encoded, err := json.Marshal(sorted)
	if err != nil { // unreachable for []string
		return "[]"
	}
	return string(encoded)
}

// decodeCordonOwners reads the owner set. An absent or empty annotation is an
// empty set; anything present that does not parse is an ERROR rather than an
// empty set, because treating a hand-edited annotation as "nobody holds this
// node" would release a machine that may be mid-reset.
func decodeCordonOwners(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var owners []string
	if err := json.Unmarshal([]byte(raw), &owners); err != nil {
		return nil, fmt.Errorf("annotation %s is not a JSON array of owners (%q): %w", cordonOwnersAnnotation, raw, err)
	}
	return owners, nil
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
			return collectCordonedNodes(cached), nil
		}
	}
	nodes, err := p.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	live := make([]*corev1.Node, len(nodes.Items))
	for i := range nodes.Items {
		live[i] = &nodes.Items[i]
	}
	return collectCordonedNodes(live), nil
}

// collectCordonedNodes renders one whole listing, and publishes how much of the
// fleet this pass had to give up on.
//
// The count is set here, over the complete listing, rather than incremented
// where the giving-up happens: it is a statement about the fleet right now, so
// it has to fall back to zero on its own when somebody fixes the annotation, and
// it has to survive a controller restart without being replayed. Both paths in
// CordonedNodes go through this so the two cannot report different fleets.
func collectCordonedNodes(nodes []*corev1.Node) []platform.CordonedNode {
	var out []platform.CordonedNode
	unreadable := 0
	for _, node := range nodes {
		cordoned, ours, err := cordonedNodeFrom(node)
		switch {
		case err != nil:
			// Left cordoned on purpose, but held out of the fleet by nothing that
			// can be asked to let go — no owner, no incident row, and a log line
			// that is printed once and never again. Somebody has to be able to see
			// that the machine is gone.
			unreadable++
		case ours:
			out = append(out, cordoned)
		}
	}
	metrics.CordonsUnreadable.Set(float64(unreadable))
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// corruptOwnerSets remembers which unreadable owner sets have been reported, so
// a hand-edited annotation is not warned about on every reconcile tick until
// people stop reading the log.
var corruptOwnerSets sync.Map

// cordonedNodeFrom decides whether one node carries a cordon this product placed
// and renders it for the janitor.
//
// The three answers are deliberately distinct. Not ours, ours, and OURS BUT
// UNREADABLE — the last is a machine held out of the fleet that nothing will
// ever release, and collapsing it into "not ours" is what made that state
// invisible. It is the only one of the three anybody needs to be told about, so
// it is the only one that is an error.
func cordonedNodeFrom(node *corev1.Node) (platform.CordonedNode, bool, error) {
	reason, hasReason := node.Annotations[cordonReasonAnnotation]
	raw, hasOwners := node.Annotations[cordonOwnersAnnotation]
	if (!hasReason && !hasOwners) || !node.Spec.Unschedulable {
		return platform.CordonedNode{}, false, nil
	}
	owners, err := decodeCordonOwners(raw)
	if err != nil {
		// Fail closed: reporting the node with no owners would send the janitor
		// down the single-owner path and release a machine that several
		// remediations may be holding. Leaving it out keeps it cordoned, which is
		// the recoverable direction, and says so once.
		reportUnreadableCordonSet(node.Name, cordonOwnersAnnotation, raw, err)
		return platform.CordonedNode{}, false, err
	}
	rawHeld := node.Annotations[cordonHeldOwnersAnnotation]
	heldOwners, err := decodeCordonOwners(rawHeld)
	if err != nil {
		// Same direction, and for a sharper reason: this annotation is the record
		// of which holds a HUMAN has taken charge of. Reporting the node without
		// it tells the janitor nobody owns any of them, and the next pass hands
		// back a machine somebody deliberately took out of service.
		reportUnreadableCordonSet(node.Name, cordonHeldOwnersAnnotation, rawHeld, err)
		return platform.CordonedNode{}, false, err
	}
	_, held := node.Annotations[cordonHeldAnnotation]
	return platform.CordonedNode{
		Name: node.Name, Reason: reason, Owners: owners, Held: held, HeldOwners: heldOwners,
	}, true, nil
}

// reportUnreadableCordonSet says once that a cordon annotation cannot be parsed
// and the node is therefore being left alone.
func reportUnreadableCordonSet(node, annotation, value string, err error) {
	if _, warned := corruptOwnerSets.LoadOrStore(node+"\x00"+annotation+"\x00"+value, struct{}{}); warned {
		return
	}
	slog.Warn("a node's cordon owner annotation cannot be read, so it is left cordoned and "+
		"out of the janitor's reach; fix or remove the annotation by hand",
		"node", node, "annotation", annotation, "value", value, "err", err)
}

// UncordonIfReason implements platform.CordonJanitor.
//
// The reason is tested against the LIVE object inside the same patch, so a
// cordon that has been replaced since the janitor listed it is not released by
// a decision made about its predecessor. A JSON Patch `test` op is the only way
// to say that without a read-modify-write race, and it stays on the `patch`
// verb this controller already holds.
func (p *Platform) UncordonIfReason(ctx context.Context, node, expectedReason string) (bool, error) {
	current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("reading %s before releasing its cordon: %w", node, err)
	}
	if current.Annotations[cordonReasonAnnotation] != expectedReason {
		return false, nil // replaced since we listed it; not ours to release
	}
	if countedCordon(current.Annotations) {
		// This node's cordon is reference-counted and the reason belongs to
		// whichever remediation cordoned LAST. Releasing on the reason alone
		// would put the machine back into service while the other owners are
		// still resetting GPUs on it, which is the defect the owner set exists to
		// close. ReleaseCordonOwners is the only way out of a shared cordon.
		return false, nil
	}
	keepUnschedulable, keepPinned, known := priorMarks(current.Annotations)
	if !known {
		unknownRestoreRecord(node)
	}

	ops := []map[string]any{{
		"op":    "test",
		"path":  "/metadata/annotations/" + escapeJSONPointer(cordonReasonAnnotation),
		"value": expectedReason,
	}, {
		// "add", not "replace": spec.unschedulable is omitted entirely when
		// false, so a human uncordoning the node by hand while the remediation
		// ran would make a replace fail forever and strand our annotations on it.
		"op": "add", "path": "/spec/unschedulable", "value": keepUnschedulable,
	}, {
		"op": "remove", "path": "/metadata/annotations/" + escapeJSONPointer(cordonReasonAnnotation),
	}}
	for _, ann := range cordonAnnotations {
		if ann == cordonReasonAnnotation {
			continue // already removed unconditionally above
		}
		if _, present := current.Annotations[ann]; present {
			ops = append(ops, map[string]any{
				"op": "remove", "path": "/metadata/annotations/" + escapeJSONPointer(ann),
			})
		}
	}
	if !keepPinned {
		if _, present := current.Annotations[doNotDisruptAnnotation]; present {
			ops = append(ops, map[string]any{
				"op": "remove", "path": "/metadata/annotations/" + escapeJSONPointer(doNotDisruptAnnotation),
			})
		}
	}
	patch, _ := json.Marshal(ops)
	if _, err := p.client.CoreV1().Nodes().Patch(ctx, node, k8stypes.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		if apierrors.IsInvalid(err) {
			// The test op failed: the reason changed between the Get and the
			// Patch. Not an error — the next pass sees the new one.
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// MarkCordonHeldIfReason implements platform.CordonJanitor.
//
// Same live re-check and same JSON-Patch test op as UncordonIfReason, and for
// the same reason: both act on a listing served from the informer cache, where
// a stale entry means the cordon has been REPLACED rather than missed. This one
// went without the guard for a round, which was the more dangerous omission of
// the two — the held mark is designed to outlive the incident row, so stamping
// it onto the wrong live incident's cordon leaves a GPU node out of the fleet
// permanently once that incident is pruned, on the strength of a decision made
// about a different incident.
func (p *Platform) MarkCordonHeldIfReason(ctx context.Context, node, expectedReason string) (bool, error) {
	current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s before marking its cordon held: %w", node, err)
	}
	if current.Annotations[cordonReasonAnnotation] != expectedReason {
		return false, nil // replaced since we listed it; not ours to mark
	}
	if countedCordon(current.Annotations) {
		// The mark this writes is NODE-scoped, and on a reference-counted cordon
		// CordonedNode.HeldBy deliberately refuses to read it — a node-scoped
		// verdict answers "a human owns this" for every other remediation on the
		// machine and strands their holds forever. So a verdict written here onto
		// a counted cordon is invisible to everything that acts on it: recorded,
		// and worth nothing. The janitor then releases the hold the moment
		// retention prunes its incident row, handing back a machine somebody took
		// out of service deliberately.
		//
		// This is reachable, not defensive. The janitor chooses between this and
		// MarkCordonHeldIfOwner from an informer listing, so a node listed before
		// its first owner joined is counted by the time the write lands. Refusing
		// costs one reconcile tick: the next listing shows the owner set and the
		// verdict is recorded against the hold it was reached about.
		return false, nil
	}
	ops := []map[string]any{{
		"op":    "test",
		"path":  "/metadata/annotations/" + escapeJSONPointer(cordonReasonAnnotation),
		"value": expectedReason,
	}, {
		"op":    "add",
		"path":  "/metadata/annotations/" + escapeJSONPointer(cordonHeldAnnotation),
		"value": "true",
	}}
	patch, _ := json.Marshal(ops)
	if _, err := p.client.CoreV1().Nodes().Patch(
		ctx, node, k8stypes.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsConflict(err) {
			return false, nil // the test op lost the race
		}
		return false, err
	}
	return true, nil
}

// escapeJSONPointer escapes a map key for use in a JSON Pointer path.
func escapeJSONPointer(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

// Uncordon makes the node schedulable again, clears the reason, and lifts the
// autoscaler protection the cordon put in place.
// priorMarks decodes the cordon-restore record into what Uncordon must put
// back, and says whether the record existed at all.
//
// The distinction is load-bearing and the first version did not make it. An
// EMPTY record means "we looked, and the node was clean"; an ABSENT one means
// "we do not know" — a node cordoned by a build that predates the record and
// still held across the upgrade, or an annotation a human edited. Splitting a
// missing key yields [""] for both, which resolves the unknown case in the
// fail-open direction: it deletes a karpenter.sh/do-not-disrupt somebody else
// placed, which is the exact harm the record was added to prevent.
//
// Unknown therefore restores nothing and clears nothing: the node keeps
// whatever it has, and a human can see it.
//
// That claim used to be false for half of it. Both callers discarded this
// return and substituted (false, true), so the pin was preserved — the half
// the change was written for — while the CORDON was cleared. The most common
// way a node is already cordoned when a remediation opens on it is that an
// engineer ran kubectl cordon, so the fail-open half destroyed exactly the
// deliberate act Cordon's own comment names as the reason this record exists,
// and put tenant work back on a machine somebody took out of service.
//
// The cost of the honest answer is real and worth stating: on upgrade from a
// build that predates the record, a node WE cordoned now stays cordoned until
// somebody uncordons it. That is stranded capacity. But it is stranded in
// plain sight — our annotations are stripped, so the node looks exactly like
// one a human cordoned, which is the state it may well be in — whereas the
// other direction silently returns a machine to service against somebody's
// explicit decision, and nothing in the audit trail says who did it.
func priorMarks(annotations map[string]string) (keepUnschedulable, keepPinned, known bool) {
	record, present := annotations[cordonRestoreAnnotation]
	if !present {
		return true, true, false
	}
	parts := strings.Split(record, ",")
	return slices.Contains(parts, "unschedulable"), slices.Contains(parts, "do-not-disrupt"), true
}

// unknownRestoreRecord logs the one case priorMarks cannot resolve. Uncordon
// runs below the controller, with no notifier, so this is the loudest signal
// available here; the node itself carries the other half of the message by
// keeping its cordon and losing our annotations.
func unknownRestoreRecord(node string) {
	slog.Warn("node has a KubeNeuron cordon reason but no restore record, so we cannot tell "+
		"whether we cordoned it or found it cordoned; leaving it cordoned and removing our "+
		"annotations — uncordon it by hand if it should return to service",
		"node", node)
}

func (p *Platform) Uncordon(ctx context.Context, node string) error {
	// Restore, do not clear. See Cordon: either mark may have been somebody
	// else's before we touched the node, and the record says which.
	current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading %s before uncordoning it: %w", node, err)
	}
	if countedCordon(current.Annotations) {
		// This node is held by a counted set of remediations and this call
		// carries no owner, so it cannot know whether it is the last one out.
		// Releasing anyway returns a machine to the scheduler while another
		// incident is still resetting a GPU on it. Callers that know who they are
		// use ReleaseCordonOwners; this one does nothing and says so.
		slog.Warn("refusing an unowned uncordon of a node several remediations hold; "+
			"release it through the owner set instead",
			"node", node, "owners", current.Annotations[cordonOwnersAnnotation])
		return nil
	}
	keepUnschedulable, keepPinned, known := priorMarks(current.Annotations)
	if !known {
		unknownRestoreRecord(node)
	}

	// The held marks go too. They deliberately OUTLIVE the incident row, so one
	// left behind on a node that is back in service is read by the next janitor
	// pass as "a human owns this cordon" — and the next incident to cordon this
	// machine can then never have its cordon cleaned up.
	annotations := map[string]*string{}
	for _, ann := range cordonAnnotations {
		annotations[ann] = nil
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
	// PRE-FLIGHT: refuse before evicting anything.
	//
	// A pod with no controller is not evictable and nothing will reschedule it,
	// so a node carrying one cannot be drained — and kubectl aborts on exactly
	// this, before issuing a single eviction, which is the entire point of
	// --force. The first version of this refusal collected the names inside the
	// eviction loop and returned at the end: it evicted every managed pod,
	// waited for them to die, and only then failed. The ladder then escalated
	// through every remaining rung, evicting the customer's jobs again at each
	// one, for a node it could never drain — while the pod that caused it (an
	// SRE's `kubectl debug node/...`, which creates a bare pod) sat there
	// untouched. Doing the disruption and then refusing is worse than either.
	//
	// Computed unconditionally, not only when refusing. These are the pods
	// nothing will recreate, so with force set they are the ones this drain
	// destroys outright — and that was the one case where their names were
	// never collected, logged or returned. The step recorded "drained node-x",
	// no metric moved, and the only trace of a researcher's work was a
	// Kubernetes Event on a pod that no longer exists.
	var unmanaged []string
	for i := range pods {
		if unmanagedPod(&pods[i]) {
			unmanaged = append(unmanaged, pods[i].Namespace+"/"+pods[i].Name)
		}
	}
	sort.Strings(unmanaged)
	if len(unmanaged) > 0 {
		if !opts.Force {
			return fmt.Errorf("drain of %s: %d pod(s) have no controller and cannot be evicted (%s); "+
				"nothing would reschedule them, so this node cannot be drained — nothing was evicted",
				node, len(unmanaged), strings.Join(unmanaged, ", "))
		}
		slog.Warn("forced drain is destroying pods that nothing will recreate",
			"node", node, "count", len(unmanaged), "pods", strings.Join(unmanaged, ", "))
		metrics.ForcedUnmanagedEvictions.Add(float64(len(unmanaged)))
	}

	// kubectl drain has TWO abort conditions, and this had only ever
	// implemented one. The second is local data: a pod writing to an emptyDir
	// loses that directory when it is evicted, which is why kubectl makes you
	// type --delete-emptydir-data separately from --force.
	//
	// Not a refusal, deliberately. Nearly every serious GPU workload mounts an
	// emptyDir for /dev/shm — PyTorch's dataloaders require it — so refusing
	// would make almost every GPU node in a real fleet undrainable, which is
	// the opposite of useful. The pod is also managed and does reschedule; it
	// is only the DATA that does not follow it.
	//
	// So: say what is being destroyed, on the way past. A training job
	// checkpointing into a scratch volume loses those checkpoints here, and
	// until now nothing anywhere recorded that it had happened.
	if withData := podsWithLocalData(pods); len(withData) > 0 {
		slog.Warn("drain is evicting pods with local scratch data; the pods reschedule, the "+
			"data does not",
			"node", node, "count", len(withData), "pods", strings.Join(withData, ", "))
	}

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
	if err := p.waitDrained(ctx, node, opts, time.NewTicker(drainPollInterval)); err != nil {
		return err
	}

	return nil
}

// evictPod issues one Eviction API call for the pod.
// evictionGrace decides whether to override the pod's own grace period, and
// with what. It returns capped=false to mean "leave the pod's spec alone".
func evictionGrace(ctx context.Context, pod *corev1.Pod, opts platform.DrainOptions) (time.Duration, bool) {
	if opts.GracePeriod > 0 {
		return opts.GracePeriod, true // an explicit caller override
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false // unbounded step: the pod's own period governs
	}
	budget := time.Until(deadline) - evictionGraceMargin
	if budget < 0 {
		budget = 0
	}
	own := time.Duration(0)
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		own = time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second
	} else {
		own = corev1DefaultGracePeriod
	}
	if own <= budget {
		return 0, false // it fits; do not touch the spec
	}
	// Below the platform's own default this stops being a clamp and becomes a
	// force-delete. GracePeriodSeconds: 0 removes the object from etcd at once
	// and the kubelet SIGKILLs with no SIGTERM — the single most destructive
	// eviction there is, and the one platform.DrainUsePodGracePeriod's contract
	// says nothing in this product wants. The clamp reached it by arithmetic:
	// the PDB retry loop recomputes this on every 5s attempt, so as the step
	// deadline approaches the budget decays through 4s, 1s, 0s and the tail of
	// every contended drain force-deleted.
	//
	// That is worse than failing. A tenant who set terminationGracePeriodSeconds
	// specifically to checkpoint before a GPU reset loses the run without a
	// signal, and waitDrained then sees no pods, reports the node drained, and
	// lets the ladder proceed to a reset while the process may still hold
	// /dev/nvidia*. A drain that times out escalates visibly; this one succeeds
	// and lies.
	//
	// So: if what remains cannot cover even the default, decline to override.
	// The pod keeps its own period, the drain overruns its step, and the step
	// fails where somebody can see it.
	if budget < corev1DefaultGracePeriod {
		return 0, false
	}
	return budget, true
}

// evictionGraceMargin is the slack left between an eviction's grace period and
// the step deadline, so the kubelet's SIGKILL and the pod's disappearance both
// land inside the window rather than the drain timing out mid-termination.
const evictionGraceMargin = 30 * time.Second

// corev1DefaultGracePeriod is Kubernetes' own default when a pod declares none.
const corev1DefaultGracePeriod = 30 * time.Second

func (p *Platform) evictPod(ctx context.Context, pod *corev1.Pod, opts platform.DrainOptions) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
	}
	// Clamp to what is left of the step, rather than choosing a number.
	//
	// terminationGracePeriodSeconds is tenant-writable and unbounded. Honouring
	// it unconditionally — the right instinct, and the point of the previous
	// change — hands the drain's duration to whoever wrote the pod: a spec
	// declaring 24h cannot terminate inside a 30-minute step, so the drain times
	// out, the step fails, and the incident climbs the whole ladder to
	// NEEDS_HUMAN having repaired nothing. A tenant should not be able to deny
	// remediation of the node they are on.
	//
	// So the pod keeps its own grace period whenever it fits, and is cut only
	// by the deadline the step already had — which is the bound that existed
	// anyway. The margin leaves room for the kubelet to act on the SIGKILL
	// before the context dies, so the node is actually drained rather than
	// abandoned mid-termination.
	if grace, capped := evictionGrace(ctx, pod, opts); capped {
		secs := int64(grace / time.Second)
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
// podsWithLocalData names the pods whose on-node scratch an eviction destroys.
//
// Memory-backed emptyDirs are excluded: those are tmpfs, they hold no data that
// survives a restart anyway, and /dev/shm is the overwhelmingly common case. It
// is the disk-backed ones that carry a checkpoint somebody wanted.
func podsWithLocalData(pods []corev1.Pod) []string {
	var out []string
	for i := range pods {
		pod := &pods[i]
		if skipDuringDrain(pod, true) {
			continue // not evicted by this drain either way
		}
		for _, volume := range pod.Spec.Volumes {
			dir := volume.EmptyDir
			if dir == nil || dir.Medium == corev1.StorageMediumMemory {
				continue
			}
			out = append(out, pod.Namespace+"/"+pod.Name)
			break
		}
	}
	sort.Strings(out)
	return out
}

func unmanagedPod(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}
	if _, isMirror := pod.Annotations[corev1.MirrorPodAnnotationKey]; isMirror {
		return false
	}
	// Already on its way out. The refusal exists because nothing would
	// recreate this pod elsewhere; a pod whose deletion has been requested is
	// not staying either way, so blocking on it only costs the rung.
	//
	// kubectl behaves the other way by default, but kubectl is typed by a
	// human who sees the message and retries ten seconds later. A ladder has
	// nobody there: an SRE who deletes their `kubectl debug` pod just before
	// the drain step fires would still fail the rung and spend an escalation.
	if pod.DeletionTimestamp != nil {
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

// gpuReplicasLabel is GPU Feature Discovery's time-slicing replica factor.
const gpuReplicasLabel = "nvidia.com/gpu.replicas"

// parsePositiveLabel reads a small positive integer from a node label, or 0
// when it is absent or is anything else. Node-written, so nothing here may
// panic or overflow on it.
func parsePositiveLabel(labels map[string]string, key string) int {
	n, err := strconv.Atoi(labels[key])
	if err != nil || n <= 0 || n > maxAdvertisedAccelerators {
		return 0
	}
	return n
}

func acceleratorCount(capacity corev1.ResourceList, nodeLabels map[string]string) int {
	whole, partitioned := 0, 0
	implausibleCapacity := false
	for name, q := range capacity {
		domain, class := classifyAcceleratorResource(name)
		if class == notAnAccelerator {
			continue
		}
		if _, known := acceleratorDomains[domain]; !known {
			continue
		}
		switch class {
		case monitoringHandle:
			// Not hardware. Counting it reported a GPU that does not exist.
			continue
		case wholeDevice:
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
			// aliyun.com/gpu-mem's MiB, Intel's millicores and tiles, Neuron
			// cores. Only note that such a resource exists.
			partitioned++
		}
	}
	if whole > 0 {
		// Time-slicing with renameByDefault: false advertises
		// physical x replicas under the SAME nvidia.com/gpu name, so the
		// device count silently multiplies on the primary vendor — the same
		// defect as Intel's millicores, wearing the resource name everything
		// else trusts, and small enough to stay under the clamp. GPU Feature
		// Discovery publishes the factor as a label; when it is there, divide
		// by it.
		//
		// Absent, unparseable or <= 1, nothing changes. Guarding the division
		// rather than trusting the label matters: it is node-written, like the
		// capacity itself.
		if replicas := parsePositiveLabel(nodeLabels, gpuReplicasLabel); replicas > 1 {
			whole = max(1, whole/replicas)
		}
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
	gpus := acceleratorCount(n.Status.Capacity, n.Labels)
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
