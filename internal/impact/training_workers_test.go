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
