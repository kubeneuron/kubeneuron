// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"testing"
)

type workerListing struct {
	platform.Platform
	workers []types.TrainingWorker
}

func (p *workerListing) TrainingWorkers(context.Context, string) ([]types.TrainingWorker, error) {
	return p.workers, nil
}

func TestIncidentCapturesWorkersBeforeLeavingOpen(t *testing.T) {
	c, st := newIngestTestController(t)
	ctx := t.Context()
	if err := c.ingest(ctx, signal(types.ClassFellOffBus, "n1", "GPU-1")); err != nil {
		t.Fatal(err)
	}
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil || len(incidents) != 1 {
		t.Fatalf("incidents %v: %v", incidents, err)
	}
	source := &workerListing{workers: []types.TrainingWorker{{PodUID: "original", OwnerUnknownReason: "unsupported-owner"}}}
	c.platform = source
	if err := c.advance(ctx, incidents[0]); err != nil {
		t.Fatal(err)
	}
	source.workers = nil
	got, err := c.IncidentTrainingWorkers(ctx, incidents[0].ID)
	if err != nil || len(got) != 1 || got[0].PodUID != "original" {
		t.Fatalf("stored workers %v: %v", got, err)
	}
}
