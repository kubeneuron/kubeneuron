// SPDX-License-Identifier: Apache-2.0

package sqlcore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

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
