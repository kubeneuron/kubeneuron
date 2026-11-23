// SPDX-License-Identifier: Apache-2.0

// Package removefirst describes what a built-in provider can establish about
// the original machine after a remove-first delete may have reached the cloud.
package removefirst

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
