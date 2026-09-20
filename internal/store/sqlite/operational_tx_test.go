package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// WithOperationalTx commits an idempotency claim, a resource write, and
// chained audit appends together, and discards all of them together when the
// callback fails, including restoring the audit head so the next committed
// event chains onto the last committed one rather than a discarded child.
func TestWithOperationalTxCommitsOrRollsBackKeyResourceAndAuditTogether(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	kind := types.ResourceRuntimeContractQualification
	key := func(k string) *types.OperationalIdempotencyRecord {
		return &types.OperationalIdempotencyRecord{Kind: kind, Actor: "alice", Key: k, RequestDigest: "sha256:req", ResourceID: "rcq-1"}
	}
	event := func(action string) *types.OperationalAuditEvent {
		return &types.OperationalAuditEvent{Kind: kind, ResourceID: "rcq-1", Actor: "alice", Action: action, Result: "ok"}
	}

	if err := s.WithOperationalTx(ctx, nil); err == nil {
		t.Fatal("nil callback must be rejected")
	}

	// Commit: every write lands, the two appends chain, and a read through the
	// transaction sees the transaction's own insert.
	err = s.WithOperationalTx(ctx, func(tx store.OperationalTx) error {
		if _, created, err := tx.PutOperationalIdempotency(ctx, key("create:k1")); err != nil || !created {
			return fmt.Errorf("put key = (%v, %v)", created, err)
		}
		if err := tx.CreateOperationalResource(ctx, &types.OperationalResource{Kind: kind, ID: "rcq-1", State: "Observing", Tenant: "team-a", Cluster: "east", Actor: "alice", Payload: []byte(`{"v":1}`)}); err != nil {
			return err
		}
		if seen, err := tx.GetOperationalResource(ctx, kind, "rcq-1"); err != nil || seen.Version != 1 {
			return fmt.Errorf("read of own insert = (%#v, %v)", seen, err)
		}
		if err := tx.AppendOperationalAudit(ctx, event("create")); err != nil {
			return err
		}
		return tx.AppendOperationalAudit(ctx, event("observe"))
	})
	if err != nil {
		t.Fatalf("commit transaction: %v", err)
	}
	if _, err := s.GetOperationalIdempotency(ctx, kind, "alice", "create:k1"); err != nil {
		t.Fatalf("committed key: %v", err)
	}
	committed, err := s.GetOperationalResource(ctx, kind, "rcq-1")
	if err != nil || committed.Version != 1 {
		t.Fatalf("committed resource = (%#v, %v)", committed, err)
	}
	events, err := s.ListOperationalAudit(ctx, kind, "rcq-1", 10)
	if err != nil || len(events) != 2 || events[0].PrevHash != "" || events[1].PrevHash != events[0].Hash {
		t.Fatalf("committed chain = (%#v, %v), want two chained events", events, err)
	}
	// The audit scope was resolved from the envelope inserted in the same
	// transaction, so the explorer's tenant filter applies to both events.
	if scoped, err := s.ListOperationalAuditEvents(ctx, types.OperationalAuditFilter{Kind: kind, Tenant: "team-a"}); err != nil || len(scoped) != 2 {
		t.Fatalf("scoped audit = (%d, %v), want both events", len(scoped), err)
	}
	head := events[1].Hash

	// Rollback: the callback's writes all execute, then it fails. The error
	// passes through unchanged, and nothing it wrote survives.
	boom := errors.New("callback failed")
	err = s.WithOperationalTx(ctx, func(tx store.OperationalTx) error {
		if _, created, err := tx.PutOperationalIdempotency(ctx, key("observe:k2")); err != nil || !created {
			return fmt.Errorf("put key = (%v, %v)", created, err)
		}
		resource, err := tx.GetOperationalResource(ctx, kind, "rcq-1")
		if err != nil {
			return err
		}
		resource.State = "Invalidated"
		if err := tx.UpdateOperationalResource(ctx, resource, resource.Version); err != nil {
			return err
		}
		if resource.Version != 2 {
			return fmt.Errorf("update inside tx bumped version to %d", resource.Version)
		}
		if err := tx.AppendOperationalAudit(ctx, event("invalidate")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("rolled back transaction error = %v, want the callback error", err)
	}
	if _, err := s.GetOperationalIdempotency(ctx, kind, "alice", "observe:k2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("key after rollback = %v, want ErrNotFound", err)
	}
	after, err := s.GetOperationalResource(ctx, kind, "rcq-1")
	if err != nil || after.Version != 1 || after.State != "Observing" {
		t.Fatalf("resource after rollback = (%#v, %v), want version 1 Observing", after, err)
	}
	if events, err = s.ListOperationalAudit(ctx, kind, "rcq-1", 10); err != nil || len(events) != 2 {
		t.Fatalf("audit after rollback = (%d, %v), want the two committed events only", len(events), err)
	}
	// The head was restored: the next committed append chains onto the last
	// committed event, and a stale expectation of the discarded head is a
	// conflict.
	stale := event("later")
	stale.PrevHash = "sha256:discarded"
	if err := s.AppendOperationalAudit(ctx, stale); !errors.Is(err, store.ErrOperationalConflict) {
		t.Fatalf("append against a discarded head = %v, want ErrOperationalConflict", err)
	}
	next := event("later")
	if err := s.AppendOperationalAudit(ctx, next); err != nil || next.PrevHash != head {
		t.Fatalf("append after rollback = (prev %q, %v), want chained onto %q", next.PrevHash, err, head)
	}

	// A version conflict inside the transaction rolls the key claim back with
	// it, so the same key is free for a retry that read the current version.
	err = s.WithOperationalTx(ctx, func(tx store.OperationalTx) error {
		if _, created, err := tx.PutOperationalIdempotency(ctx, key("observe:k3")); err != nil || !created {
			return fmt.Errorf("put key = (%v, %v)", created, err)
		}
		return tx.UpdateOperationalResource(ctx, &types.OperationalResource{Kind: kind, ID: "rcq-1", State: "Observing", Payload: []byte(`{"v":2}`)}, 7)
	})
	if !errors.Is(err, store.ErrOperationalConflict) {
		t.Fatalf("stale update inside tx = %v, want ErrOperationalConflict", err)
	}
	if _, err := s.GetOperationalIdempotency(ctx, kind, "alice", "observe:k3"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("key after conflict = %v, want ErrNotFound", err)
	}
	err = s.WithOperationalTx(ctx, func(tx store.OperationalTx) error {
		if _, created, err := tx.PutOperationalIdempotency(ctx, key("observe:k3")); err != nil || !created {
			return fmt.Errorf("retry put key = (%v, %v), want a fresh claim", created, err)
		}
		return tx.UpdateOperationalResource(ctx, &types.OperationalResource{Kind: kind, ID: "rcq-1", State: "Observing", Payload: []byte(`{"v":2}`)}, 1)
	})
	if err != nil {
		t.Fatalf("retry with the current version: %v", err)
	}
	if final, err := s.GetOperationalResource(ctx, kind, "rcq-1"); err != nil || final.Version != 2 {
		t.Fatalf("resource after retry = (%#v, %v), want version 2", final, err)
	}
	// A replay inside a transaction sees the committed claim.
	err = s.WithOperationalTx(ctx, func(tx store.OperationalTx) error {
		existing, created, err := tx.PutOperationalIdempotency(ctx, key("observe:k3"))
		if err != nil || created || existing.ResourceID != "rcq-1" {
			return fmt.Errorf("replay put = (%#v, %v, %v)", existing, created, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
