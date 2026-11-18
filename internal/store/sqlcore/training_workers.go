// SPDX-License-Identifier: Apache-2.0

package sqlcore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

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
	_, err := q.db.ExecContext(ctx, `INSERT INTO training_progress (incident_id,job_uid,pod_uid,step,received_at)
		VALUES (?,?,?,?,?) ON CONFLICT (incident_id,job_uid,pod_uid) DO UPDATE
		SET step=excluded.step, received_at=excluded.received_at
		WHERE excluded.step > training_progress.step`, incidentID, jobUID, podUID, step, ts(received))
	return err
}
