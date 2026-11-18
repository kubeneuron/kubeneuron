// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type trainingWorkerSource interface {
	TrainingWorkers(context.Context, string) ([]types.TrainingWorker, error)
}

type trainingWorkerStore interface {
	SaveTrainingWorkers(context.Context, string, []types.TrainingWorker) error
}

// captureTrainingWorkers records the allocation before the first recovery step.
// Discovery failure leaves the observation unknown. Kubernetes errors may
// contain workload names, so keep their detail at debug level.
func (c *Controller) captureTrainingWorkers(ctx context.Context, inc *types.Incident) error {
	source, ok := c.platform.(trainingWorkerSource)
	if !ok {
		return nil
	}
	target, ok := c.store.(trainingWorkerStore)
	if !ok {
		return nil
	}
	lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	workers, err := source.TrainingWorkers(lookup, inc.Target.Node)
	if err != nil {
		c.log.Debug("training worker discovery unavailable", "incident", inc.ID, "err", err)
		return nil
	}
	return target.SaveTrainingWorkers(ctx, inc.ID, workers)
}

// IncidentTrainingWorkers reads the original allocation even after the pods disappear.
func (c *Controller) IncidentTrainingWorkers(ctx context.Context, id string) ([]types.TrainingWorker, error) {
	reader, ok := c.store.(interface {
		TrainingWorkers(context.Context, string) ([]types.TrainingWorker, error)
	})
	if !ok {
		return nil, nil
	}
	return reader.TrainingWorkers(ctx, id)
}
