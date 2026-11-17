// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

type repinFaultStore struct {
	store.Store
	failures int
	calls    int
	err      error
}

func (s *repinFaultStore) WithTx(ctx context.Context, fn func(store.Tx) error) error {
	s.calls++
	if s.calls <= s.failures {
		return s.err
	}
	return s.Store.WithTx(ctx, fn)
}

func TestRepinRetriesStorageFailureButNotConflict(t *testing.T) {
	for _, tc := range []struct {
		name                string
		failures, wantCalls int
		err                 error
		success             bool
	}{
		{"transient", 1, 2, errors.New("temporary storage failure"), true},
		{"bounded", 4, 3, errors.New("storage unavailable"), false},
		{"conflict", 1, 1, store.ErrConflict, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c, inc, _ := checkpointFixture(t, &changedCloudPlatform{}, checkpoint.Policy{})
			inc.CloudIdentity = recycleTestPin()
			inc.CloudIdentity.Node = inc.Target.Node
			if err := c.store.UpdateIncident(ctx, inc); err != nil {
				t.Fatal(err)
			}
			fault := &repinFaultStore{Store: c.store, failures: tc.failures, err: tc.err}
			c.store = fault
			result := cloud.RecycleResult{Before: *inc.CloudIdentity, After: *inc.CloudIdentity, CompletedAt: time.Now()}
			result.After.BootID = "new-boot"
			err := c.repinAfterRecycle(ctx, inc, result)
			if (err == nil) != tc.success || fault.calls != tc.wantCalls {
				t.Fatalf("err=%v calls=%d", err, fault.calls)
			}
			saved, err := c.store.GetIncident(ctx, inc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (saved.CloudIdentity.BootID == "new-boot") != tc.success {
				t.Fatal("unexpected persisted pin")
			}
		})
	}
}
