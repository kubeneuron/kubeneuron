// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func printTrainingRecovery(out io.Writer, jobs []types.AffectedJob) {
	if jobs == nil {
		_, _ = fmt.Fprintln(out, "Affected training jobs: unknown")
		return
	}
	if len(jobs) == 0 {
		_, _ = fmt.Fprintln(out, "Affected training jobs: none observed")
		return
	}
	for _, job := range jobs {
		name := "unknown owner"
		if job.Owner != nil {
			name = job.Owner.Kind + "/" + job.Owner.Name
		}
		_, _ = fmt.Fprintf(out, "Training job %s (%s, %d observed workers)\n", name, job.Namespace, len(job.Workers))
		ready := "unknown"
		if at := job.Recovery.InfrastructureReadyAt; at != nil {
			ready = at.Format(time.RFC3339)
		}
		progress := strings.ReplaceAll(job.Recovery.ProgressStatus, "_", " ")
		if progress == "" {
			progress = "unknown"
		}
		if at := job.Recovery.ProgressConfirmedAt; at != nil {
			progress = "confirmed at " + at.Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(out, "  Infrastructure ready: %s\n  Training progress: %s\n", ready, progress)
	}
}
