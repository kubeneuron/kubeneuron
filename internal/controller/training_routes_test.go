// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	corev1 "k8s.io/api/core/v1"
)

type routeCountingStore struct {
	store.Store
	lists, snapshots, writes, audits int
}

func (s *routeCountingStore) ListIncidents(context.Context, store.IncidentFilter) ([]*types.Incident, error) {
	s.lists++
	return []*types.Incident{{ID: "incident"}}, nil
}

func (s *routeCountingStore) TrainingWorkers(context.Context, string) ([]types.TrainingWorker, error) {
	s.snapshots++
	return []types.TrainingWorker{{Namespace: "training", Owners: []types.TrainingOwner{{UID: "job"}}}}, nil
}

func (s *routeCountingStore) RecordAffectedJobReport(context.Context, string, string, string, string, checkpoint.TrainingReport, time.Time) (bool, error) {
	s.writes++
	return true, nil
}

func (s *routeCountingStore) AuditTrail(context.Context, string) ([]*types.AuditEntry, error) {
	s.audits++
	return nil, nil
}

func TestTrainingReportUnmatchedOwnerSkipsAudit(t *testing.T) {
	s := &routeCountingStore{}
	c := Controller{store: s}
	pod := &corev1.Pod{}
	report := func(namespace, uid string) {
		t.Helper()
		pod.Namespace = namespace
		if err := c.RecordTrainingReport(t.Context(), pod, types.TrainingOwner{UID: uid}, checkpoint.TrainingReport{Kind: "progress"}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	report("training", "other-job")
	report("other-namespace", "job")
	if s.lists != 1 || s.snapshots != 1 || s.writes != 0 || s.audits != 0 {
		t.Fatalf("unmatched reports: %+v", s)
	}
	report("training", "job")
	if s.lists != 1 || s.snapshots != 1 || s.writes != 1 {
		t.Fatalf("matched report: %+v", s)
	}
}
