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

func TestSuccessorSurvivesAndListsWhileOpen(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	inc := &types.Incident{ID: "succ", Target: types.Target{Node: "worker"}, Class: types.ClassECCDBE, State: types.StateOpen, OpenedAt: now, UpdatedAt: now, StateChangedAt: now}
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	open := func() []*types.Incident {
		t.Helper()
		out, err := s.ListIncidents(ctx, store.IncidentFilter{OpenSuccessor: true})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := open(); len(got) != 0 {
		t.Fatalf("an incident without a successor lists as open: %+v", got)
	}
	got, err := s.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Successor != nil {
		t.Fatalf("successor = %+v, want none", got.Successor)
	}
	want := &types.SuccessorRecord{
		Successor: cloud.Successor{Operation: "op", Instance: "i-old", Pool: "gpu", Phase: cloud.SuccessorCreated,
			RequestedAt: now, ProviderID: "aws:///z/i-new", State: map[string]string{"before": "3"}},
		Node: "worker-2", NodeUID: "uid-2", ReadyAt: &now,
	}
	got.Successor = want
	if err := s.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	listed := open()
	if len(listed) != 1 || !reflect.DeepEqual(listed[0].Successor, want) {
		t.Fatalf("open successors = %+v, want the incident with %+v", listed, want)
	}
	got = listed[0]
	got.Successor.Outcome = types.SuccessorReplaced
	got.Successor.EndedAt = &now
	if err := s.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	if listed := open(); len(listed) != 0 {
		t.Fatalf("an ended successor still lists as open: %+v", listed)
	}
	got, err = s.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Successor == nil || got.Successor.Outcome != types.SuccessorReplaced {
		t.Fatalf("successor = %+v", got.Successor)
	}
}
