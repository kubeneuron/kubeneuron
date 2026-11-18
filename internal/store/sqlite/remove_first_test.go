// SPDX-License-Identifier: Apache-2.0
package sqlite

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestRemoveFirstPlanSurvivesAndListsWhileOpen(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	inc := &types.Incident{ID: "rf", Target: types.Target{Node: "worker"}, Class: types.ClassECCDBE, State: types.StateOpen, OpenedAt: now, UpdatedAt: now, StateChangedAt: now}
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	open := func() []*types.Incident {
		t.Helper()
		out, err := s.ListIncidents(ctx, store.IncidentFilter{OpenRemoveFirst: true})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := open(); len(got) != 0 {
		t.Fatalf("an incident without a plan lists as open: %+v", got)
	}
	got, err := s.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoveFirst != nil {
		t.Fatalf("plan = %+v, want none", got.RemoveFirst)
	}
	want := &types.RemoveFirstRecord{RemoveFirstPlan: cloud.RemoveFirstPlan{Operation: "op", Instance: "p/z/i-old", Pool: "gpu",
		Phase: cloud.RemoveFirstDeleting, PlannedAt: now, Restore: "resize", State: map[string]string{"size": "3"}}}
	got.RemoveFirst = want
	if err := s.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	listed := open()
	if len(listed) != 1 || !reflect.DeepEqual(listed[0].RemoveFirst, want) {
		t.Fatalf("open plans = %+v, want the incident with %+v", listed, want)
	}
	got = listed[0]
	got.RemoveFirst.Outcome = types.RemoveFirstRestored
	got.RemoveFirst.EndedAt = &now
	if err := s.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	if listed := open(); len(listed) != 0 {
		t.Fatalf("an ended plan still lists as open: %+v", listed)
	}
	if got, err = s.GetIncident(ctx, inc.ID); err != nil || got.RemoveFirst == nil || got.RemoveFirst.Outcome != types.RemoveFirstRestored {
		t.Fatalf("plan = %+v, %v", got, err)
	}
}
