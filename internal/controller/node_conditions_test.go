// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// An unexpected platform mutation would call the nil embedded implementation.
type diskPressureNode struct{ platform.Platform }

func (diskPressureNode) NodeConditions(context.Context, string) (*types.NodeConditionSnapshot, error) {
	return &types.NodeConditionSnapshot{ObservedAt: time.Now(), Conditions: []types.NodeConditionObservation{{Type: "DiskPressure", Status: "True"}}}, nil
}
func TestDiskPressureContextCannotEscalateObserveOnlyIncident(t *testing.T) {
	c, st := newIngestTestController(t)
	c.platform = diskPressureNode{}
	ctx := t.Context()
	if err := c.ingest(ctx, signal(types.ClassAgentDown, "node", "")); err != nil {
		t.Fatal(err)
	}
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil || len(incidents) != 1 {
		t.Fatalf("incidents=%v err=%v", incidents, err)
	}
	inc := incidents[0]
	if err := c.advance(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if inc.State != types.StateObserving {
		t.Fatalf("state %s", inc.State)
	}
	_, audit, err := c.IncidentDetail(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	captured := map[string]bool{}
	for _, entry := range audit {
		if entry.Params["node_conditions"] != "" {
			captured[entry.Action] = true
		}
	}
	if !captured["node-context"] || !captured["observe-only"] {
		t.Fatalf("missing observations: %v", captured)
	}
}
