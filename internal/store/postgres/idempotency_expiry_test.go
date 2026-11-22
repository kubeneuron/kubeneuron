// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// A28: a key whose previous use has expired is a new request, not a replay.
func TestExpiredIdempotencyKeyIsNotReplayed(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if _, err := s.SQL.Exec("TRUNCATE TABLE operational_idempotency"); err != nil {
		t.Fatal(err)
	}
	put := func(digest, resource string, expires time.Time) (*types.OperationalIdempotencyRecord, bool) {
		t.Helper()
		var got *types.OperationalIdempotencyRecord
		var created bool
		if err := s.WithOperationalTx(ctx, func(tx store.OperationalTx) error {
			var err error
			got, created, err = tx.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{
				Kind: types.ResourceRuntimeContractQualification, Actor: "alice", Key: "k",
				RequestDigest: digest, ResourceID: resource, ExpiresAt: expires,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got, created
	}
	if _, created := put("sha256:a", "r-1", time.Now().Add(-time.Minute)); !created {
		t.Fatal("first claim must create")
	}
	got, created := put("sha256:b", "r-2", time.Now().Add(time.Hour))
	if !created || got.ResourceID != "r-2" || got.RequestDigest != "sha256:b" {
		t.Fatalf("after expiry: created=%v record=%+v, want a fresh claim, not a replay of r-1", created, got)
	}
	again, created := put("sha256:b", "r-3", time.Now().Add(time.Hour))
	if created || again.ResourceID != "r-2" {
		t.Fatalf("unexpired key: created=%v record=%+v, want the replay of r-2", created, again)
	}
}
