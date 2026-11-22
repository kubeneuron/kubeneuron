// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestTrainingRecoveryOutput(t *testing.T) {
	ready := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	progress := ready.Add(time.Minute)
	job := types.AffectedJob{Namespace: "ml", Owner: &types.TrainingOwner{Kind: "Job", Name: "trainer"}, Recovery: types.TrainingRecoverySnapshot{InfrastructureReadyAt: &ready, ProgressStatus: "not_confirmed"}}
	var out bytes.Buffer
	printTrainingRecovery(&out, []types.AffectedJob{job})
	if !strings.Contains(out.String(), "Infrastructure ready: 2026-01-01T00:00:00Z\n  Training progress: not confirmed") {
		t.Fatal(out.String())
	}
	job.Recovery.ProgressConfirmedAt = &progress
	out.Reset()
	printTrainingRecovery(&out, []types.AffectedJob{job})
	if !strings.Contains(out.String(), "Training progress: confirmed at 2026-01-01T00:01:00Z") {
		t.Fatal(out.String())
	}
}
