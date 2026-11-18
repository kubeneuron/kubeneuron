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
	CompletionIndex    *int64          `json:"completionIndex,omitempty"`
	ControllerState    string          `json:"controllerState"`
	Namespace          string          `json:"namespace"`
	Pod                string          `json:"pod"`
	PodUID             string          `json:"podUID"`
	Owners             []TrainingOwner `json:"owners"`
	OwnerUnknownReason string          `json:"ownerUnknownReason,omitempty"`
}

// AffectedJob groups the workers of a verified controller in one namespace.
// An unresolved worker has its own entry and retains the reported owner chain.
type AffectedJob struct {
	Namespace string           `json:"namespace"`
	Owner     *TrainingOwner   `json:"owner,omitempty"`
	Workers   []TrainingWorker `json:"workers"`
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
