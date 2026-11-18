// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestTrainingReportsBoundToIncident(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	const id = "incident"
	if err := s.CreateIncident(ctx, testIncident(id)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTrainingWorkers(ctx, id, []types.TrainingWorker{{Namespace: "ml", Owners: []types.TrainingOwner{{UID: "job"}}}}); err != nil {
		t.Fatal(err)
	}
	step := int64(17)
	now := time.Now().UTC()
	for _, kind := range []string{"checkpoint", "resume", "progress"} {
		r := checkpoint.TrainingReport{Kind: kind, CheckpointID: "epoch-1"}
		if kind == "progress" {
			r.CheckpointID = ""
			r.Step = &step
		}
		for _, received := range []time.Time{now, now.Add(time.Minute)} {
			ok, err := s.RecordAffectedJobReport(ctx, id, "ml", "job", "pod", r, received)
			if err != nil || !ok {
				t.Fatalf("%s recorded=%v, err=%v", kind, ok, err)
			}
		}
	}
	checkCounts := func(wantFacts, wantProgress int) {
		t.Helper()
		var facts, progress int
		err := s.SQL.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM training_checkpoint_facts WHERE received_at=$1 AND step IS NULL),
		(SELECT COUNT(*) FROM training_progress WHERE received_at=$1 AND step=17)`, now.Format(time.RFC3339Nano)).Scan(&facts, &progress)
		if err != nil || facts != wantFacts || progress != wantProgress {
			t.Fatalf("facts=%d progress=%d err=%v", facts, progress, err)
		}
	}
	checkCounts(2, 1)
	if _, err := s.SQL.ExecContext(ctx, "DELETE FROM incidents WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	checkCounts(0, 0)
}
