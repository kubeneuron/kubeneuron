// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"errors"
	"time"
)

// ErrRemoveFirstUnplanned is wrapped by PlanRemoveFirst for an instance whose
// pool needs no plan: removing the instance is the whole replacement (an
// autoscaler owns the pool's size), so the caller uses Provider.Replace.
var ErrRemoveFirstUnplanned = errors.New("the replacement needs no planned size restore")

// RemoveFirstPhase is how far a planned remove-first replacement has come.
type RemoveFirstPhase string

const (
	// RemoveFirstPlanned: the provider has read the pool and recorded in
	// State its size before. Nothing has changed in the cloud.
	RemoveFirstPlanned RemoveFirstPhase = "planned"
	// RemoveFirstDeleting: the pool was found as planned and the instance is
	// to be deleted; the caller records this before the delete is sent, so
	// from here the pool may be one node short.
	RemoveFirstDeleting RemoveFirstPhase = "deleting"
	// RemoveFirstResizing: the instance is gone and the pool was asked to
	// return to its size before.
	RemoveFirstResizing RemoveFirstPhase = "resizing"
	// RemoveFirstDone: the instance is gone and the pool is back at its size
	// before.
	RemoveFirstDone RemoveFirstPhase = "done"
)

// RemoveFirstPlan is one remove-first replacement on a pool whose size does
// not come back on its own: the instance is deleted, then the pool's size is
// set back. The caller persists the plan before the first call that changes
// the cloud, and again after every call, also one that returns an error, and
// passes it back unchanged: a restarted caller finishes the restore from what
// was recorded.
type RemoveFirstPlan struct {
	// Operation identifies the replacement; it is stable across retries and
	// restarts.
	Operation string `json:"operation"`
	// Instance is the instance being replaced.
	Instance string `json:"instance"`
	// Pool is the provider's name for the instance's group.
	Pool      string           `json:"pool,omitempty"`
	Phase     RemoveFirstPhase `json:"phase,omitempty"`
	PlannedAt time.Time        `json:"planned_at"`
	// Restore is what a person runs to set the pool back to its size before,
	// should the plan not finish on its own.
	Restore string `json:"restore,omitempty"`
	// State is the provider's own record of the plan; the caller stores it
	// verbatim.
	State map[string]string `json:"state,omitempty"`
}

// PlannedRemoveFirstProvider replaces an instance by removing it first on a
// pool whose size the provider must set back afterwards. Every method acts
// only on the plan it is given and is safe to call again with the same input.
type PlannedRemoveFirstProvider interface {
	// PlanRemoveFirst reads the instance's pool and returns a plan in
	// RemoveFirstPlanned. It changes nothing in the cloud. A pool that cannot
	// be replaced this way wraps ErrRemoveFirstUnavailable; one that needs no
	// plan wraps ErrRemoveFirstUnplanned.
	PlanRemoveFirst(ctx context.Context, instanceID, operation string) (RemoveFirstPlan, error)
	// ContinueRemoveFirst moves the plan on by one step and returns it. From
	// RemoveFirstPlanned it changes nothing: it returns RemoveFirstDeleting
	// if the pool is still as planned, or an error wrapping
	// ErrRemoveFirstUnavailable. From RemoveFirstDeleting it sends the delete,
	// again after a lost answer, then sets the size back. While another
	// operation runs on the pool, or a call of the plan is still in progress,
	// it returns the same phase and no error; the caller asks again. A pool
	// it can no longer restore safely wraps ErrNeedsOperator, with Restore the
	// way back. It never sets a size lower than the pool's current one.
	ContinueRemoveFirst(ctx context.Context, plan RemoveFirstPlan) (RemoveFirstPlan, error)
}
