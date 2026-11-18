// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"errors"
	"time"
)

// ErrAddFirstUnavailable marks a node whose pool cannot bring up a successor
// before the node is removed: the pool is owned by an autoscaler, is at its
// maximum size, or gives no way to tell which new instance is ours. The
// wrapping error says which.
var ErrAddFirstUnavailable = errors.New("add-first replacement is not available for this node")

// ErrNeedsOperator marks a successor operation the provider will not settle on
// its own: what the pool did cannot be tied to this operation, or undoing it
// would remove something it did not create. Nothing more is changed
// automatically; the wrapping error says what a person has to look at.
var ErrNeedsOperator = errors.New("the replacement needs an operator")

// SuccessorPhase is how far a successor operation has come.
type SuccessorPhase string

const (
	// SuccessorPlanned: the provider has read the pool and recorded in State
	// what it needs to recognise its own request later. Nothing has changed in
	// the cloud yet.
	SuccessorPlanned SuccessorPhase = "planned"
	// SuccessorRequested: the provider has asked the pool for one more
	// instance; the instance is not known yet.
	SuccessorRequested SuccessorPhase = "requested"
	// SuccessorCreated: exactly one new instance in the pool belongs to this
	// operation; ProviderID names it.
	SuccessorCreated SuccessorPhase = "created"
)

// Successor is one add-first operation: a new instance requested in the pool
// of the pinned instance it is to replace. The caller persists it after every
// call and passes it back unchanged, so an operation survives a restart and is
// continued, never started twice.
type Successor struct {
	// Operation identifies the replacement this successor belongs to; it is
	// stable across retries and restarts.
	Operation string `json:"operation"`
	// Instance is the pinned instance the successor replaces.
	Instance string `json:"instance"`
	// Pool is the provider's name for the group both instances belong to.
	Pool        string         `json:"pool,omitempty"`
	Phase       SuccessorPhase `json:"phase,omitempty"`
	RequestedAt time.Time      `json:"requested_at"`
	// ProviderID is the successor's Kubernetes providerID once Phase is
	// SuccessorCreated.
	ProviderID string `json:"provider_id,omitempty"`
	// State is the provider's own record of the operation (the pool's size and
	// members before the request, its marker). The caller stores it verbatim.
	State map[string]string `json:"state,omitempty"`
}

// SuccessorProvider brings up the successor of a node before the node is
// drained, for replacement by adding first. Every method acts only on the
// operation it is given and is safe to call again with the same input.
type SuccessorProvider interface {
	// CheckAddFirst reports whether the instance's pool can prepare a
	// successor: nil, or an error wrapping ErrAddFirstUnavailable with the
	// reason. Any other error is a transient lookup failure.
	CheckAddFirst(ctx context.Context, instanceID string) error
	// PrepareSuccessor moves the operation one phase on and returns it.
	//
	// From no phase it plans and changes nothing in the cloud, so the caller
	// can record the plan before the first request. From SuccessorPlanned it
	// requests the instance; a retry after a request that was accepted but
	// not answered must find that request and not grow the pool again. From
	// SuccessorRequested it returns SuccessorCreated once exactly one new
	// instance can be tied to the operation, and the same phase while there is
	// none yet. No capacity or quota wraps ErrReplacementCapacity, a pool
	// change it cannot attribute wraps ErrNeedsOperator. It never deletes.
	PrepareSuccessor(ctx context.Context, s Successor) (Successor, error)
	// ReleaseSuccessor undoes the operation when the replacement is
	// abandoned: it removes the created successor and returns the pool to its
	// size before the request. With no successor created yet, it restores the
	// size only if the pool's members are exactly those before; otherwise it
	// returns ErrNeedsOperator. The caller has made sure the successor runs no
	// workload.
	ReleaseSuccessor(ctx context.Context, s Successor) error
	// ReplaceWithSuccessor removes the pinned instance once its created
	// successor has taken over, leaving the pool at its size before the
	// request. It continues the same operation after a restart.
	ReplaceWithSuccessor(ctx context.Context, s Successor) error
}
