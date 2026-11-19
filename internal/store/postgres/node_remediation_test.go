// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"fmt"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestConcurrentNodeRemediationClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	ids := []string{"incident-a", "incident-b"}
	for _, id := range ids {
		if err := s.WithTx(ctx, func(tx store.Tx) error {
			inc := testIncident(id)
			inc.RepairNodeUID = "uid-a"
			if id == ids[1] {
				inc.Class = types.ClassFellOffBus
			}
			return tx.CreateIncident(ctx, inc)
		}); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		owner string
		err   error
	}
	results := make(chan result, len(ids))
	start := make(chan struct{})
	for _, id := range ids {
		go func(id string) {
			<-start
			var owner string
			err := s.WithTx(ctx, func(tx store.Tx) error {
				inc, err := tx.GetIncident(ctx, id)
				if err != nil {
					return err
				}
				if inc.RepairNodeUID != "uid-a" {
					return fmt.Errorf("stored repair UID = %q", inc.RepairNodeUID)
				}
				owner, err = tx.ClaimNodeRemediation(ctx, "node-a", inc.RepairNodeUID, id)
				return err
			})
			results <- result{owner, err}
		}(id)
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("claim errors: %v, %v", first.err, second.err)
	}
	if first.owner != second.owner || (first.owner != ids[0] && first.owner != ids[1]) {
		t.Fatalf("unexpected owners: %q and %q", first.owner, second.owner)
	}

	follower := ids[0]
	if follower == first.owner {
		follower = ids[1]
	}
	entries, err := s.AuditTrail(ctx, follower)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Params["owner_id"] != first.owner {
		t.Fatalf("unexpected follower trail: %+v", entries)
	}
}
