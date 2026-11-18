// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// RecordTrainingReport routes an authenticated report to matching incident snapshots.
// Keep accepting reports for a day after resolution so a replacement worker can resume.
func (c *Controller) RecordTrainingReport(ctx context.Context, pod *corev1.Pod, owner types.TrainingOwner, report checkpoint.TrainingReport, received time.Time) error {
	writer, ok := c.store.(interface {
		RecordAffectedJobReport(context.Context, string, string, string, string, checkpoint.TrainingReport, time.Time) (bool, error)
	})
	if !ok {
		return errors.New("training report storage unavailable")
	}
	cutoff := received.Add(-24 * time.Hour)
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{ActiveSince: cutoff})
	if err != nil {
		return err
	}
	for _, inc := range incidents {
		if inc.State == types.StateExpired || inc.State == types.StateSuperseded {
			continue
		}
		if inc.State == types.StateResolved && (inc.ResolvedAt == nil || inc.ResolvedAt.Before(cutoff)) {
			continue
		}
		_, err := writer.RecordAffectedJobReport(ctx, inc.ID, pod.Namespace, owner.UID, string(pod.UID), report, received)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}
