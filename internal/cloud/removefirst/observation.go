// SPDX-License-Identifier: Apache-2.0

// Package removefirst describes what a built-in provider can establish about
// the original machine after a remove-first delete may have reached the cloud.
package removefirst

import (
	"context"
	"errors"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// ErrRefused denotes an explicit rejection of the initial delete request.
var ErrRefused = errors.New("delete request refused")

type Observation string

const (
	Unknown     Observation = ""
	SameMachine Observation = "same-machine"
	// Listed requires a matching live Kubernetes identity before retry.
	Listed          Observation = "listed"
	IdentityChanged Observation = "identity-changed"
	Deleting        Observation = "deleting"
	Deleted         Observation = "deleted"
)

// RefusedAfterGrow carries the verified pre-delete size of a fixed AKS pool.
// CurrentSize is zero if the post-refusal read could not confirm that size.
type RefusedAfterGrow struct {
	Cause       error
	GrownSize   int32
	CurrentSize int32
}

func (e *RefusedAfterGrow) Error() string { return e.Cause.Error() }
func (e *RefusedAfterGrow) Unwrap() error { return e.Cause }

// PhaseGrowing is a targeted delete whose fixed-size pool is being grown by
// one first: the record holds the size before and the absolute target.
const PhaseGrowing = "growing"

// Grow is what a provider records before it grows a fixed-size pool by one
// for a replacement. Target is absolute: a resumed replacement compares the
// pool with it and never adds one to a size read again.
type Grow struct {
	Pool    string
	Machine string
	Before  int32
	Target  int32
	// Marker is the pool tag value the grow writes with the new size.
	Marker string
}

// Intents are the callbacks a provider runs before each change it sends.
// An error from one stops the replacement before that change.
type Intents struct {
	// Grow runs before a fixed-size pool is grown; nil grows without it.
	Grow func(Grow) error
	// Delete runs before the machine's delete is sent.
	Delete func(pool, machine, successor string) error
}

// GrowState is what a provider reads back about a recorded grow.
type GrowState string

const (
	// GrowUnknown: the pool is busy or could not be read; wait.
	GrowUnknown GrowState = ""
	// GrowNotApplied: no marker and the size before; nothing changed yet.
	GrowNotApplied GrowState = "not-applied"
	// GrowApplied: the marker, the target size and as many machines.
	GrowApplied GrowState = "applied"
	// GrowChanged: anything else; never grown again.
	GrowChanged GrowState = "changed"
)

// GrowObservation is a read of a recorded grow. Size is the pool's node count
// when it was read from a settled pool, zero otherwise.
type GrowObservation struct {
	State  GrowState
	Size   int32
	Detail string
}

// SettleResult is what finishing a grown pool after its machine is gone
// found: Size is the count read, Before the size the marker recorded.
type SettleResult struct {
	Size, Before int32
	// Extra is true when the pool was read settled at one node more than
	// before: the delete went through and the pool did not shrink.
	Extra bool
	// Untagged is true when the pool's replacement tag was removed.
	Untagged bool
}

// State keys a provider writes on a plan that sets a pool's size back: the
// name of the resize operation it sent, and what showed the plan done.
// EvidenceFinalState is a pool found at its size without the resize
// operation's own report.
const (
	StateResizeOperation = "operation"
	StateEvidence        = "evidence"
	EvidenceOperation    = "operation"
	EvidenceFinalState   = "final-state"
)

// ResizeMaySend reports whether continuing the plan may send the resize: it
// is recorded as resizing and no resize operation is recorded yet. A resize
// whose answer was lost is sent again only if the pool still shows nothing
// of it.
func ResizeMaySend(plan cloud.RemoveFirstPlan) bool {
	return plan.Phase == cloud.RemoveFirstResizing && plan.State[StateResizeOperation] == ""
}

// PoolSize is a pool's node count read from the cloud, and the count it had
// when the plan was made.
type PoolSize struct {
	Current, Planned int32
}

type noSendKey struct{}

// WithoutSend marks a continuation that may read and record but must not
// send a resize: the caller holds it.
func WithoutSend(ctx context.Context) context.Context {
	return context.WithValue(ctx, noSendKey{}, true)
}

// MaySend reports whether a continuation may send a resize.
func MaySend(ctx context.Context) bool {
	held, _ := ctx.Value(noSendKey{}).(bool)
	return !held
}
