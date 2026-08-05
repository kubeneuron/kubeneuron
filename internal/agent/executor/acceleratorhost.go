package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	// persistenceDaemonUnit holds the GPU device open for as long as it runs.
	// It is an ordinary host service, so no Kubernetes label can stand it down:
	// only the node itself can, which is why this is an agent action and not a
	// platform one.
	persistenceDaemonUnit = "nvidia-persistenced"
	// holderPollInterval paces the wait for device handles to be released.
	holderPollInterval = 2 * time.Second
)

// deviceReleaseWaitTimeout bounds waitDeviceReleased when the quiesce action
// carries no timeout of its own. waitIdle refuses to run without a deadline; a
// quiesce step must likewise never poll forever, or a holder that never releases
// would wedge the agent's sequential action loop until the agent restarts. It is
// a var only so tests can shrink it.
var deviceReleaseWaitTimeout = 2 * time.Minute

// quiesceAcceleratorHost releases the node-side handles on the GPU and waits
// until the device is genuinely free.
//
// The waiting belongs here rather than in the controller. The controller can
// only infer, from pod labels, that the vendor's components have gone — and on
// a real cluster that inference was wrong: the node's device plugin came from
// the machine image rather than the GPU Operator, carried different labels, and
// the controller reported the stack settled while the plugin still held
// /dev/nvidia0. The node can simply look. Evidence beats inference, and the
// action fails with the remaining holders named rather than reporting a
// quiet stack that is not quiet.
func (e *Executor) quiesceAcceleratorHost(ctx context.Context, a types.Action, res *types.ActionResult) error {
	e.acceleratorHostMu.Lock()
	defer e.acceleratorHostMu.Unlock()
	index, err := paramInt(a.Params, "gpu_index")
	if err != nil {
		return err
	}
	key, err := hostStateKey(a.Params)
	if err != nil {
		return err
	}
	// The device UUID is the stable identity restore re-applies persistence
	// against, so an enumeration shift between quiesce and restore cannot touch a
	// neighbor. It is optional so drivers/tests without a UUID keep the index path.
	uuid := strings.TrimSpace(a.Params["gpu_uuid"])
	serviceActive, err := e.persistenceServiceActive(ctx)
	if err != nil {
		return fmt.Errorf("quiesce_accelerator_host: %w", err)
	}
	state := acceleratorHostState{GPUIndex: index, GPUUUID: uuid, ServiceWasActive: serviceActive}
	if reader, ok := e.driver.(persistenceModeReader); ok {
		enabled, err := reader.PersistenceMode(ctx, index)
		if err != nil {
			return fmt.Errorf("quiesce_accelerator_host: %w", err)
		}
		state.PersistenceKnown = true
		state.PersistenceWasEnabled = enabled
	}
	// This fsync-backed snapshot is deliberately before the first mutation. A
	// crash after systemctl stop must still be reversible by the janitor.
	if err := e.saveAcceleratorHostState(key, state); err != nil {
		return fmt.Errorf("quiesce_accelerator_host: %w", err)
	}
	// Persistence mode keeps the driver holding the device even with no
	// workload anywhere. Stop the daemon first: disabling the mode while its
	// daemon runs leaves the handle in place.
	if serviceActive {
		if out, err := e.runHostCommand(ctx, "systemctl", "stop", persistenceDaemonUnit); err != nil {
			res.Output = truncateOutput(out)
			return fmt.Errorf("quiesce_accelerator_host: stopping %s: %w", persistenceDaemonUnit, err)
		}
	}
	if state.PersistenceKnown && state.PersistenceWasEnabled {
		if err := e.setPersistenceMode(ctx, state.GPUUUID, index, false); err != nil {
			return fmt.Errorf("quiesce_accelerator_host: %w", err)
		}
	}
	holders, err := e.waitDeviceReleased(ctx, index)
	if err != nil {
		return fmt.Errorf("quiesce_accelerator_host: %w", err)
	}
	if len(holders) != 0 {
		res.Output = truncateOutput([]byte(nvml.FormatHolders(holders)))
		return fmt.Errorf("quiesce_accelerator_host: GPU %d is still held by %d process(es): %s",
			index, len(holders), nvml.FormatHolders(holders))
	}
	res.Output = fmt.Sprintf("GPU %d released; host persistence state quiesced", index)
	return nil
}

// restoreAcceleratorHost puts the node back exactly as it was before quiesce.
func (e *Executor) restoreAcceleratorHost(ctx context.Context, a types.Action, res *types.ActionResult) error {
	e.acceleratorHostMu.Lock()
	defer e.acceleratorHostMu.Unlock()
	key, err := hostStateKey(a.Params)
	if err != nil {
		return err
	}
	state, exists, err := e.loadAcceleratorHostState(key)
	if err != nil {
		return fmt.Errorf("restore_accelerator_host: %w", err)
	}
	if !exists {
		res.Output = "no accelerator host state was recorded; nothing to restore"
		return nil
	}
	// Quiesce changes persistence mode only when it was enabled. Do not write a
	// redundant "false" on restore: a separate administrator change made while
	// the host was quiesced must not be overwritten just because the original
	// state was already disabled.
	if state.PersistenceKnown && state.PersistenceWasEnabled {
		// Restore against the same PHYSICAL device the quiesce snapshotted. Prefer
		// the stable UUID: indices renumber when a neighbor falls off the bus or an
		// earlier ladder rung reloads the driver, so re-applying persistence by the
		// quiesce-time index could enable it on whichever device now holds that
		// index. The UUID names the original device no matter how enumeration moved.
		if err := e.setPersistenceMode(ctx, state.GPUUUID, state.GPUIndex, state.PersistenceWasEnabled); err != nil {
			return fmt.Errorf("restore_accelerator_host: %w", err)
		}
	}
	if state.ServiceWasActive {
		if out, err := e.runHostCommand(ctx, "systemctl", "start", persistenceDaemonUnit); err != nil {
			res.Output = truncateOutput(out)
			return fmt.Errorf("restore_accelerator_host: starting %s: %w", persistenceDaemonUnit, err)
		}
	}
	if err := e.deleteAcceleratorHostState(key); err != nil {
		return fmt.Errorf("restore_accelerator_host: %w", err)
	}
	res.Output = "accelerator host state restored"
	return nil
}

// setPersistenceMode applies persistence mode to one device, preferring the
// stable UUID over the enumeration index when both the caller and the driver
// can offer it. Addressing the device by UUID keeps quiesce and restore pinned
// to one physical GPU across any enumeration shift; the index path stays for
// drivers (the fake, any future one) or callers that have no UUID. A driver with
// neither setter silently no-ops, matching the pre-existing optional behavior.
func (e *Executor) setPersistenceMode(ctx context.Context, uuid string, index int, enabled bool) error {
	if uuid != "" {
		if setter, ok := e.driver.(persistenceModeSetterByUUID); ok {
			return setter.SetPersistenceModeByUUID(ctx, uuid, enabled)
		}
	}
	if setter, ok := e.driver.(persistenceModeSetter); ok {
		return setter.SetPersistenceMode(ctx, index, enabled)
	}
	return nil
}

// persistenceModeSetter is implemented by drivers that can toggle the driver's
// persistence mode. Optional so the fake driver stays usable.
type persistenceModeSetter interface {
	SetPersistenceMode(ctx context.Context, index int, enabled bool) error
}

// persistenceModeSetterByUUID toggles persistence mode addressing the device by
// its stable UUID, so an enumeration shift between quiesce and restore cannot
// move the toggle onto a neighbor. Optional, preferred when present.
type persistenceModeSetterByUUID interface {
	SetPersistenceModeByUUID(ctx context.Context, uuid string, enabled bool) error
}

type persistenceModeReader interface {
	PersistenceMode(ctx context.Context, index int) (bool, error)
}

func (e *Executor) persistenceServiceActive(ctx context.Context) (bool, error) {
	out, err := e.runHostCommand(ctx, "systemctl", "is-active", persistenceDaemonUnit)
	state := strings.TrimSpace(string(out))
	switch state {
	case "active":
		if err != nil {
			return false, fmt.Errorf("checking %s: %w", persistenceDaemonUnit, err)
		}
		return true, nil
	case "inactive", "failed":
		return false, nil
	default:
		return false, fmt.Errorf("checking %s returned %q: %w", persistenceDaemonUnit, state, err)
	}
}

// waitDeviceReleased polls until nothing holds the device, or the action's
// deadline passes. It returns the holders that outlasted the wait.
func (e *Executor) waitDeviceReleased(ctx context.Context, index int) ([]nvml.DeviceHolder, error) {
	lister, ok := e.driver.(deviceHolderLister)
	if !ok {
		return nil, nil
	}
	// Bound the wait even when the action carries no timeout, so a holder that
	// never releases cannot hang the agent's action loop indefinitely. When the
	// deadline passes the loop below returns the outstanding holders and the
	// caller turns them into a clear "still held" failure, so the remediation
	// ladder proceeds instead of blocking until an agent restart.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deviceReleaseWaitTimeout)
		defer cancel()
	}
	for {
		holders, err := lister.DeviceHolders(index)
		if err != nil {
			// Fail closed: without a readable process table there is no
			// evidence the device is free.
			return nil, fmt.Errorf("cannot determine which processes hold GPU %d: %w", index, err)
		}
		if len(holders) == 0 {
			return nil, nil
		}
		timer := time.NewTimer(holderPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return holders, nil
		case <-timer.C:
		}
	}
}

// runHostCommand runs a command in the host's namespaces, using the same entry
// the reboot action uses. A distroless container cannot address the host's
// service manager any other way.
func (e *Executor) runHostCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := e.rebootCommand()
	prefix, ok := rebootProbeCommand(command)
	if !ok {
		return nil, fmt.Errorf("no host namespace entry is configured (reboot command %q has no '--' separator)", command)
	}
	// rebootProbeCommand ends the prefix with "true"; replace it with the call.
	full := append(prefix[:len(prefix)-1:len(prefix)-1], name)
	full = append(full, args...)
	return e.run(ctx, full[0], full[1:]...)
}
