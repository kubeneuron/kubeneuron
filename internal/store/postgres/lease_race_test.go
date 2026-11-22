// SPDX-License-Identifier: Apache-2.0

package postgres

// A8 (audit #22): one live lease per node. On PostgreSQL the claim's "no
// lease is held on this node" check runs in the statement's snapshot, so two
// claimers that pick DIFFERENT rows of one node (the restorative claim and
// the plain claim, for example) both saw no lease and both won.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestConcurrentClaimsOfDifferentActionsLeaseOneNodeOnce(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for round := 0; round < 40; round++ {
		if _, err := s.SQL.Exec("TRUNCATE TABLE actions CASCADE"); err != nil {
			t.Fatal(err)
		}
		// The oldest action is a reset; the restorative claim skips it and
		// picks the restore, so the two claimers lock different rows.
		if err := s.EnqueueAction(ctx, "node-a", types.Action{ID: "reset", Type: types.ActionGPUReset, Timeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
		if err := s.EnqueueAction(ctx, "node-a", types.Action{ID: "restore", Type: types.ActionRestoreAcceleratorHost, Timeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		won := 0
		start := make(chan struct{})
		claim := func(f func(context.Context, string, string, time.Duration) (*types.QueuedAction, error)) {
			defer wg.Done()
			<-start
			if _, err := f(ctx, "node-a", "boot-x", time.Minute); err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					t.Errorf("claim: %v", err)
				}
				return
			}
			mu.Lock()
			won++
			mu.Unlock()
		}
		for i := 0; i < 4; i++ {
			wg.Add(2)
			go claim(s.ClaimNextAction)
			go claim(s.ClaimNextRestorativeAction)
		}
		close(start)
		wg.Wait()
		var live int
		if err := s.SQL.QueryRow(`SELECT count(*) FROM actions WHERE node='node-a' AND state='leased' AND lease_expires_at_ns > $1`, time.Now().UnixNano()).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != 1 || won != 1 {
			t.Fatalf("round %d: %d live leases and %d winners on one node, want exactly 1", round, live, won)
		}
	}
}

// A28: the first report for a node arriving twice at once must not surface a
// raw unique violation; one wins and the other is stale or equal.
func TestConcurrentFirstAcceleratorReports(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		if _, err := s.SQL.Exec("TRUNCATE TABLE accelerator_reports CASCADE"); err != nil {
			t.Fatal(err)
		}
		base := time.Now()
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				errs <- s.UpsertAcceleratorReport(ctx, &types.AgentAcceleratorReport{
					Node: "node-a", Vendor: types.AcceleratorVendorNVIDIA,
					ObservedAt: base.Add(time.Duration(i) * time.Millisecond), TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
					Readiness: types.AcceleratorReadinessReady, DriverVersion: "580.1", RuntimeVersion: "dcgm-3.3",
					Devices: []types.AgentAcceleratorDevice{{ID: "GPU-1", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU}},
				})
			}(i)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil && !errors.Is(err, store.ErrStaleAcceleratorReport) {
				t.Fatalf("round %d: concurrent first report: %v", round, err)
			}
		}
		got, err := s.GetAcceleratorReport(ctx, "node-a", types.AcceleratorVendorNVIDIA)
		if err != nil {
			t.Fatal(err)
		}
		if want := base.Add(7 * time.Millisecond); got.ObservedAt.UnixNano() != want.UnixNano() {
			t.Fatalf("round %d: kept report observed at %v, want the newest %v", round, got.ObservedAt, want)
		}
	}
}
