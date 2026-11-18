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

// ErrRemoveFirstUnavailable is wrapped by a provider that cannot replace a
// node by removing it first, with the reason.
var ErrRemoveFirstUnavailable = errors.New("remove-first replacement is not available for this node")

// RemoveFirstChecker is implemented by a provider whose pools may refuse a
// remove-first replacement. The controller asks it before the node is
// cordoned, so a refusal never leaves a drained node without a replacement.
// A provider that does not implement it is taken to allow remove-first
// wherever it allows Replace.
type RemoveFirstChecker interface {
	// CheckRemoveFirst is nil, or an error wrapping
	// ErrRemoveFirstUnavailable with the reason. Any other error is a
	// transient lookup failure.
	CheckRemoveFirst(ctx context.Context, instanceID string) error
}

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

// UnrequestedPlan reports whether the caller has yet to record a growth request.
func (s Successor) UnrequestedPlan() bool {
	return s.Phase == SuccessorPlanned && s.RequestedAt.IsZero()
}

// SuccessorProvider brings up the successor of a node before the node is
// drained, for replacement by adding first. Every method acts only on the
// operation it is given and is safe to call again with the same input.
// Before requesting growth the caller must persist the planned operation with
// RequestedAt set. For a planned operation with zero RequestedAt, PrepareSuccessor
// returns an error and ReleaseSuccessor succeeds. Neither calls the cloud.
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
	// instance can be tied to the operation. While the request is still in
	// progress and no new instance is there yet, it returns the same phase and
	// no error; the caller asks again until its timeout. A finished request
	// with no instance to tie to it, or a pool change it cannot attribute,
	// wraps ErrNeedsOperator. No capacity or quota wraps
	// ErrReplacementCapacity. It never deletes.
	PrepareSuccessor(ctx context.Context, s Successor) (Successor, error)
	// ReleaseSuccessor undoes the operation when the replacement is
	// abandoned: it removes the created successor and returns the pool to its
	// size before the request. With no successor created yet, it restores the
	// size only if the pool's members are exactly those before and it can show
	// that no request of the operation is still in flight: it never reached
	// the cloud, or the cloud ended it. Members equal to those before are not
	// enough after a request whose answer was lost. Otherwise it returns
	// ErrNeedsOperator and changes nothing. The caller has made sure the
	// successor runs no workload.
	ReleaseSuccessor(ctx context.Context, s Successor) error
	// ReplaceWithSuccessor removes the pinned instance once its created
	// successor has taken over, leaving the pool at its size before the
	// request. It continues the same operation after a restart. When the
	// operation or the successor does not match the pool, or a delete was
	// sent and its result is unknown, it wraps ErrNeedsOperator: the caller
	// then keeps the successor and changes nothing more.
	ReplaceWithSuccessor(ctx context.Context, s Successor) error
}
