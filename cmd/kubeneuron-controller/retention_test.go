// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"
)

// retentionRun drives runRetention with channels the test controls and reports
// each prune on pruned.
func retentionRun(t *testing.T, leading bool) (firstPass chan struct{}, tick chan time.Time, pruned chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	firstPass, tick, pruned = make(chan struct{}), make(chan time.Time), make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runRetention(ctx, firstPass, tick, func() bool { return leading }, func(context.Context) { pruned <- struct{}{} })
	}()
	t.Cleanup(func() { cancel(); <-done })
	return firstPass, tick, pruned
}

func expectPrune(t *testing.T, pruned chan struct{}, want bool, why string) {
	t.Helper()
	select {
	case <-pruned:
		if !want {
			t.Fatalf("retention pruned %s", why)
		}
	case <-time.After(100 * time.Millisecond):
		if want {
			t.Fatalf("retention did not prune %s", why)
		}
	}
}

// Retention must not prune before the janitors' first pass: a resolved
// incident's cordon the janitor has not yet released would lose the only row
// that says it may be released.
func TestRetentionWaitsForTheFirstJanitorPass(t *testing.T) {
	firstPass, tick, pruned := retentionRun(t, true)
	expectPrune(t, pruned, false, "before the janitors' first pass")
	close(firstPass)
	expectPrune(t, pruned, true, "after the janitors' first pass")
	tick <- time.Now()
	expectPrune(t, pruned, true, "on the next tick")
}

// A janitor pass that never completes must not stop retention for good.
func TestRetentionStartsOnTheFirstTickWithoutAJanitorPass(t *testing.T) {
	_, tick, pruned := retentionRun(t, true)
	tick <- time.Now()
	expectPrune(t, pruned, true, "on the first tick with no janitor pass")
}

func TestRetentionPrunesOnlyWhileLeading(t *testing.T) {
	firstPass, tick, pruned := retentionRun(t, false)
	close(firstPass)
	tick <- time.Now()
	expectPrune(t, pruned, false, "on a replica that does not lead")
}
