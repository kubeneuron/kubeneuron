package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type hostDriver struct {
	*holdingDriver
	persistence     []bool
	persistenceI    []int    // GPU index each index-scoped SetPersistenceMode toggle targeted
	persistenceUUID []string // GPU UUID each UUID-scoped toggle targeted
	pmErr           error
	mode            bool
	modeErr         error
}

func (d *hostDriver) SetPersistenceMode(_ context.Context, index int, enabled bool) error {
	if d.pmErr != nil {
		return d.pmErr
	}
	d.persistence = append(d.persistence, enabled)
	d.persistenceI = append(d.persistenceI, index)
	return nil
}

// uuidHostDriver additionally addresses persistence mode by the device's stable
// UUID, so quiesce/restore stay pinned to one physical GPU across a renumber.
type uuidHostDriver struct {
	*hostDriver
}

func (d *uuidHostDriver) SetPersistenceModeByUUID(_ context.Context, uuid string, enabled bool) error {
	if d.pmErr != nil {
		return d.pmErr
	}
	d.persistence = append(d.persistence, enabled)
	d.persistenceUUID = append(d.persistenceUUID, uuid)
	return nil
}

func (d *hostDriver) PersistenceMode(_ context.Context, _ int) (bool, error) {
	return d.mode, d.modeErr
}

func hostExecutor(t *testing.T, d *hostDriver, run *scripted) *Executor {
	t.Helper()
	t.Setenv(destructiveLabEnv, "1")
	e := NewWithOptions(d, Options{EnableDestructiveActions: true, AcceleratorHostStatePath: t.TempDir() + "/accelerator-host-state.json"})
	e.run = run.run
	return e
}

func TestQuiesceHostStopsPersistenceThroughTheHostNamespaces(t *testing.T) {
	run := &scripted{outputs: []string{"active", ""}}
	d := &hostDriver{holdingDriver: &holdingDriver{Fake: &nvml.Fake{}}, mode: true}
	e := hostExecutor(t, d, run)

	res := &types.ActionResult{}
	err := e.dispatch(context.Background(), types.Action{
		ID: "q1", Type: types.ActionQuiesceAcceleratorHost, Params: map[string]string{"gpu_index": "0", "host_state_key": "node-a"},
	}, res)
	if err != nil {
		t.Fatalf("quiesce = %v", err)
	}
	// The persistence daemon is an ordinary host service; a distroless
	// container reaches it only through PID 1's namespaces.
	if len(run.calls) != 2 || run.calls[0] != "/bin/nsenter -t 1 -m -u -i -n -p -- systemctl is-active nvidia-persistenced" || run.calls[1] != "/bin/nsenter -t 1 -m -u -i -n -p -- systemctl stop nvidia-persistenced" {
		t.Fatalf("calls = %v", run.calls)
	}
	if len(d.persistence) != 1 || d.persistence[0] {
		t.Fatalf("persistence toggles = %v, want it disabled", d.persistence)
	}
}

func TestQuiesceHostFailsWithTheRemainingHoldersNamed(t *testing.T) {
	run := &scripted{outputs: []string{"active", ""}}
	d := &hostDriver{holdingDriver: &holdingDriver{Fake: &nvml.Fake{}, holders: []nvml.DeviceHolder{
		{PID: 11567, Command: "nvidia-device-p", Device: "/dev/nvidia0"},
	}}, mode: true}
	e := hostExecutor(t, d, run)

	res := &types.ActionResult{}
	// A zero timeout means the deadline has already passed, so the wait ends
	// immediately with whatever still holds the device.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := e.quiesceAcceleratorHost(ctx, types.Action{
		ID: "q2", Type: types.ActionQuiesceAcceleratorHost, Params: map[string]string{"gpu_index": "0", "host_state_key": "node-a"},
	}, res)
	if err == nil {
		t.Fatal("a device still held must fail the quiesce")
	}
	if !strings.Contains(err.Error(), "nvidia-device-p(11567)") {
		t.Fatalf("err = %v, want the holder named", err)
	}
}

func TestQuiesceHostFailsClosedWhenTheProcessTableIsUnreadable(t *testing.T) {
	run := &scripted{outputs: []string{"active", ""}}
	d := &hostDriver{holdingDriver: &holdingDriver{Fake: &nvml.Fake{}, err: errors.New("procfs unavailable")}, mode: true}
	e := hostExecutor(t, d, run)

	err := e.quiesceAcceleratorHost(context.Background(), types.Action{
		ID: "q3", Params: map[string]string{"gpu_index": "0", "host_state_key": "node-a"},
	}, &types.ActionResult{})
	if err == nil || !strings.Contains(err.Error(), "cannot determine") {
		t.Fatalf("err = %v, want a fail-closed refusal", err)
	}
}

func TestRestoreHostPutsPersistenceBack(t *testing.T) {
	run := &scripted{outputs: []string{"active", "", ""}}
	d := &hostDriver{holdingDriver: &holdingDriver{Fake: &nvml.Fake{}}, mode: true}
	e := hostExecutor(t, d, run)

	if err := e.dispatch(context.Background(), types.Action{ID: "q-restore", Type: types.ActionQuiesceAcceleratorHost, Params: map[string]string{"gpu_index": "0", "host_state_key": "node-a"}}, &types.ActionResult{}); err != nil {
		t.Fatalf("quiesce = %v", err)
	}
	res := &types.ActionResult{}
	if err := e.dispatch(context.Background(), types.Action{
		ID: "r1", Type: types.ActionRestoreAcceleratorHost, Params: map[string]string{"host_state_key": "node-a"},
	}, res); err != nil {
		t.Fatalf("restore = %v", err)
	}
	if len(d.persistence) != 2 || d.persistence[0] || !d.persistence[1] {
		t.Fatalf("persistence toggles = %v, want it re-enabled", d.persistence)
	}
	if len(run.calls) != 3 || !strings.HasSuffix(run.calls[2], "systemctl start nvidia-persistenced") {
		t.Fatalf("calls = %v, want the daemon started again", run.calls)
	}
}

// TestQuiesceRestoreScopePersistenceToTheTargetGPU is the regression for the
// node-wide persistence toggle. `nvidia-smi -pm` with no -i flips persistence
// for every GPU on the node, so a per-GPU quiesce would disturb healthy
// siblings and restore could not put the exact snapshot back. Both the quiesce
// disable and the restore enable must name the incident's GPU index — here a
// non-zero index, so a hardcoded 0 would fail.
func TestQuiesceRestoreScopePersistenceToTheTargetGPU(t *testing.T) {
	run := &scripted{outputs: []string{"active", "", ""}}
	d := &hostDriver{holdingDriver: &holdingDriver{Fake: &nvml.Fake{}}, mode: true}
	e := hostExecutor(t, d, run)
	key := map[string]string{"gpu_index": "3", "host_state_key": "node-a"}

	if err := e.dispatch(context.Background(), types.Action{ID: "q", Type: types.ActionQuiesceAcceleratorHost, Params: key}, &types.ActionResult{}); err != nil {
		t.Fatalf("quiesce = %v", err)
	}
	if err := e.dispatch(context.Background(), types.Action{ID: "r", Type: types.ActionRestoreAcceleratorHost, Params: map[string]string{"host_state_key": "node-a"}}, &types.ActionResult{}); err != nil {
		t.Fatalf("restore = %v", err)
	}
	// Disable on quiesce, enable on restore — both against GPU index 3, never a
	// bare node-wide toggle.
	if len(d.persistence) != 2 || d.persistence[0] || !d.persistence[1] {
		t.Fatalf("persistence toggles = %v, want [disable, enable]", d.persistence)
	}
	for _, gotIndex := range d.persistenceI {
		if gotIndex != 3 {
			t.Fatalf("persistence toggled GPU indices = %v, want every toggle scoped to index 3", d.persistenceI)
		}
	}
}

func TestRestoreHostPreservesInitiallyDisabledState(t *testing.T) {
	run := &scripted{outputs: []string{"inactive"}}
	d := &hostDriver{holdingDriver: &holdingDriver{Fake: &nvml.Fake{}}, mode: false}
	e := hostExecutor(t, d, run)
	key := map[string]string{"gpu_index": "0", "host_state_key": "node-a"}
	if err := e.dispatch(context.Background(), types.Action{ID: "q-disabled", Type: types.ActionQuiesceAcceleratorHost, Params: key}, &types.ActionResult{}); err != nil {
		t.Fatalf("quiesce = %v", err)
	}
	if err := e.dispatch(context.Background(), types.Action{ID: "r-disabled", Type: types.ActionRestoreAcceleratorHost, Params: map[string]string{"host_state_key": "node-a"}}, &types.ActionResult{}); err != nil {
		t.Fatalf("restore = %v", err)
	}
	if len(run.calls) != 1 || !strings.HasSuffix(run.calls[0], "systemctl is-active nvidia-persistenced") {
		t.Fatalf("calls = %v, inactive daemon must not be started", run.calls)
	}
	if len(d.persistence) != 0 {
		t.Fatalf("persistence toggles = %v, disabled mode must stay disabled", d.persistence)
	}
}

// TestWaitDeviceReleasedIsBoundedWithoutAnActionTimeout is the regression for
// waitDeviceReleased polling forever. A quiesce step whose action carried no
// timeout, against a holder that never releases, would block the agent's
// sequential action loop until an agent restart. The wait must impose its own
// bound (like waitIdle's required deadline) and return promptly with the
// outstanding holders so the caller fails the action instead of hanging.
func TestWaitDeviceReleasedIsBoundedWithoutAnActionTimeout(t *testing.T) {
	original := deviceReleaseWaitTimeout
	deviceReleaseWaitTimeout = 30 * time.Millisecond
	t.Cleanup(func() { deviceReleaseWaitTimeout = original })

	run := &scripted{outputs: []string{"active", ""}}
	d := &hostDriver{holdingDriver: &holdingDriver{Fake: &nvml.Fake{}, holders: []nvml.DeviceHolder{
		{PID: 4242, Command: "nvidia-device-p", Device: "/dev/nvidia0"},
	}}, mode: true}
	e := hostExecutor(t, d, run)

	// context.Background() carries NO deadline, exactly like an action with no
	// timeout. Without the self-imposed bound this call would never return.
	done := make(chan []nvml.DeviceHolder, 1)
	go func() {
		holders, _ := e.waitDeviceReleased(context.Background(), 0)
		done <- holders
	}()

	select {
	case holders := <-done:
		if len(holders) != 1 || holders[0].PID != 4242 {
			t.Fatalf("holders = %v, want the holder that outlasted the bounded wait", holders)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitDeviceReleased hung well past its own bound; the action loop would be wedged")
	}

	// The surrounding quiesce turns that into a clear, holder-named failure.
	err := e.quiesceAcceleratorHost(context.Background(), types.Action{
		ID: "q-timeout", Type: types.ActionQuiesceAcceleratorHost, Params: map[string]string{"gpu_index": "0", "host_state_key": "node-t"},
	}, &types.ActionResult{})
	if err == nil || !strings.Contains(err.Error(), "still held") {
		t.Fatalf("quiesce = %v, want a clear still-held failure rather than a hang", err)
	}
}

// TestRestoreScopesPersistenceToTheSnapshottedUUID is the regression for restore
// re-applying persistence mode by the quiesce-time index. An enumeration shift
// between quiesce and restore would enable persistence on whichever device now
// holds that index; snapshotting the UUID and restoring via -i <uuid> keeps the
// toggle pinned to the original physical device.
func TestRestoreScopesPersistenceToTheSnapshottedUUID(t *testing.T) {
	run := &scripted{outputs: []string{"active", "", ""}}
	d := &uuidHostDriver{hostDriver: &hostDriver{holdingDriver: &holdingDriver{Fake: &nvml.Fake{}}, mode: true}}
	t.Setenv(destructiveLabEnv, "1")
	e := NewWithOptions(d, Options{EnableDestructiveActions: true, AcceleratorHostStatePath: t.TempDir() + "/accelerator-host-state.json"})
	e.run = run.run

	params := map[string]string{"gpu_index": "3", "gpu_uuid": "GPU-abc", "host_state_key": "node-a"}
	if err := e.dispatch(context.Background(), types.Action{ID: "q", Type: types.ActionQuiesceAcceleratorHost, Params: params}, &types.ActionResult{}); err != nil {
		t.Fatalf("quiesce = %v", err)
	}
	if err := e.dispatch(context.Background(), types.Action{ID: "r", Type: types.ActionRestoreAcceleratorHost, Params: map[string]string{"host_state_key": "node-a"}}, &types.ActionResult{}); err != nil {
		t.Fatalf("restore = %v", err)
	}
	// Disable on quiesce, enable on restore — both addressed by the snapshotted
	// UUID, never by the quiesce-time index (which could name a neighbor now).
	if len(d.persistence) != 2 || d.persistence[0] || !d.persistence[1] {
		t.Fatalf("persistence toggles = %v, want [disable, enable]", d.persistence)
	}
	if len(d.persistenceUUID) != 2 {
		t.Fatalf("UUID-scoped toggles = %v, want both quiesce and restore addressed by UUID", d.persistenceUUID)
	}
	for _, gotUUID := range d.persistenceUUID {
		if gotUUID != "GPU-abc" {
			t.Fatalf("persistence toggled UUIDs = %v, want every toggle scoped to GPU-abc", d.persistenceUUID)
		}
	}
	if len(d.persistenceI) != 0 {
		t.Fatalf("index-scoped toggles = %v, want none when a UUID is available", d.persistenceI)
	}
}

func TestHostQuiesceIsDestructiveAndGated(t *testing.T) {
	run := &scripted{}
	d := &hostDriver{holdingDriver: &holdingDriver{Fake: &nvml.Fake{}}}
	e := New(d) // destructive actions disabled
	e.run = run.run

	err := e.dispatch(context.Background(), types.Action{
		ID: "q4", Type: types.ActionQuiesceAcceleratorHost, Params: map[string]string{"gpu_index": "0", "host_state_key": "node-a"},
	}, &types.ActionResult{})
	if err == nil || !strings.Contains(err.Error(), "destructive actions are disabled") {
		t.Fatalf("err = %v, want the destructive gate to refuse", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("calls = %v, want nothing executed", run.calls)
	}
}
