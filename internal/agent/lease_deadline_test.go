// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/actionjournal"
	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// deadlineDriver records the deadline the executor's context carried into
// the idle probe, and counts resets.
type deadlineDriver struct {
	nvml.GPUDriver
	mu       sync.Mutex
	deadline time.Time
	hasDL    bool
	probes   atomic.Int32
	resets   atomic.Int32
}

func (d *deadlineDriver) EnsureIdle(ctx context.Context, index int) error {
	d.probes.Add(1)
	d.mu.Lock()
	d.deadline, d.hasDL = ctx.Deadline()
	d.mu.Unlock()
	return d.GPUDriver.EnsureIdle(ctx, index)
}

func (d *deadlineDriver) ResetGPU(ctx context.Context, index int) error {
	d.resets.Add(1)
	return d.GPUDriver.ResetGPU(ctx, index)
}

// leaseHarness is an agent whose journal already holds one received action
// under a lease that expires at leaseExpiresAt, as after a restart.
func leaseHarness(t *testing.T, action types.Action, leaseExpiresAt time.Time) (*Agent, *deadlineDriver, *types.ActionResult) {
	t.Helper()
	var posted types.ActionResult
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == types.AgentActionLeasePath+"/"+action.ID+"/result":
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Errorf("decode result: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(controller.Close)
	driver := &deadlineDriver{GPUDriver: &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-test"}}}}
	a, err := New(Config{
		NodeName:          "gpu-node-1",
		ControllerURL:     controller.URL,
		AllowInsecureHTTP: true,
		SpoolPath:         t.TempDir() + "/spool.jsonl",
		ActionJournalPath: t.TempDir() + "/actions.jsonl",
	}, driver, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := a.journal.RecordReceived(action); err != nil {
		t.Fatal(err)
	}
	if _, err := a.journal.SetClaim(action.ID, "lease-1", leaseExpiresAt); err != nil {
		t.Fatal(err)
	}
	return a, driver, &posted
}

// The audit's reproducer: an action recovered with most of its lease gone
// must not get its full timeout, which would run it past the lease while the
// controller may hand it out again.
func TestRecoveredDestructiveActionWithShortLeaseIsNotStarted(t *testing.T) {
	action := types.Action{ID: "act-reset", Type: types.ActionGPUReset, Timeout: 5 * time.Minute,
		Params: map[string]string{"gpu_index": "0", "gpu_uuid": "GPU-test"}}
	a, driver, posted := leaseHarness(t, action, time.Now().Add(types.AgentResultGrace+30*time.Second))

	a.cfg.NodeLevelActions = true
	a.recoverQueuedActions(context.Background())

	if driver.resets.Load() != 0 || driver.probes.Load() != 0 {
		t.Fatalf("resets %d, probes %d: a destructive action started with 30s of a 5m lease left",
			driver.resets.Load(), driver.probes.Load())
	}
	if posted.ActionID != action.ID || posted.OK || !strings.Contains(posted.Error, "not executed") {
		t.Fatalf("posted = %+v, want a non-OK not-executed result", posted)
	}
	entry, ok := a.journal.Get(action.ID)
	if !ok || entry.State != actionjournal.StateReported || entry.Result == nil || entry.Result.OK {
		t.Fatalf("journal = %#v, want a reported known not-executed outcome (never running, never unknown)", entry)
	}
}

func TestActionWithLessThanTheMinimumWindowIsNotStarted(t *testing.T) {
	action := types.Action{ID: "act-idle", Type: types.ActionIdleCheck, Params: map[string]string{"gpu_index": "0"}}
	a, driver, posted := leaseHarness(t, action, time.Now().Add(types.AgentResultGrace+2*time.Second))

	a.cfg.NodeLevelActions = true
	a.recoverQueuedActions(context.Background())

	if driver.probes.Load() != 0 {
		t.Fatal("an action started with 2s left before its execution deadline")
	}
	if posted.OK || !strings.Contains(posted.Error, "not executed") {
		t.Fatalf("posted = %+v, want not executed", posted)
	}
}

// A live lease bounds the executor: the context it runs under ends at the
// lease expiry minus the reporting grace, not at now + the action timeout.
func TestExecutionContextEndsBeforeTheLease(t *testing.T) {
	lease := time.Now().Add(types.AgentResultGrace + 2*time.Minute)
	action := types.Action{ID: "act-bounded", Type: types.ActionIdleCheck, Timeout: time.Hour,
		Params: map[string]string{"gpu_index": "0"}}
	a, driver, posted := leaseHarness(t, action, lease)

	a.cfg.NodeLevelActions = true
	a.recoverQueuedActions(context.Background())

	if driver.probes.Load() != 1 {
		t.Fatalf("probes = %d, want the action to run once", driver.probes.Load())
	}
	driver.mu.Lock()
	deadline, has := driver.deadline, driver.hasDL
	driver.mu.Unlock()
	want := lease.Add(-types.AgentResultGrace)
	if !has || deadline.After(want) || want.Sub(deadline) > time.Second {
		t.Fatalf("executor deadline = %v (set %t), want the lease expiry minus the grace (%v), not the 1h timeout", deadline, has, want)
	}
	if !posted.OK {
		t.Fatalf("posted = %+v, want the successful result reported inside the lease", posted)
	}
}

func TestRecoveredActionRefusedWhenNodeLevelActionsDisabled(t *testing.T) {
	action := types.Action{ID: "disabled", Type: types.ActionIdleCheck, Params: map[string]string{"gpu_index": "0"}}
	a, driver, posted := leaseHarness(t, action, time.Now().Add(5*time.Minute))
	a.recoverQueuedActions(context.Background())
	if driver.probes.Load() != 0 || posted.ActionID != action.ID || posted.OK || !strings.Contains(posted.Error, "node-level recovery is disabled") {
		t.Fatalf("probes = %d, posted = %+v", driver.probes.Load(), posted)
	}
}
