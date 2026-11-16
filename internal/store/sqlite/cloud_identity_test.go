// SPDX-License-Identifier: Apache-2.0
package sqlite

import (
	"context"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"testing"
	"time"
)

func TestCloudIdentitySurvivesIncidentUpdates(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
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
}
