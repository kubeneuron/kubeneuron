// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

func TestGroupAffectedJobsPreservesUnknownWorkers(t *testing.T) {
	root := TrainingOwner{UID: "job-uid", Kind: "Job"}
	w := TrainingWorker{Namespace: "training", Owners: []TrainingOwner{root}}
	unknown := w
	unknown.OwnerUnknownReason = "owner-replaced"
	jobs := GroupAffectedJobs([]TrainingWorker{w, unknown, w, unknown})
	if len(jobs) != 3 || len(jobs[0].Workers) != 2 || jobs[0].Owner.UID != root.UID || jobs[1].Owner != nil || jobs[2].Owner != nil {
		t.Fatalf("jobs=%+v", jobs)
	}
	if GroupAffectedJobs(nil) != nil || GroupAffectedJobs([]TrainingWorker{}) == nil {
		t.Fatal("missing and empty observations must remain distinct")
	}
}

func TestTrainingRecoveryReportSnapshot(t *testing.T) {
	checkpoint, last, resumed := int64(100), int64(120), int64(105)
	var snapshot TrainingRecoverySnapshot
	snapshot.RecordCheckpointSteps(&checkpoint, &last, &resumed)
	if *snapshot.StepsAfterCheckpoint != 20 || *snapshot.ConfirmedRepeatedSteps != 5 {
		t.Fatal(snapshot)
	}
	resumed = 150
	snapshot.RecordCheckpointSteps(&checkpoint, &last, &resumed)
	if *snapshot.ConfirmedRepeatedSteps != 20 {
		t.Fatal(snapshot)
	}
	snapshot.RecordCheckpointSteps(&checkpoint, &last, nil)
	if snapshot.ConfirmedRepeatedSteps != nil || *snapshot.StepsAfterCheckpoint != 20 {
		t.Fatal(snapshot)
	}
}
