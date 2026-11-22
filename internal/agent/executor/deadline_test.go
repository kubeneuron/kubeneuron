// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// stallingDriver's idle preflight outlives the action's deadline, the way a
// wedged nvidia-smi or a slow procfs scan can. It returns success anyway, so
// only the deadline check stands between it and the reset.
type stallingDriver struct {
	*holdingDriver
	stall time.Duration
}

func (d *stallingDriver) EnsureIdle(ctx context.Context, index int) error {
	time.Sleep(d.stall)
	return nil
}

func TestStalledPreflightDoesNotLeadToALateReset(t *testing.T) {
	driver := &stallingDriver{
		holdingDriver: &holdingDriver{Fake: &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-0"}}}},
		stall:         80 * time.Millisecond,
	}
	e := destructiveExecutor(t, driver)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := e.dispatch(ctx, types.Action{
		ID: "late", Type: types.ActionGPUReset, Params: map[string]string{"gpu_index": "0", "gpu_uuid": "GPU-0"},
	}, &types.ActionResult{})
	if !errors.Is(err, ErrDeadlineBeforeSideEffect) {
		t.Fatalf("reset after a stalled preflight = %v, want ErrDeadlineBeforeSideEffect", err)
	}
	if driver.reset != 0 {
		t.Fatalf("the reset ran %d time(s) after the deadline passed", driver.reset)
	}
}

func TestRebootAndScriptsRefuseAfterTheDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := beforeSideEffect(ctx); !errors.Is(err, ErrDeadlineBeforeSideEffect) {
		t.Fatalf("beforeSideEffect(expired) = %v", err)
	}
	if err := beforeSideEffect(context.Background()); err != nil {
		t.Fatalf("beforeSideEffect(live) = %v", err)
	}
}
