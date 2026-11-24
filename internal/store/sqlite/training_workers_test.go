// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlcore"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestTrainingWorkersKeepFirstObservation(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	for _, id := range []string{"occupied", "empty"} {
		impactIncident(t, s, id)
		if _, err := s.sqlDB.ExecContext(ctx, "UPDATE incidents SET node=? WHERE id=?", id, id); err != nil {
			t.Fatal(err)
		}
		if got, err := s.TrainingWorkers(ctx, id); err != nil || got != nil {
			t.Fatalf("unobserved=%v err=%v", got, err)
		}
	}
	first := []types.TrainingWorker{{Namespace: "ml", Pod: "trainer", PodUID: "uid"}}
	if err := s.SaveTrainingWorkers(ctx, "occupied", first); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"occupied", "empty"} {
		if err := s.SaveTrainingWorkers(ctx, id, []types.TrainingWorker{}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.TrainingWorkers(ctx, "occupied")
	if err != nil || len(got) != 1 || got[0].PodUID != "uid" {
		t.Fatalf("occupied=%v err=%v", got, err)
	}
	got, err = s.TrainingWorkers(ctx, "empty")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty=%v err=%v", got, err)
	}
}

func TestTrainingProgressRetryAndRetention(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := t.Context()
	impactIncident(t, s, "incident")
	start := time.Now().UTC()
	for i, step := range []int64{0, 10, 10, 3} {
		if err := s.SaveTrainingProgress(ctx, "incident", "job", "worker", step, start.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	var step int64
	var received string
	if err := s.sqlDB.QueryRowContext(ctx, "SELECT step, received_at FROM training_progress").Scan(&step, &received); err != nil {
		t.Fatal(err)
	}
	if step != 10 || received != sqlcore.TS(start.Add(time.Minute)) {
		t.Fatalf("step %d received %s", step, received)
	}
	if _, err := s.sqlDB.ExecContext(ctx, "DELETE FROM incidents WHERE id='incident'"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.sqlDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM training_progress").Scan(&count); err != nil || count != 0 {
		t.Fatalf("count %d err %v", count, err)
	}
}

func TestTrainingCheckpointReceipts(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	impactIncident(t, s, "incident")
	start := time.Now().UTC()
	for _, kind := range []string{"checkpoint", "resume"} {
		r := checkpoint.TrainingReport{Kind: kind, CheckpointID: "epoch-1"}
		for _, received := range []time.Time{start, start.Add(time.Minute)} {
			if err := s.SaveTrainingReport(ctx, "incident", "job", "pod", r, received); err != nil {
				t.Fatal(err)
			}
		}
	}
	var count int
	if err := s.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM training_checkpoint_facts WHERE received_at=? AND step IS NULL`, sqlcore.TS(start)).Scan(&count); err != nil || count != 2 {
		t.Fatalf("original receipts: %d, %v", count, err)
	}
	if _, err := s.sqlDB.ExecContext(ctx, "DELETE FROM incidents WHERE id='incident'"); err != nil {
		t.Fatal(err)
	}
	if err := s.sqlDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM training_checkpoint_facts").Scan(&count); err != nil || count != 0 {
		t.Fatalf("after deletion: %d, %v", count, err)
	}
}

func TestRecordAffectedJobReportMatchesVerifiedRoot(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	impactIncident(t, s, "incident")
	workers := []types.TrainingWorker{
		{Namespace: "ml", PodUID: "old", Owners: []types.TrainingOwner{{UID: "child"}, {UID: "root"}}},
		{Namespace: "ml", OwnerUnknownReason: "OwnerReadFailed", Owners: []types.TrainingOwner{{UID: "partial"}}},
	}
	if err := s.SaveTrainingWorkers(ctx, "incident", workers); err != nil {
		t.Fatal(err)
	}
	report := checkpoint.TrainingReport{Kind: "resume", CheckpointID: "epoch-1"}
	for _, tc := range []struct {
		namespace, uid string
		want           bool
	}{
		{"other", "root", false}, {"ml", "child", false}, {"ml", "partial", false}, {"ml", "recreated-job", false}, {"ml", "root", true},
	} {
		written, err := s.RecordAffectedJobReport(ctx, "incident", tc.namespace, tc.uid, "replacement-worker", report, time.Now())
		if err != nil || written != tc.want {
			t.Fatalf("%v: written=%v err=%v", tc, written, err)
		}
	}
	var count int
	if err := s.sqlDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM training_checkpoint_facts").Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
}

func TestTrainingConfirmationPreservesFirstAdvance(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	impactIncident(t, s, "i")
	at := time.Date(2026, 1, 2, 3, 4, 5, 123400000, time.UTC)
	if err := s.AppendAudit(ctx, &types.AuditEntry{IncidentID: "i", Time: at, Actor: "system", Action: "resolve", FromState: types.StateVerifying, ToState: types.StateResolved, Params: map[string]string{"infrastructure_evidence": "verified_recycle_inventory"}}); err != nil {
		t.Fatal(err)
	}
	for i, step := range []int64{1, 1, 2, 3} {
		if err := s.SaveTrainingProgress(ctx, "i", "job", "pod", step, at.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		results, err := s.TrainingConfirmations(ctx, "i", at)
		if err != nil {
			t.Fatal(err)
		}
		got := results["job"]
		if (i < 2 && !got.IsZero()) || (i >= 2 && !got.Equal(at.Add(2*time.Minute))) {
			t.Fatalf("report %d: confirmation %v", i, got)
		}
	}
	// rc.1 and rc.2 identify the recovery round by its original text.
	var stamp string
	err = s.SQL.QueryRowContext(ctx, "SELECT CASE WHEN ready_at=? THEN confirmed_at END FROM training_progress WHERE incident_id=?", at.Format(time.RFC3339Nano), "i").Scan(&stamp)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || !confirmed.Equal(at.Add(2*time.Minute)) {
		t.Fatalf("rollback reader got %q: %v", stamp, err)
	}

}

func TestTrainingConfirmationDistinguishesSilentAndReportingJobs(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	impactIncident(t, s, "i")
	now := time.Now().UTC()
	if err := s.SaveTrainingReport(ctx, "i", "reporting", "pod", checkpoint.TrainingReport{Kind: "checkpoint", CheckpointID: "epoch1"}, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.TrainingConfirmations(ctx, "i", now)
	if err != nil {
		t.Fatal(err)
	}
	if stamp, ok := got["reporting"]; !ok || !stamp.IsZero() {
		t.Fatalf("missing reporting job without progress: %v", got)
	}
	if _, ok := got["silent"]; ok {
		t.Fatal("silent job counted as reporting")
	}
}

func TestTrainingCheckpointsAtDecision(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	impactIncident(t, s, "incident")
	at := time.Now().UTC().Truncate(time.Second)
	for _, fact := range []struct {
		job, kind, id string
		offset        time.Duration
	}{
		{"job", "checkpoint", "earlier", -time.Minute},
		{"job", "checkpoint", "at-decision", 0},
		{"job", "checkpoint", "later", time.Nanosecond},
		{"resume-only", "resume", "restored", -time.Second},
	} {
		r := checkpoint.TrainingReport{Kind: fact.kind, CheckpointID: fact.id}
		if err := s.SaveTrainingReport(t.Context(), "incident", fact.job, "pod", r, at.Add(fact.offset)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.TrainingCheckpointsAt(t.Context(), "incident", at)
	if err != nil || len(got) != 1 || !got["job"].Equal(at) {
		t.Fatalf("checkpoint receipts at decision = %v, %v", got, err)
	}
}
