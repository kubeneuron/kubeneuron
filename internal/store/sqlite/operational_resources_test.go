package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestOperationalResourcesAreVersionedIdempotentAndAuditable(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	resource := &types.OperationalResource{
		Kind:    types.ResourceCandidateConfiguration,
		ID:      "candidate-1",
		State:   "uploaded",
		Tenant:  "tenant-a",
		Cluster: "cluster-a",
		Actor:   "alice",
		Payload: []byte(`{"api_version":"v1"}`),
	}
	if err := s.CreateOperationalResource(ctx, resource); err != nil {
		t.Fatal(err)
	}
	if resource.Version != 1 || resource.CreatedAt.IsZero() {
		t.Fatalf("created resource = %#v", resource)
	}

	stored, err := s.GetOperationalResource(ctx, resource.Kind, resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.State = "previewed"
	stored.Payload = []byte(`{"api_version":"v1","previewed":true}`)
	if err := s.UpdateOperationalResource(ctx, stored, 1); err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 {
		t.Fatalf("version after update = %d, want 2", stored.Version)
	}
	if err := s.UpdateOperationalResource(ctx, stored, 1); !errors.Is(err, store.ErrOperationalConflict) {
		t.Fatalf("stale update error = %v, want operational conflict", err)
	}

	record := &types.OperationalIdempotencyRecord{
		Kind: types.ResourceCandidateConfiguration, Actor: "alice", Key: "req-1",
		RequestDigest: "sha256:request", ResourceID: resource.ID,
	}
	got, created, err := s.PutOperationalIdempotency(ctx, record)
	if err != nil || !created || got.ResourceID != resource.ID {
		t.Fatalf("first idempotency = %#v created=%v err=%v", got, created, err)
	}
	got, created, err = s.PutOperationalIdempotency(ctx, record)
	if err != nil || created || got.RequestDigest != record.RequestDigest {
		t.Fatalf("replayed idempotency = %#v created=%v err=%v", got, created, err)
	}

	first := &types.OperationalAuditEvent{
		Kind: resource.Kind, ResourceID: resource.ID, Actor: "alice", Action: "upload",
		Time: time.Now(), Params: map[string]string{"source": "api"},
	}
	if err := s.AppendOperationalAudit(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := &types.OperationalAuditEvent{
		Kind: resource.Kind, ResourceID: resource.ID, Actor: "bob", Action: "preview",
		Time: time.Now(),
	}
	if err := s.AppendOperationalAudit(ctx, second); err != nil {
		t.Fatal(err)
	}
	if second.PrevHash != first.Hash || second.Hash == "" {
		t.Fatalf("audit chain first=%#v second=%#v", first, second)
	}
	if first.Tenant != resource.Tenant || first.Cluster != resource.Cluster || second.Tenant != resource.Tenant || second.Cluster != resource.Cluster {
		t.Fatalf("audit scope was not inherited from resource: first=%#v second=%#v", first, second)
	}
	events, err := s.ListOperationalAudit(ctx, resource.Kind, resource.ID, 10)
	if err != nil || len(events) != 2 || events[1].PrevHash != events[0].Hash {
		t.Fatalf("audit events=%#v err=%v", events, err)
	}
	page, err := s.ListOperationalAuditEvents(ctx, types.OperationalAuditFilter{Actor: "bob", AfterID: first.ID, Limit: 10})
	if err != nil || len(page) != 1 || page[0].ID != second.ID || page[0].Action != "preview" {
		t.Fatalf("filtered audit page=%#v err=%v", page, err)
	}
	scoped, err := s.ListOperationalAuditEvents(ctx, types.OperationalAuditFilter{Tenant: "tenant-a", Cluster: "cluster-a", Limit: 10})
	if err != nil || len(scoped) != 2 || scoped[0].Tenant != "tenant-a" || scoped[1].Cluster != "cluster-a" {
		t.Fatalf("scoped audit page=%#v err=%v", scoped, err)
	}
	otherScope, err := s.ListOperationalAuditEvents(ctx, types.OperationalAuditFilter{Tenant: "tenant-other", Limit: 10})
	if err != nil || len(otherScope) != 0 {
		t.Fatalf("cross-tenant audit page=%#v err=%v", otherScope, err)
	}

	resources, err := s.ListOperationalResources(ctx, types.OperationalResourceFilter{Kind: resource.Kind})
	if err != nil || len(resources) != 1 || resources[0].State != "previewed" {
		t.Fatalf("resources=%#v err=%v", resources, err)
	}
}
