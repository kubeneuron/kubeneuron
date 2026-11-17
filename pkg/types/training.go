// SPDX-License-Identifier: Apache-2.0

package types

// TrainingOwner identifies one controller in the observed worker's namespace.
type TrainingOwner struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

// TrainingWorker records the pod even when its owning job cannot be verified.
type TrainingWorker struct {
	Namespace          string          `json:"namespace"`
	Pod                string          `json:"pod"`
	PodUID             string          `json:"podUID"`
	Owners             []TrainingOwner `json:"owners"`
	OwnerUnknownReason string          `json:"ownerUnknownReason,omitempty"`
}
