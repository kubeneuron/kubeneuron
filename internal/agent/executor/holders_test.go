package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// holdingDriver is a fake driver that is idle by every compute-app measure but
// still has device handles open — the situation on a stock GPU Operator node.
type holdingDriver struct {
	*nvml.Fake
	holders []nvml.DeviceHolder
	err     error
	reset   int
}

func (d *holdingDriver) DeviceHolders(int) ([]nvml.DeviceHolder, error) {
	return d.holders, d.err
}

func (d *holdingDriver) ResetGPU(ctx context.Context, index int) error {
	d.reset++
	return d.Fake.ResetGPU(ctx, index)
}

func destructiveExecutor(t *testing.T, driver nvml.GPUDriver) *Executor {
	t.Helper()
	t.Setenv(destructiveLabEnv, "1")
	return NewWithOptions(driver, Options{EnableDestructiveActions: true})
}

func TestGPUResetRefusesWhileDeviceHoldersRemain(t *testing.T) {
	driver := &holdingDriver{Fake: &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-0"}}}, holders: []nvml.DeviceHolder{
		{PID: 19146, Command: "nv-hostengine", Device: "/dev/nvidia0"},
		{PID: 11999, Command: "dcgm-exporter", Device: "/dev/nvidia0"},
	}}
	e := destructiveExecutor(t, driver)

	res := &types.ActionResult{}
	err := e.dispatch(context.Background(), types.Action{
		ID: "a1", Type: types.ActionGPUReset, Params: map[string]string{"gpu_index": "0", "gpu_uuid": "GPU-0"},
	}, res)
	if err == nil {
		t.Fatal("reset must be refused while processes hold the device")
	}
	// The point of the check is that the operator learns what to stop; NVIDIA's
	// own exit 19 text names nothing.
	for _, want := range []string{"nv-hostengine", "19146", "dcgm-exporter", "quiesce"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to mention %q", err, want)
		}
	}
	if driver.reset != 0 {
		t.Fatal("nvidia-smi --gpu-reset must not run while the device is held")
	}
}

func TestGPUResetProceedsWhenNoHoldersRemain(t *testing.T) {
	driver := &holdingDriver{Fake: &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-0"}}}}
	e := destructiveExecutor(t, driver)

	err := e.dispatch(context.Background(), types.Action{
		ID: "a2", Type: types.ActionGPUReset, Params: map[string]string{"gpu_index": "0", "gpu_uuid": "GPU-0"},
	}, &types.ActionResult{})
	if err != nil {
		t.Fatalf("reset on an unheld device = %v", err)
	}
	if driver.reset != 1 {
		t.Fatalf("reset calls = %d, want 1", driver.reset)
	}
}

func TestGPUResetFailsClosedWhenHoldersCannotBeDetermined(t *testing.T) {
	driver := &holdingDriver{Fake: &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-0"}}}, err: errors.New("procfs unavailable")}
	e := destructiveExecutor(t, driver)

	err := e.dispatch(context.Background(), types.Action{
		ID: "a3", Type: types.ActionGPUReset, Params: map[string]string{"gpu_index": "0", "gpu_uuid": "GPU-0"},
	}, &types.ActionResult{})
	if err == nil || !strings.Contains(err.Error(), "cannot determine") {
		t.Fatalf("err = %v, want a fail-closed refusal", err)
	}
	if driver.reset != 0 {
		t.Fatal("an unreadable process table must not clear a reset")
	}
}

// TestGPUResetBindsToUUIDNotStaleIndex is the regression for the reset landing
// on the wrong physical GPU. GPU indices are not stable: when the incident's
// device drops off the bus or an earlier ladder rung reloads the driver, the
// remaining GPUs renumber. A reset must follow the incident's UUID to its live
// index and fail closed on any disagreement, never trust the index captured
// when the incident opened.
func TestGPUResetBindsToUUIDNotStaleIndex(t *testing.T) {
	// UUID-X was at index 3 when the incident opened; a neighbor fell off the
	// bus and it now lives at index 5. Resetting stale index 3 would hit a
	// healthy neighbor.
	inventory := []types.GPUInfo{
		{Index: 0, UUID: "GPU-0"},
		{Index: 1, UUID: "GPU-1"},
		{Index: 2, UUID: "GPU-2"},
		{Index: 3, UUID: "GPU-neighbor"},
		{Index: 4, UUID: "GPU-4"},
		{Index: 5, UUID: "GPU-X"},
		{Index: 6, UUID: "GPU-6"},
	}

	t.Run("renumbered UUID fails closed rather than resetting the stale index", func(t *testing.T) {
		driver := &holdingDriver{Fake: &nvml.Fake{GPUs: inventory}}
		e := destructiveExecutor(t, driver)

		err := e.dispatch(context.Background(), types.Action{
			ID: "renumber", Type: types.ActionGPUReset,
			Params: map[string]string{"gpu_index": "3", "gpu_uuid": "GPU-X"},
		}, &types.ActionResult{})
		if err == nil {
			t.Fatal("reset must be refused when the UUID no longer maps to the requested index")
		}
		// The error must name both indices so an operator sees the renumber.
		for _, want := range []string{"GPU-X", "index 5", "index 3"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to mention %q", err, want)
			}
		}
		if driver.reset != 0 {
			t.Fatalf("nvidia-smi --gpu-reset ran %d time(s); it must never reset the stale index", driver.reset)
		}
	})

	t.Run("UUID absent from inventory fails closed", func(t *testing.T) {
		driver := &holdingDriver{Fake: &nvml.Fake{GPUs: inventory}}
		e := destructiveExecutor(t, driver)

		err := e.dispatch(context.Background(), types.Action{
			ID: "absent", Type: types.ActionGPUReset,
			Params: map[string]string{"gpu_index": "3", "gpu_uuid": "GPU-gone"},
		}, &types.ActionResult{})
		if err == nil || !strings.Contains(err.Error(), "not in current inventory") {
			t.Fatalf("err = %v, want a fail-closed refusal for a UUID absent from inventory", err)
		}
		if driver.reset != 0 {
			t.Fatalf("reset ran %d time(s); a device absent from inventory must never be reset", driver.reset)
		}
	})

	t.Run("MIG instance UUID fails closed (MIG decision: physical GPU is the remediation unit)", func(t *testing.T) {
		driver := &holdingDriver{Fake: &nvml.Fake{GPUs: inventory}}
		e := destructiveExecutor(t, driver)

		err := e.dispatch(context.Background(), types.Action{
			ID: "mig", Type: types.ActionGPUReset,
			Params: map[string]string{"gpu_index": "0", "gpu_uuid": "MIG-4b5c6d7e-1234-5678-9abc-def012345678"},
		}, &types.ActionResult{})
		if err == nil || !strings.Contains(err.Error(), "MIG instance UUID") {
			t.Fatalf("err = %v, want a fail-closed refusal for a MIG instance UUID", err)
		}
		if driver.reset != 0 {
			t.Fatalf("reset ran %d time(s); a MIG instance must never be reset per-instance", driver.reset)
		}
	})

	t.Run("missing gpu_uuid fails closed", func(t *testing.T) {
		driver := &holdingDriver{Fake: &nvml.Fake{GPUs: inventory}}
		e := destructiveExecutor(t, driver)

		err := e.dispatch(context.Background(), types.Action{
			ID: "no-uuid", Type: types.ActionGPUReset,
			Params: map[string]string{"gpu_index": "3"},
		}, &types.ActionResult{})
		if err == nil || !strings.Contains(err.Error(), "gpu_uuid is required") {
			t.Fatalf("err = %v, want a refusal to reset by index alone", err)
		}
		if driver.reset != 0 {
			t.Fatalf("reset ran %d time(s); a reset without a UUID must never run", driver.reset)
		}
	})

	t.Run("UUID resolving to the requested index proceeds", func(t *testing.T) {
		driver := &holdingDriver{Fake: &nvml.Fake{GPUs: inventory}}
		e := destructiveExecutor(t, driver)

		err := e.dispatch(context.Background(), types.Action{
			ID: "match", Type: types.ActionGPUReset,
			Params: map[string]string{"gpu_index": "5", "gpu_uuid": "GPU-X"},
		}, &types.ActionResult{})
		if err != nil {
			t.Fatalf("reset with a UUID that still maps to the requested index = %v", err)
		}
		if driver.reset != 1 {
			t.Fatalf("reset calls = %d, want 1", driver.reset)
		}
	})
}

// uuidResettingDriver additionally implements ResetGPUByUUID, so the executor
// should reset by stable UUID instead of the integer index.
type uuidResettingDriver struct {
	*holdingDriver
	resetUUID  string
	byUUIDCall int
}

func (d *uuidResettingDriver) ResetGPUByUUID(_ context.Context, uuid string) error {
	d.byUUIDCall++
	d.resetUUID = uuid
	return nil
}

// TestGPUResetPrefersUUIDOverIndex is the regression for the reset still being
// issued by integer index. When the driver can reset by UUID, the executor must
// pass the incident's UUID so an index shift in the moment before the reset
// cannot move it onto a healthy drained neighbor. The index path must not run.
func TestGPUResetPrefersUUIDOverIndex(t *testing.T) {
	driver := &uuidResettingDriver{holdingDriver: &holdingDriver{Fake: &nvml.Fake{GPUs: []types.GPUInfo{
		{Index: 0, UUID: "GPU-0"},
		{Index: 1, UUID: "GPU-X"},
	}}}}
	t.Setenv(destructiveLabEnv, "1")
	e := NewWithOptions(driver, Options{EnableDestructiveActions: true})

	if err := e.dispatch(context.Background(), types.Action{
		ID: "u1", Type: types.ActionGPUReset,
		Params: map[string]string{"gpu_index": "1", "gpu_uuid": "GPU-X"},
	}, &types.ActionResult{}); err != nil {
		t.Fatalf("reset = %v", err)
	}
	if driver.byUUIDCall != 1 || driver.resetUUID != "GPU-X" {
		t.Fatalf("ResetGPUByUUID calls = %d, uuid = %q; want it reset GPU-X by UUID", driver.byUUIDCall, driver.resetUUID)
	}
	if driver.reset != 0 {
		t.Fatalf("index-based ResetGPU ran %d time(s); the UUID path must be preferred", driver.reset)
	}
}

// uuidPreflightDriver addresses idle and holder checks by UUID. Its index-based
// checks are deliberately made to report a healthy, idle neighbor, while its
// UUID-based checks report the truth about the incident's device. A reset that
// preflighted by the (possibly renumbered) index would clear against the
// neighbor; a reset that preflights by the same UUID it will reset catches the
// real device's state.
type uuidPreflightDriver struct {
	*holdingDriver
	uuidBusy       string // UUID whose UUID-addressed idle check reports busy
	uuidHolders    []nvml.DeviceHolder
	byUUIDIdle     int
	byUUIDHolders  int
	resetUUID      string
	resetUUIDCalls int
}

// EnsureIdle is the index path: always idle, modeling a drained neighbor.
func (d *uuidPreflightDriver) EnsureIdle(context.Context, int) error { return nil }

func (d *uuidPreflightDriver) EnsureIdleByUUID(_ context.Context, uuid string) error {
	d.byUUIDIdle++
	if uuid == d.uuidBusy {
		return errors.New("GPU " + uuid + " is not idle: processes still attached (54321)")
	}
	return nil
}

// DeviceHolders is the index path: no holders, modeling a drained neighbor.
func (d *uuidPreflightDriver) DeviceHolders(int) ([]nvml.DeviceHolder, error) { return nil, nil }

func (d *uuidPreflightDriver) DeviceHoldersByUUID(string) ([]nvml.DeviceHolder, error) {
	d.byUUIDHolders++
	return d.uuidHolders, nil
}

func (d *uuidPreflightDriver) ResetGPUByUUID(_ context.Context, uuid string) error {
	d.resetUUIDCalls++
	d.resetUUID = uuid
	return nil
}

// TestGPUResetPreflightsTheSameUUIDItResets is the regression for the reset
// preflighting a stale index while resetting a UUID. resolveResetIndex proves
// UUID->index a moment before the checks, but a renumber in that window would
// leave an index-bound idle/holder check clearing a healthy neighbor while
// ResetGPUByUUID resets the real, unverified target. The idle and holder gates
// must therefore address the SAME UUID as the reset.
func TestGPUResetPreflightsTheSameUUIDItResets(t *testing.T) {
	inventory := []types.GPUInfo{{Index: 0, UUID: "GPU-X"}}

	t.Run("UUID-addressed idle catches a device the index check would clear", func(t *testing.T) {
		driver := &uuidPreflightDriver{
			holdingDriver: &holdingDriver{Fake: &nvml.Fake{GPUs: inventory}},
			uuidBusy:      "GPU-X", // the real target is busy...
		}
		e := destructiveExecutor(t, driver)

		err := e.dispatch(context.Background(), types.Action{
			ID: "busy-by-uuid", Type: types.ActionGPUReset,
			Params: map[string]string{"gpu_index": "0", "gpu_uuid": "GPU-X"},
		}, &types.ActionResult{})
		if err == nil || !strings.Contains(err.Error(), "not idle") {
			t.Fatalf("err = %v, want the UUID-addressed idle check to refuse the reset", err)
		}
		if driver.byUUIDIdle != 1 {
			t.Fatalf("UUID idle checks = %d, want the preflight bound to the UUID", driver.byUUIDIdle)
		}
		if driver.resetUUIDCalls != 0 {
			t.Fatalf("reset ran %d time(s); a device the UUID check found busy must never be reset", driver.resetUUIDCalls)
		}
	})

	t.Run("UUID-addressed holder check catches a device the index check would clear", func(t *testing.T) {
		driver := &uuidPreflightDriver{
			holdingDriver: &holdingDriver{Fake: &nvml.Fake{GPUs: inventory}},
			uuidHolders:   []nvml.DeviceHolder{{PID: 777, Command: "nv-hostengine", Device: "/dev/nvidia0"}},
		}
		e := destructiveExecutor(t, driver)

		err := e.dispatch(context.Background(), types.Action{
			ID: "held-by-uuid", Type: types.ActionGPUReset,
			Params: map[string]string{"gpu_index": "0", "gpu_uuid": "GPU-X"},
		}, &types.ActionResult{})
		if err == nil || !strings.Contains(err.Error(), "nv-hostengine") {
			t.Fatalf("err = %v, want the UUID-addressed holder check to refuse the reset", err)
		}
		if driver.byUUIDHolders != 1 {
			t.Fatalf("UUID holder checks = %d, want the preflight bound to the UUID", driver.byUUIDHolders)
		}
		if driver.resetUUIDCalls != 0 {
			t.Fatalf("reset ran %d time(s); a held device must never be reset", driver.resetUUIDCalls)
		}
	})

	t.Run("an idle, unheld target preflighted and reset by the same UUID proceeds", func(t *testing.T) {
		driver := &uuidPreflightDriver{holdingDriver: &holdingDriver{Fake: &nvml.Fake{GPUs: inventory}}}
		e := destructiveExecutor(t, driver)

		if err := e.dispatch(context.Background(), types.Action{
			ID: "ok-by-uuid", Type: types.ActionGPUReset,
			Params: map[string]string{"gpu_index": "0", "gpu_uuid": "GPU-X"},
		}, &types.ActionResult{}); err != nil {
			t.Fatalf("reset = %v", err)
		}
		if driver.byUUIDIdle != 1 || driver.byUUIDHolders != 1 {
			t.Fatalf("UUID preflights = idle:%d holders:%d, want both bound to the UUID", driver.byUUIDIdle, driver.byUUIDHolders)
		}
		if driver.resetUUIDCalls != 1 || driver.resetUUID != "GPU-X" {
			t.Fatalf("reset by UUID = %d call(s) for %q, want exactly one for GPU-X", driver.resetUUIDCalls, driver.resetUUID)
		}
	})
}

func TestPreflightRebootProbesWithoutRebooting(t *testing.T) {
	run := &scripted{}
	e := newTestExecutor(t, "boot-1", run)

	if err := e.PreflightReboot(context.Background()); err != nil {
		t.Fatalf("PreflightReboot() = %v", err)
	}
	if len(run.calls) != 1 {
		t.Fatalf("calls = %v, want exactly one probe", run.calls)
	}
	// The namespace entry is kept; what it would have run there is replaced.
	if got := run.calls[0]; got != "/bin/nsenter -t 1 -m -u -i -n -p -- true" {
		t.Fatalf("probe = %q, want the reboot replaced by true", got)
	}
	if strings.Contains(run.calls[0], "reboot") {
		t.Fatal("a startup preflight must never reboot the node")
	}
}

func TestPreflightRebootReportsAnUnprobeableCommand(t *testing.T) {
	run := &scripted{}
	e := newTestExecutor(t, "boot-1", run)
	e.RebootCommand = []string{"/sbin/reboot"}

	err := e.PreflightReboot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot be preflighted") {
		t.Fatalf("err = %v, want an explicit unverifiable result rather than a silent pass", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("calls = %v, want nothing executed", run.calls)
	}
}
