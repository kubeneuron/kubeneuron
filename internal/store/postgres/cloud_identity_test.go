// SPDX-License-Identifier: Apache-2.0
package postgres

import (
	"context"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"testing"
	"time"
)

func TestCloudIdentitySurvivesIncidentUpdates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	inc := &types.Incident{ID: "cloud-pin", Target: types.Target{Node: "worker"}, Class: types.ClassECCDBE, State: types.StateOpen, OpenedAt: now, UpdatedAt: now, StateChangedAt: now}
	pin := cloud.NodeIdentity{Node: "worker", NodeUID: "uid-original", ProviderID: "aws:///zone/i-original", InstanceID: "i-original", BootID: "boot-original"}
	inc.CloudIdentity = &pin
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CloudIdentity == nil || *got.CloudIdentity != pin {
		t.Fatalf("lost identity: %+v", got.CloudIdentity)
	}
	got.State = types.StateAwaitingApproval
	if err := s.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CloudIdentity == nil || *got.CloudIdentity != pin {
		t.Fatalf("approval update changed pin: %+v", got.CloudIdentity)
	}
	got.State = types.StateSuperseded
	if err := s.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	next := *inc
	next.ID = "cloud-pin-next"
	if err := s.CreateIncident(ctx, &next); err != nil {
		t.Fatalf("superseded target blocked new incident: %v", err)
	}
	active, err := s.GetOpenIncident(ctx, next.Target, next.Class)
	if err != nil || active.ID != next.ID {
		t.Fatalf("open incident=%+v err=%v", active, err)
	}

}
