// SPDX-License-Identifier: Apache-2.0

package impact

import (
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestPreviewDigestBindsAffectedWorkerIdentity(t *testing.T) {
	in := Input{TrainingWorkers: []types.TrainingWorker{{Namespace: "training", Pod: "trainer", PodUID: "old"}}}
	before := Compute(in)
	in.TrainingWorkers[0].PodUID = "replacement"
	after := Compute(in)
	if before.Digest == after.Digest || len(before.AffectedJobs) != 1 || before.AffectedJobs[0].Workers[0].PodUID != "old" {
		t.Fatalf("original worker missing or digest unchanged")
	}
}

func TestUnreadableJobMapPreservesApprovalPreview(t *testing.T) {
	in := Input{Round: 1, AffectedJobsUnavailable: "SnapshotReadFailed"}
	first := Compute(in)
	if first.Unavailable != nil || first.AffectedJobs != nil || first.AffectedJobsUnavailable != in.AffectedJobsUnavailable || first.Digest == "" || Compute(in).Digest != first.Digest {
		t.Fatalf("unexpected preview: %+v", first)
	}
	in.Round++
	if Compute(in).Digest == first.Digest {
		t.Fatal("digest did not bind approval round")
	}
}

func TestPreviewBindsCheckpointAge(t *testing.T) {
	at := time.Now().UTC()
	in := Input{ComputedAt: at, TrainingWorkers: []types.TrainingWorker{{Owners: []types.TrainingOwner{{UID: "job"}}}}}
	before := Compute(in)
	in.CheckpointReceipts = map[string]time.Time{"job": at.Add(-2 * time.Minute)}
	after := Compute(in)
	got := after.AffectedJobs[0].LastCheckpoint
	if before.AffectedJobs[0].LastCheckpoint != nil || before.Digest == after.Digest || got == nil || got.AgeSeconds != 120 || !got.ReceivedAt.Equal(in.CheckpointReceipts["job"]) {
		t.Fatalf("unexpected checkpoint receipt: %+v", got)
	}
}
