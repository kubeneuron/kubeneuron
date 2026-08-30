// Package platform abstracts where the GPU nodes live. A Platform answers
// two questions: which nodes/GPUs exist (inventory), and how to move
// workloads off a node (cordon/drain). Executing commands ON a node is a
// separate concern — see internal/actuator.
//
// Shipped implementations: kubernetes (node informer + eviction API) and
// baremetal (inventory file + agent self-registration, pluggable drain
// hook). Slurm, VM, and cloud platforms implement the same interface.
package platform

import (
	"context"
	"slices"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// NodeEventType describes an inventory change.
type NodeEventType string

const (
	NodeAdded   NodeEventType = "added"
	NodeUpdated NodeEventType = "updated"
	NodeRemoved NodeEventType = "removed"
)

// NodeEvent is an inventory change notification from WatchNodes.
type NodeEvent struct {
	Type NodeEventType
	Node types.Node
}

// DrainUsePodGracePeriod tells Drain to leave each pod's own
// terminationGracePeriodSeconds alone, clamping only where the step's own
// deadline cannot accommodate it. DeleteOptions.GracePeriodSeconds overrides
// the pod spec in BOTH directions, so any concrete value here silently
// truncates a workload that asked for longer.
//
// It is also the ZERO VALUE of DrainOptions.GracePeriod, on purpose: an
// explicit 0 means "SIGKILL immediately", and that must never be what a
// caller gets by writing DrainOptions{Timeout: x} and thinking about the
// timeout. The most destructive eviction possible should require saying so.
const DrainUsePodGracePeriod = 0

// DrainOptions controls workload eviction during a drain.
type DrainOptions struct {
	// Timeout bounds the whole drain; expiry fails the playbook step.
	Timeout time.Duration
	// Force evicts workloads that lack a controller/manager.
	Force bool
	// GracePeriod overrides the workload's own termination grace period when
	// POSITIVE. Zero (the default) leaves the pod's own period in place — see
	// DrainUsePodGracePeriod. There is deliberately no way to express
	// "SIGKILL immediately" here; nothing in this product wants it.
	GracePeriod time.Duration
}

// Workload is a schedulable unit running on a node (a pod, a job, ...).
type Workload struct {
	Name      string
	Namespace string
	Kind      string
	// UsesGPU marks workloads holding GPU resources (used by XID 94
	// targeted restarts).
	UsesGPU bool
}

// Platform is the per-environment implementation of inventory and workload
// control.
type Platform interface {
	// Name is the platform identifier: "kubernetes", "baremetal", ...
	Name() string

	// ListNodes returns the current GPU node inventory.
	ListNodes(ctx context.Context) ([]types.Node, error)
	// WatchNodes streams inventory changes until ctx is done.
	// Implementations that cannot watch may return a channel that only
	// closes on ctx.Done().
	WatchNodes(ctx context.Context) (<-chan NodeEvent, error)

	// Cordon marks a node unschedulable. Reason lands in the node's
	// annotation/labels where supported.
	//
	// Prefer CordonForOwner. This one takes no owner, so it cannot participate
	// in the count that keeps a node down while another remediation is still
	// working on it; it remains for callers that genuinely act on the node
	// rather than on behalf of an incident.
	Cordon(ctx context.Context, node string, reason string) error
	// Uncordon makes the node schedulable again. See Cordon: prefer
	// ReleaseCordonOwners, which cannot release a node somebody else holds.
	Uncordon(ctx context.Context, node string) error

	// CordonForOwner and ReleaseCordonOwners are REQUIRED, not an optional
	// capability a platform may skip.
	//
	// They used to be an optional interface with a fallback to the unguarded
	// pair above, and the fallback silently reintroduced the P0 they exist to
	// prevent: with several GPUs per node, two incidents can hold one machine,
	// and the first to finish handed it back while the other was still
	// resetting a GPU on it. A platform that simply did not implement the
	// optional interface got that behaviour with nothing to notice — a new
	// adapter would compile, pass its tests, and be wrong only in production.
	//
	// Putting them here makes forgetting a compile error instead.

	// CordonForOwner cordons the node on behalf of one remediation, adding
	// owner to the node's owner set. It is idempotent: an owner already in the
	// set is not added twice, and any original-state snapshot is written only
	// by the first owner, never overwritten by a later one.
	CordonForOwner(ctx context.Context, node, owner, reason string) error
	// ReleaseCordonOwners removes the named owners and reports whether that
	// emptied the set — released is true only when the node was actually
	// returned to service, and remaining counts the holders left. Removing an
	// owner that is not there is a no-op, not an error: steps are retried and
	// replayed.
	ReleaseCordonOwners(ctx context.Context, node string, owners []string) (released bool, remaining int, err error)
	// Drain evicts workloads from the node.
	Drain(ctx context.Context, node string, opts DrainOptions) error

	// NodeWorkloads lists workloads currently on the node.
	NodeWorkloads(ctx context.Context, node string) ([]Workload, error)
	// EvictWorkload removes a single workload (targeted restart for
	// contained errors like XID 94).
	EvictWorkload(ctx context.Context, w Workload) error
}

// CordonedNode is a node this product cordoned, with the reason it recorded.
type CordonedNode struct {
	Name   string
	Reason string
	// Owners lists every remediation currently holding this node cordoned, as
	// recorded by CordonOwnership.CordonForOwner. Empty means the platform does
	// not track ownership, or the node carries a cordon placed by a build that
	// predates the owner set — in both cases the single Reason above is the only
	// thing linking the node to whatever took it out of service.
	//
	// It exists because Reason cannot answer the janitor's question on a
	// multi-GPU node. Incidents are per (target, class), so two of them can be
	// remediating two GPUs of one machine at once, and only one of their reasons
	// fits in the annotation. The janitor then evaluated one incident and could
	// not see the other: an abandoned cordon whose reason had been overwritten
	// was never released, and a live one whose reason happened to be on the node
	// was.
	Owners []string
	// Held is set once the janitor has decided a human owns this cordon. It
	// survives the incident row, which retention eventually prunes.
	//
	// It is NODE-scoped, so it can only be trusted on a node with a single
	// holder — an untracked cordon. Ask HeldBy instead of reading it.
	Held bool
	// HeldOwners lists the individual holds a human has taken charge of. It is
	// the owner-set counterpart of Held and exists because Held cannot be
	// answered per hold on a shared cordon.
	//
	// A node-scoped mark says "a human owns this cordon" about the whole machine,
	// and it is the one thing that stops the janitor releasing a hold whose
	// incident row retention has swept. One halted remediation therefore answered
	// that question with "yes" for every OTHER remediation on the same node: an
	// abandoned hold beside it could never be dropped, the owner set could never
	// empty, and the mark blocking it is only cleared when the set DOES empty.
	// The node deadlocked out of the fleet, with no incident row left to explain
	// why and no stuck-cordon notification, because the incident behind the
	// surviving hold was gone.
	HeldOwners []string
}

// Tracked reports whether this cordon is reference-counted — whether the
// platform recorded an owner set for it, as opposed to a cordon placed by a
// build that predates them, or by a platform that cannot count holders.
//
// It is one predicate rather than a `len(node.Owners) > 0` at each call site
// because the three questions below have to agree about the same node: which
// holds exist, which of them a human owns, and which release path applies.
func (n CordonedNode) Tracked() bool { return len(n.Owners) > 0 }

// Holds lists every remediation to decide about on this node.
//
// An untracked cordon names its single holder by the reason it wrote, which is
// exactly what a legacy owner is, so callers get one shape for both and cannot
// judge a shared cordon by whichever incident happened to cordon last.
func (n CordonedNode) Holds() []string {
	if n.Tracked() {
		return n.Owners
	}
	return []string{LegacyCordonOwner(n.Reason)}
}

// HeldBy reports whether a human has taken charge of ONE hold on this node.
//
// The node-scoped Held mark answers for the whole machine, so it is only
// consulted where it cannot be wrong: an untracked cordon has exactly one
// holder. On a counted cordon the answer must name the hold, or a verdict about
// one remediation strands every other hold on the node forever.
func (n CordonedNode) HeldBy(owner string) bool {
	if !n.Tracked() {
		return n.Held
	}
	return slices.Contains(n.HeldOwners, owner)
}

// LegacyCordonOwnerPrefix marks an owner-set entry that stands in for a cordon
// placed before this product tracked cordon ownership. Such a cordon identifies
// itself only by the reason it wrote, so the reason IS the owner name, and this
// prefix says so rather than letting it be mistaken for an incident ID.
//
// It is what keeps an upgrade honest. A node cordoned by the running build
// carries a reason and no owner set; the first owner to join afterwards seeds
// the set with the entry below, so the incident that is still remediating that
// node cannot be forgotten and released out from under by the newcomer.
const LegacyCordonOwnerPrefix = "legacy-reason:"

// LegacyCordonOwner names the pre-upgrade holder of a cordon by its reason.
func LegacyCordonOwner(reason string) string { return LegacyCordonOwnerPrefix + reason }

// CordonJanitor is implemented by platforms that can report the nodes this
// product cordoned.
//
// A playbook that dies between its cordon and its uncordon leaves the node
// unschedulable with nothing left running to notice. That was measured: a reboot
// interrupted its own controller, and the node stayed cordoned until a human
// looked. Listing them lets the controller reconcile that residue instead of
// depending on every failure path remembering to clean up after itself.
type CordonJanitor interface {
	CordonedNodes(ctx context.Context) ([]CordonedNode, error)
	// UncordonIfReason releases a cordon only if the node still carries the
	// reason the caller decided on, reporting whether it did.
	//
	// The listing above is served from a cache, and a stale entry is not a
	// missed cordon — it is a cordon that has since been REPLACED. A node that
	// resolved and immediately faulted again is cordoned by a new incident
	// while the old reason is still in the cache, and releasing on that basis
	// hands the scheduler a machine in the middle of its own drain. The check
	// has to happen against the live object, at the moment of the write.
	UncordonIfReason(ctx context.Context, node, expectedReason string) (released bool, err error)
	// MarkCordonHeldIfReason records on the node that a human owns this cordon, so a
	// later pass cannot mistake an unreadable incident for a resolved one.
	// Reason-scoped for the same reason its sibling above is: the listing it
	// is called from comes from the informer cache, and a stale entry is not a
	// missed cordon but a cordon that has since been REPLACED. Stamping the
	// held mark from a decision made about incident A onto incident B's live
	// cordon is worse than doing nothing, because the mark deliberately
	// OUTLIVES the incident row — when B's row is pruned the janitor sees the
	// mark and keeps the node cordoned forever.
	MarkCordonHeldIfReason(ctx context.Context, node, expectedReason string) (marked bool, err error)
}

// CordonOwnership is implemented by platforms that can hold ONE node cordoned on
// behalf of SEVERAL remediations at once.
//
// A node has many GPUs and an incident is per (target, class), so two GPUs on
// one machine can be remediated concurrently. Cordon/Uncordon cannot express
// that: the state is one reason and one restore snapshot, the second cordon
// overwrites the first, and the first playbook's uncordon then hands the whole
// machine back to the scheduler while the other incident is still working —
// tenant work lands on a node whose GPU is about to be reset. It also restores
// from whichever snapshot survived, so a human's `kubectl cordon` and their
// karpenter.sh/do-not-disrupt pin can be wiped by an incident that never saw
// them.
//
// The fix is a reference count that lives on the node: each remediation joins an
// owner set, the original state is snapshotted once by whoever joins first, and
// the node is put back only when the LAST owner leaves. Every write is a
// compare-and-swap against the value that was read, so two controllers racing
// cannot both win.
//
// It is a separate interface, like CordonJanitor and NodeTainter, because not
// every platform has an object to hold the set on. Platforms that do not
// implement it keep the single-owner Cordon/Uncordon behaviour, which is correct
// for one remediation at a time and is all they ever had.
type CordonOwnership interface {
	// CordonForOwner cordons the node on behalf of one remediation, adding owner
	// to the node's owner set. It is idempotent: an owner already in the set is
	// not added twice, and the original-state snapshot is written only by the
	// first owner, never overwritten by a later one.
	CordonForOwner(ctx context.Context, node, owner, reason string) error
	// ReleaseCordonOwners removes the named owners from the node's owner set and
	// reports whether that emptied it — released is true only when the node was
	// actually returned to service, and remaining counts the holders left.
	//
	// Removing an owner that is not in the set is a no-op and not an error:
	// steps are retried and replayed, and a release that already happened must
	// stay quiet rather than fail a playbook that has nothing left to do.
	//
	// The caller passes every name it may be known by — its incident ID and, for
	// a cordon it placed before this product tracked ownership,
	// LegacyCordonOwner(reason).
	ReleaseCordonOwners(ctx context.Context, node string, owners []string) (released bool, remaining int, err error)
	// MarkCordonHeldIfOwner records that a human owns THIS HOLD, but only while
	// owner is still in the node's owner set. The mark it writes is reported back
	// as CordonedNode.HeldOwners and must name the hold, not the node.
	//
	// It is the owner-set counterpart of MarkCordonHeldIfReason and exists
	// because that one cannot be reached on a shared cordon: the reason
	// annotation belongs to whichever incident cordoned LAST, so a decision made
	// about any other owner never matches it, the held mark is never written,
	// and once retention prunes that incident's row the janitor sees an
	// unexplained owner and puts a node a human took charge of back into
	// service.
	//
	// Naming the hold is what keeps the other direction safe. A node-scoped mark
	// answers "does a human own this?" with "yes" for every other remediation on
	// the machine too, so an abandoned hold beside a halted one can never be
	// dropped and the node never returns to the fleet.
	MarkCordonHeldIfOwner(ctx context.Context, node, owner string) (marked bool, err error)
}

// NodeTainter is implemented by platforms whose scheduler can be told to
// prefer other nodes.
//
// It is deliberately separate from Cordon. A cordon is a decision — this node
// is not fit, take it out of service — made after evidence and undone
// deliberately. This is a hint attached to the mere EXISTENCE of an open
// incident: the node may still be perfectly usable, nothing running on it is
// touched, and the only effect is that the scheduler stops adding to the pile
// while the incident is worked. Both are opt-in and both must be removable
// without the process that applied them.
type NodeTainter interface {
	// ApplyDegradedTaint marks the node degraded with the given value and
	// effect. It must be idempotent and must not disturb taints it does not
	// own.
	ApplyDegradedTaint(ctx context.Context, node, value, effect string) error
	// RemoveDegradedTaint clears the mark. Safe on a node that carries none.
	RemoveDegradedTaint(ctx context.Context, node string) error
	// DegradedTaintedNodes lists the nodes currently carrying the mark.
	//
	// It exists for the same reason CordonJanitor does: the marks live on the
	// cluster, not in this process's memory, so a controller that died between
	// applying one and closing its incident must be able to find it again. A
	// taint nobody will ever come back for is worse than no taint at all.
	DegradedTaintedNodes(ctx context.Context) ([]string, error)
}

// InstanceRecycler drives the cloud instance behind a node. It is satisfied by
// any cloud provider (internal/cloud) and injected into the platform, so the
// platform stays free of any cloud SDK and of any provider's providerID scheme.
//
// The provider owns the node -> instance-ID mapping: each cloud writes its
// Kubernetes providerID in its own format, so InstanceID parses it on the
// provider side and the platform never learns what any provider's providerID
// scheme looks like.
type InstanceRecycler interface {
	// InstanceID extracts the cloud instance ID from a node's providerID using
	// the provider's own scheme. An unparseable or foreign providerID fails
	// closed with an error, so a step never recycles the wrong instance.
	InstanceID(providerID string) (string, error)
	// CheckRecycle reports whether Recycle can work for this exact instance
	// (capabilities are provider-scoped, viability is instance-scoped). A
	// definitive no wraps cloud.ErrRecycleNotViable.
	CheckRecycle(ctx context.Context, instanceID string) error
	// Recycle stops and starts the instance, reinitializing the GPU passthrough.
	Recycle(ctx context.Context, instanceID string) error
	// Replace terminates the instance for the autoscaler to replace.
	Replace(ctx context.Context, instanceID string) error
}

// NodeRecycler is implemented by platforms that can recycle or replace the
// cloud instance behind a GPU node.
//
// It is the cloud-native stand-in for a hardware GPU reset, which is impossible
// on a virtualized instance: the hypervisor withholds the PCI reset from the
// guest. Recycling (stop/start) reinitializes the GPU passthrough on the same
// node; replacing (terminate) hands the node to the autoscaler. Both are driven
// by the controller, never the agent, because the agent dies with its node.
type NodeRecycler interface {
	// CheckRecycleNode reports whether RecycleNode can work for this exact
	// node's instance. A definitive no wraps cloud.ErrRecycleNotViable — the
	// controller escalates to ReplaceNode at admission instead of a human
	// approving a recycle that the node group will fight and lose by timeout.
	// Any other error is a transient lookup failure.
	CheckRecycleNode(ctx context.Context, node string) error
	// RecycleNode stops and starts the node's instance.
	RecycleNode(ctx context.Context, node string) error
	// ReplaceNode terminates the node's instance.
	ReplaceNode(ctx context.Context, node string) error
	// CloudRecyclingConfigured reports whether a cloud provider is wired up, so
	// the capability can be advertised only where it can actually be performed.
	CloudRecyclingConfigured() bool
	// NodeReady reports whether the node has rejoined the cluster and is Ready.
	// A recycle is not done when the instance is merely powered on: the OS,
	// kubelet and agent take minutes more to return, and a verify that fires in
	// that window fails on a stale heartbeat. The recycle step waits on this.
	NodeReady(ctx context.Context, node string) (bool, error)
}

// NodePresence is implemented by platforms that can answer whether one exact
// node object still exists. It is deliberately separate from ListNodes: GPU
// inventory may temporarily omit a healthy Kubernetes Node while a device
// plugin is down, which must never be interpreted as node deletion.
type NodePresence interface {
	NodeExists(ctx context.Context, node string) (bool, error)
}

// NodeLabeler is implemented by platforms that can read one exact node's
// labels WITHOUT going through the GPU-filtered inventory.
//
// It exists for the same reason NodePresence does, and the omission was the
// same mistake in a more dangerous place. Destructive blast-radius confinement
// asks "is this node inside spec.safety.destructiveExecution.nodeSelector?",
// and answering it from ListNodes means a node that has dropped out of the GPU
// inventory — a device plugin restarting, a driver reloading, exactly the
// conditions under which remediation is wanted — becomes UNRESOLVABLE. Every
// destructive step on it then holds forever, leaving the machine cordoned and
// drained with no path forward and nobody paged.
//
// A node that is not found is (nil, false, nil): a resolved absence, not an
// error, so the caller can distinguish "gone" from "cannot tell right now".
type NodeLabeler interface {
	NodeLabels(ctx context.Context, node string) (labels map[string]string, found bool, err error)
}

// AcceleratorStackController is implemented by platforms that can stop and
// restart the vendor's own monitoring stack on one node.
//
// A GPU reset needs every handle on the device released, and the vendor's
// components hold them: on a stock NVIDIA GPU Operator node, nv-hostengine,
// dcgm-exporter and the device plugin each keep /dev/nvidia0 open without ever
// appearing as a compute application. Without this, nvidia-smi --gpu-reset
// fails with exit 19 no matter how thoroughly the node was drained.
//
// Implementations must be reversible and must report what they changed, so a
// playbook that fails midway can be put back exactly as it was.
type AcceleratorStackController interface {
	// QuiesceAcceleratorStack stops the vendor's monitoring components on the
	// node and returns the components it stopped. It is idempotent: components
	// already stopped are not reported as newly stopped.
	QuiesceAcceleratorStack(ctx context.Context, node string) ([]string, error)
	// RestoreAcceleratorStack undoes QuiesceAcceleratorStack and returns the
	// components it restarted. It is safe to call on a node that was never
	// quiesced.
	RestoreAcceleratorStack(ctx context.Context, node string) ([]string, error)
	// QuiescedNodes lists the nodes currently standing with their vendor stack
	// switched off by KubeNeuron.
	//
	// It exists so recovery does not depend on the controller's memory. A
	// controller that restarts mid-playbook has no idea which nodes it left
	// quiesced, and monitoring that stays off because a process died is the
	// worst outcome available.
	QuiescedNodes(ctx context.Context) ([]string, error)
}

// Whether the device is actually free is deliberately not asked here. A
// platform can only infer it from pod metadata, and that inference was wrong on
// a real cluster: a device plugin installed by the machine image carried
// different labels than the GPU Operator's, so the controller believed the
// stack had settled while the plugin still held the GPU. The node itself can
// read its own process table, so the agent answers that question instead.
