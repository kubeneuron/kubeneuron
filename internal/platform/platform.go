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

// DrainOptions controls workload eviction during a drain.
type DrainOptions struct {
	// Timeout bounds the whole drain; expiry fails the playbook step.
	Timeout time.Duration
	// Force evicts workloads that lack a controller/manager.
	Force bool
	// GracePeriod overrides the workload's own termination grace period
	// when >= 0.
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
	Cordon(ctx context.Context, node string, reason string) error
	// Uncordon makes the node schedulable again.
	Uncordon(ctx context.Context, node string) error
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
}

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
