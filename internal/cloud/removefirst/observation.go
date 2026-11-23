// SPDX-License-Identifier: Apache-2.0

// Package removefirst describes what a built-in provider can establish about
// the original machine after a remove-first delete may have reached the cloud.
package removefirst

import "errors"

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
