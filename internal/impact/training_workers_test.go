// SPDX-License-Identifier: Apache-2.0

package impact

import (
	"testing"

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
