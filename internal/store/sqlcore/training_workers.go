// SPDX-License-Identifier: Apache-2.0

package sqlcore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// SaveTrainingWorkers keeps the first successful observation with the incident.
func (q *Queries) SaveTrainingWorkers(ctx context.Context, id string, workers []types.TrainingWorker) error {
	if workers == nil {
		return errors.New("missing training worker observation")
	}
	payload, err := json.Marshal(workers)
	if err != nil {
		return err
	}
	_, err = q.db.ExecContext(ctx, "UPDATE incidents SET training_workers=? WHERE id=? AND training_workers IS NULL", string(payload), id)
	return err
}

// TrainingWorkers returns nil when there is no observation and an empty slice
// when a successful listing found no workers.
func (q *Queries) TrainingWorkers(ctx context.Context, id string) ([]types.TrainingWorker, error) {
	var value sql.NullString
	err := q.db.QueryRowContext(ctx, "SELECT training_workers FROM incidents WHERE id=?", id).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil || !value.Valid {
		return nil, err
	}
	var workers []types.TrainingWorker
	err = json.Unmarshal([]byte(value.String), &workers)
	return workers, err
}

// SaveTrainingProgress retains the highest reported step for this worker attempt.
// Duplicate and out-of-order reports cannot refresh the evidence of progress.
func (q *Queries) SaveTrainingProgress(ctx context.Context, incidentID, jobUID, podUID string, step int64, received time.Time) error {
	if incidentID == "" || jobUID == "" || podUID == "" || step < 0 || received.IsZero() {
		return errors.New("invalid training progress")
	}
	trail, err := q.AuditTrail(ctx, incidentID)
	if err != nil {
		return err
	}
	var ready, confirmed *string
	if at := types.VerifiedInfrastructureTime(trail); at != nil && received.After(*at) {
		r, c := ts(*at), ts(received)
		ready, confirmed = &r, &c
	}
	_, err = q.db.ExecContext(ctx, `INSERT INTO training_progress (incident_id,job_uid,pod_uid,step,received_at,ready_at,confirmed_at)
		VALUES (?,?,?,?,?,?,?) ON CONFLICT (incident_id,job_uid,pod_uid) DO UPDATE
		SET step=excluded.step, received_at=excluded.received_at,
		confirmed_at=CASE WHEN excluded.ready_at IS NOT NULL AND
		    (training_progress.ready_at IS NULL OR training_progress.ready_at <> excluded.ready_at)
		    THEN excluded.confirmed_at ELSE training_progress.confirmed_at END,
		ready_at=COALESCE(excluded.ready_at, training_progress.ready_at)
		WHERE excluded.step > training_progress.step`, incidentID, jobUID, podUID, step, ts(received), ready, confirmed)
	return err
}

// SaveTrainingReport records a verified worker's statement against its incident.
// Checkpoint and resume each keep their first receipt for that checkpoint ID.
func (q *Queries) SaveTrainingReport(ctx context.Context, incidentID, jobUID, podUID string, report checkpoint.TrainingReport, received time.Time) error {
	if err := report.Validate(); err != nil {
		return err
	}
	if report.Kind == "progress" {
		return q.SaveTrainingProgress(ctx, incidentID, jobUID, podUID, *report.Step, received)
	}
	if incidentID == "" || jobUID == "" || podUID == "" || received.IsZero() {
		return errors.New("missing training report identity or receive time")
	}
	_, err := q.db.ExecContext(ctx, `INSERT INTO training_checkpoint_facts
		(incident_id, job_uid, pod_uid, kind, checkpoint_id, step, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (incident_id, job_uid, pod_uid, kind, checkpoint_id) DO NOTHING`,
		incidentID, jobUID, podUID, report.Kind, report.CheckpointID, report.Step, ts(received))
	return err
}

// RecordAffectedJobReport links a report only to a verified root in the
// immutable allocation. A replacement worker can report from a different node.
func (q *Queries) RecordAffectedJobReport(ctx context.Context, incidentID, namespace, ownerUID, podUID string, report checkpoint.TrainingReport, received time.Time) (bool, error) {
	workers, err := q.TrainingWorkers(ctx, incidentID)
	if err != nil {
		return false, err
	}
	for _, worker := range workers {
		if namespace == "" || worker.Namespace != namespace || worker.OwnerUnknownReason != "" || len(worker.Owners) == 0 {
			continue
		}
		root := worker.Owners[len(worker.Owners)-1]
		if ownerUID != "" && root.UID == ownerUID {
			err := q.SaveTrainingReport(ctx, incidentID, ownerUID, podUID, report, received)
			return err == nil, err
		}
	}
	return false, nil
}

// TrainingConfirmations returns the earliest accepted post-recovery progress per job.
func (q *Queries) TrainingConfirmations(ctx context.Context, id string, ready time.Time) (map[string]time.Time, error) {
	rows, err := q.db.QueryContext(ctx, "SELECT job_uid,confirmed_at FROM training_progress WHERE incident_id=? AND ready_at=?", id, ts(ready))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]time.Time)
	for rows.Next() {
		var uid, stamp string
		if err := rows.Scan(&uid, &stamp); err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return nil, err
		}
		if first, ok := out[uid]; !ok || at.Before(first) {
			out[uid] = at
		}
	}
	return out, rows.Err()
}
