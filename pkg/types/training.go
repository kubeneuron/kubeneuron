// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// TrainingOwner identifies one controller in the observed worker's namespace.
type TrainingOwner struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

// TrainingWorker records the pod even when its owning job cannot be verified.
type TrainingWorker struct {
	Source             string          `json:"source,omitempty"`
	ObservedAt         *time.Time      `json:"observed_at,omitempty"`
	WorkerRecreation   string          `json:"worker_recreation"`
	CompletionIndex    *int64          `json:"completion_index,omitempty"`
	ControllerState    string          `json:"controller_state"`
	Namespace          string          `json:"namespace"`
	Pod                string          `json:"pod"`
	PodUID             string          `json:"pod_uid"`
	Owners             []TrainingOwner `json:"owners"`
	OwnerUnknownReason string          `json:"owner_unknown_reason,omitempty"`
}

// AffectedJob groups the workers of a verified controller in one namespace.
// An unresolved worker has its own entry and retains the reported owner chain.
type AffectedJob struct {
	Recovery  TrainingRecoverySnapshot `json:"recovery"`
	Namespace string                   `json:"namespace"`
	Owner     *TrainingOwner           `json:"owner,omitempty"`
	Workers   []TrainingWorker         `json:"workers"`
}

func GroupAffectedJobs(workers []TrainingWorker) []AffectedJob {
	if workers == nil {
		return nil
	}
	jobs := make([]AffectedJob, 0)
	type identity struct{ namespace, uid string }
	indices := make(map[identity]int)
	for _, w := range workers {
		job := AffectedJob{Namespace: w.Namespace, Workers: []TrainingWorker{w}}
		if w.OwnerUnknownReason == "" && len(w.Owners) > 0 {
			root := w.Owners[len(w.Owners)-1]
			if root.UID != "" {
				key := identity{w.Namespace, root.UID}
				if idx, ok := indices[key]; ok {
					jobs[idx].Workers = append(jobs[idx].Workers, w)
					continue
				}
				job.Owner = &root
				indices[key] = len(jobs)
			}
		}
		jobs = append(jobs, job)
	}
	return jobs
}

// TrainingRecoverySnapshot carries independently established observations.
// Nil values encode unknown, including for incidents that predate the contract.
type TrainingRecoverySnapshot struct {
	InfrastructureReadyAt          *time.Time `json:"infrastructure_ready_at"`
	ProgressConfirmedAt            *time.Time `json:"progress_confirmed_at"`
	CheckpointAgeAtDecisionSeconds *int64     `json:"checkpoint_age_at_decision_seconds"`
	StepsAfterCheckpoint           *int64     `json:"steps_after_checkpoint"`
	ConfirmedRepeatedSteps         *int64     `json:"confirmed_repeated_steps"`
}

// RecordCheckpointSteps takes a verified pre-fault checkpoint and last step
// on the same scale. resumedStep is supplied only for subsequent progress of
// the current attempt after it confirms loading that exact checkpoint.
func (s *TrainingRecoverySnapshot) RecordCheckpointSteps(checkpoint, last, resumedStep *int64) {
	s.StepsAfterCheckpoint = nil
	s.ConfirmedRepeatedSteps = nil
	if checkpoint == nil || last == nil || *checkpoint < 0 || *last < *checkpoint {
		return
	}
	gap := *last - *checkpoint
	s.StepsAfterCheckpoint = &gap
	if resumedStep != nil && *resumedStep > *checkpoint {
		repeated := min(*resumedStep, *last) - *checkpoint
		s.ConfirmedRepeatedSteps = &repeated
	}
}

// VerifiedInfrastructureTime reads the durable evidence recorded when the
// controller accepted post-recycle inventory from the verified boot.
func VerifiedInfrastructureTime(trail []*AuditEntry) *time.Time {
	for _, entry := range trail {
		if entry != nil && !entry.DryRun && entry.Actor == "system" && entry.Action == "resolve" && entry.FromState == StateVerifying && entry.ToState == StateResolved && entry.Params["infrastructure_evidence"] == "verified_recycle_inventory" && !entry.Time.IsZero() {
			t := entry.Time
			return &t
		}
	}
	return nil
}
