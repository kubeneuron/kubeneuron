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

func TestTrainingConfirmationAfterSecondRecovery(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	const id = "twice"
	if err := s.CreateIncident(ctx, testIncident(id)); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	transition := func(from, to types.IncidentState, action string, at time.Time) {
		t.Helper()
		entry := &types.AuditEntry{IncidentID: id, Actor: "system", Action: action, Time: at, FromState: from, ToState: to}
		if action == "resolve" {
			entry.Params = map[string]string{"infrastructure_evidence": "verified_recycle_inventory"}
		}
		if err := s.AppendAudit(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	progress := func(step int64, received time.Time) {
		t.Helper()
		if err := s.SaveTrainingProgress(ctx, id, "job", "pod", step, received); err != nil {
			t.Fatal(err)
		}
	}
	assertConfirmed := func(ready, want time.Time) {
		t.Helper()
		got, err := s.TrainingConfirmations(ctx, id, ready)
		if err != nil || !got["job"].Equal(want) {
			t.Fatalf("ready=%s confirmed=%v want=%v err=%v", ready, got["job"], want, err)
		}
	}
	transition(types.StateVerifying, types.StateResolved, "resolve", base)
	progress(1, base.Add(time.Minute))
	assertConfirmed(base, base.Add(time.Minute))
	transition(types.StateResolved, types.StateOpen, "reopen", base.Add(2*time.Minute))
	progress(2, base.Add(3*time.Minute))
	next := base.Add(5 * time.Minute)
	transition(types.StateVerifying, types.StateResolved, "resolve", next)
	progress(2, next.Add(time.Minute))
	assertConfirmed(next, time.Time{})
	progress(3, next.Add(2*time.Minute))
	progress(4, next.Add(3*time.Minute))
	assertConfirmed(next, next.Add(2*time.Minute))
}
