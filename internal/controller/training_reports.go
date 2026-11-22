// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"sync"
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
	routes, err := c.trainingDestinations(ctx, received)
	if err != nil {
		return err
	}
	for _, inc := range routes[trainingJobKey{pod.Namespace, owner.UID}] {
		if inc.ResolvedAt != nil && inc.ResolvedAt.Before(received.Add(-24*time.Hour)) {
			continue
		}
		_, err := writer.RecordAffectedJobReport(ctx, inc.ID, pod.Namespace, owner.UID, string(pod.UID), report, received)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

type trainingJobKey struct{ namespace, uid string }

type trainingRouteCache struct {
	sync.Mutex
	until  time.Time
	routes map[trainingJobKey][]*types.Incident
}

func (c *Controller) trainingDestinations(ctx context.Context, received time.Time) (map[trainingJobKey][]*types.Incident, error) {
	cache := &c.trainingRoutes
	cache.Lock()
	defer cache.Unlock()
	now := time.Now()
	if now.Before(cache.until) {
		return cache.routes, nil
	}
	routes := make(map[trainingJobKey][]*types.Incident)
	cutoff := received.Add(-24 * time.Hour)
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{ActiveSince: cutoff})
	if err != nil {
		return nil, err
	}
	for _, inc := range incidents {
		if inc.State == types.StateExpired || inc.State == types.StateSuperseded {
			continue
		}
		if inc.State == types.StateResolved && (inc.ResolvedAt == nil || inc.ResolvedAt.Before(cutoff)) {
			continue
		}
		workers, err := c.IncidentTrainingWorkers(ctx, inc.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, job := range types.GroupAffectedJobs(workers) {
			if job.Owner == nil {
				continue
			}
			key := trainingJobKey{job.Namespace, job.Owner.UID}
			routes[key] = append(routes[key], inc)
		}
	}
	cache.routes = routes
	cache.until = now.Add(5 * time.Second)
	return routes, nil
}

func (c *Controller) TrainingConfirmations(ctx context.Context, id string, ready time.Time) (map[string]time.Time, error) {
	reader, ok := c.store.(interface {
		TrainingConfirmations(context.Context, string, time.Time) (map[string]time.Time, error)
	})
	if !ok {
		return nil, errors.New("training report storage unavailable")
	}
	return reader.TrainingConfirmations(ctx, id, ready)
}
