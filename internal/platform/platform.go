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
