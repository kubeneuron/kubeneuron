// Package executor runs controller-requested actions locally on the node
// and enforces idempotency: action IDs are deterministic, and completed IDs
// are cached so a controller retry after a crash re-returns the original
// result instead of re-executing.
package executor

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	// cacheTTL bounds how long completed action results are kept for replay.
	cacheTTL = 24 * time.Hour
	// defaultBundleDir receives diagnostic bundles.
	defaultBundleDir = "/var/lib/kube-neuron/bundles"
	// defaultScriptsDir holds operator-provisioned remediation scripts. Only
	// scripts that already exist in this root-controlled directory can run;
	// a playbook or CRD can select among them but can never inject content.
	defaultScriptsDir = "/etc/kube-neuron/scripts"
	// defaultAcceleratorHostStatePath records the original host state before a
	// quiesce changes it. The managed agent mounts this directory from the host,
	// so a controller or agent restart cannot lose the restoration contract.
	defaultAcceleratorHostStatePath = "/var/lib/kube-neuron/accelerator-host-state.json"
	// maxCapturedOutput bounds subprocess output kept in an ActionResult.
	maxCapturedOutput = 16 << 10
	// waitIdlePollInterval keeps an explicitly requested idle wait responsive
	// without turning an unavailable GPU into a tight polling loop.
	waitIdlePollInterval = 5 * time.Second
	// destructiveLabEnv is deliberately required in addition to explicit
	// runtime enablement when a Go test binary executes an action. It keeps a
	// unit-test regression from rebooting or resetting the host running tests.
	// Only a dedicated, disposable hardware lab runner may opt in.
	destructiveLabEnv = "KUBENEURON_DESTRUCTIVE_LAB"
	// rebootPreflightTimeout bounds the startup check that the host reboot
	// mechanism is reachable. It only enters the host namespaces and returns.
	rebootPreflightTimeout = 10 * time.Second
)

// defaultRebootCommand asks the host's init to reboot from inside the agent
// container. `systemctl` alone fails with exit 127: the agent image is
// distroless and, even with the binary present, systemd must be addressed in
// the host's namespaces. The managed DaemonSet already runs privileged with
// hostPID, so entering PID 1's namespaces is available; nsenter ships in the
// agent image for exactly this call. Operators on a different init can replace
// the whole command with --reboot-command.
func defaultRebootCommand() []string {
	return []string{nsenterPath, "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "systemctl", "reboot"}
}

// nsenterPath is the static nsenter shipped in the agent image. It is busybox
// installed under the applet's own name, so argv[0] selects nsenter.
const nsenterPath = "/bin/nsenter"

// Options controls capabilities that must never be enabled implicitly.
type Options struct {
	// EnableDestructiveActions permits actions that can alter node or device
	// state. It must be enabled explicitly by the production agent
	// configuration. It is still blocked from go test binaries unless
	// KUBENEURON_DESTRUCTIVE_LAB=1 is set for a dedicated hardware lab run.
	EnableDestructiveActions bool
	// Armed, when set, is the LIVE arming source consulted on every
	// destructive dispatch — the agent wires it to its controller-served
	// arming state, so one process can arm and disarm as the declared blast
	// radius changes without a restart. When nil, the static
	// EnableDestructiveActions value applies for the process lifetime.
	Armed func() bool
	// AcceleratorHostStatePath stores the crash-safe host quiesce snapshots.
	// Empty selects the managed host-path default.
	AcceleratorHostStatePath string
}

// Executor executes actions on the local node.
type Executor struct {
	driver nvml.GPUDriver
	// BundleDir receives collect_bundle output; default defaultBundleDir.
	BundleDir string
	// ScriptsDir holds operator-provisioned scripts; default defaultScriptsDir.
	ScriptsDir string
	// RebootCommand reboots the host; default defaultRebootCommand(). It is
	// replaceable because the mechanism is environment-specific, but it is
	// never derived from action parameters: a playbook can request a reboot,
	// it can never choose how the host is rebooted.
	RebootCommand []string

	// run executes a subprocess and returns combined output; tests inject it.
	run runner
	// readBootID returns the current boot ID; tests inject it.
	readBootID func() (string, error)
	// armed is deliberately opt-in and consulted per dispatch. A normal executor is
	// observation-only with respect to host and GPU state.
	armed                    func() bool
	acceleratorHostStatePath string
	// acceleratorHostMu serializes node-wide persistence-daemon transitions.
	// Different per-GPU actions can otherwise race through the same durable
	// host-state record and restore the wrong state.
	acceleratorHostMu sync.Mutex

	mu      sync.Mutex
	done    map[string]cached        // action ID -> result
	running map[string]chan struct{} // action ID -> completion notification
}

type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

type cached struct {
	result types.ActionResult
	at     time.Time
}

// New builds an executor on top of a GPU driver with destructive actions
// disabled. Production callers must use NewWithOptions with explicit
// enablement after their own configuration preflight.
func New(driver nvml.GPUDriver) *Executor {
	return NewWithOptions(driver, Options{})
}

// NewWithOptions builds an executor on top of a GPU driver. Destructive node
// and GPU operations remain unavailable unless EnableDestructiveActions is
// explicitly true. This constructor does not bypass the go test lab guard.
func NewWithOptions(driver nvml.GPUDriver, options Options) *Executor {
	hostStatePath := options.AcceleratorHostStatePath
	if hostStatePath == "" {
		hostStatePath = defaultAcceleratorHostStatePath
	}
	return &Executor{
		driver:                   driver,
		BundleDir:                defaultBundleDir,
		ScriptsDir:               defaultScriptsDir,
		RebootCommand:            defaultRebootCommand(),
		run:                      execRunner,
		readBootID:               currentBootID,
		armed:                    armedSource(options),
		acceleratorHostStatePath: hostStatePath,
		done:                     map[string]cached{},
		running:                  map[string]chan struct{}{},
	}
}

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func currentBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// Execute runs an action, replaying the cached result for already-completed
// action IDs.
func (e *Executor) Execute(ctx context.Context, a types.Action) (*types.ActionResult, error) {
	if a.ID == "" {
		return nil, fmt.Errorf("action ID is required")
	}

	// A poll retry can overlap an in-flight result POST. Keep exactly one
	// execution per action ID inside this process; callers that arrive while an
	// action is running wait for the cached result rather than dispatching a
	// second subprocess or reset.
	for {
		e.mu.Lock()
		if c, ok := e.done[a.ID]; ok && time.Since(c.at) < cacheTTL {
			e.mu.Unlock()
			r := c.result
			return &r, nil
		}
		if complete, ok := e.running[a.ID]; ok {
			e.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-complete:
			}
			continue
		}
		complete := make(chan struct{})
		e.running[a.ID] = complete
		e.mu.Unlock()
		break
	}

	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}

	res := types.ActionResult{ActionID: a.ID, StartedAt: time.Now()}
	err := e.dispatch(ctx, a, &res)
	res.FinishedAt = time.Now()
	res.OK = err == nil
	if err != nil {
		res.Error = err.Error()
		if errors.Is(err, nvml.ErrNotIdle) {
			// Say so in a field rather than leaving the controller to read the
			// prose: "the device is busy" and "the idle probe is broken" are
			// the same failure to the ladder but opposite facts to anyone
			// asking what the control plane protected.
			res.Refusal = types.RefusalNotIdle
		}
	}

	e.mu.Lock()
	e.done[a.ID] = cached{result: res, at: time.Now()}
	// Evict expired entries while we hold the lock; without this the
	// idempotency cache grows for the life of the process.
	for id, c := range e.done {
		if time.Since(c.at) >= cacheTTL {
			delete(e.done, id)
		}
	}
	complete := e.running[a.ID]
	delete(e.running, a.ID)
	close(complete)
	e.mu.Unlock()
	return &res, err
}

func (e *Executor) dispatch(ctx context.Context, a types.Action, res *types.ActionResult) error {
	if isDestructive(a.Type) {
		if err := e.allowDestructiveAction(a.Type); err != nil {
			return err
		}
	}

	switch a.Type {
	case types.ActionIdleCheck:
		return e.idleCheck(ctx, a)
	case types.ActionWaitIdle:
		return e.waitIdle(ctx, a)
	case types.ActionGPUReset:
		// Bind the reset to the physical device by its stable UUID before doing
		// anything: the request's gpu_index was captured when the incident
		// opened and may no longer point at the same GPU.
		idx, err := e.resolveResetIndex(ctx, a)
		if err != nil {
			return err
		}
		uuid := strings.TrimSpace(a.Params["gpu_uuid"])
		// Final safety net after the drain: never reset a GPU that still has
		// processes attached. The reset targets the stable UUID, so the preflight
		// must too — resolveResetIndex proved UUID->idx a moment ago, but a renumber
		// between that resolve and here would leave an index-bound idle/holder check
		// clearing a healthy neighbor while ResetGPUByUUID resets the real (possibly
		// non-idle) target. Addressing the idle and holder checks by the SAME UUID
		// keeps preflight and reset pinned to one device. Drivers with no UUID
		// addressing fall back to the freshly-resolved index.
		if err := e.ensureIdle(ctx, uuid, idx); err != nil {
			return err
		}
		if err := e.ensureNoDeviceHolders(uuid, idx, res); err != nil {
			return err
		}
		return e.resetGPU(ctx, uuid, idx)
	case types.ActionRunDiag:
		return e.runDiag(ctx, a, res)
	case types.ActionCollectBundle:
		return e.collectBundle(ctx, a, res)
	case types.ActionQuiesceAcceleratorHost:
		return e.quiesceAcceleratorHost(ctx, a, res)
	case types.ActionRestoreAcceleratorHost:
		return e.restoreAcceleratorHost(ctx, a, res)
	case types.ActionReboot:
		return e.reboot(ctx, a, res)
	case types.ActionDriverReload:
		return e.runAllowListedScript(ctx, "driver_reload.sh", a, res)
	case types.ActionDriverReinstall:
		return e.runAllowListedScript(ctx, "driver_reinstall.sh", a, res)
	case types.ActionRunScript:
		name, err := scriptName(a.Params["script"])
		if err != nil {
			return err
		}
		return e.runAllowListedScript(ctx, name, a, res)
	default:
		return fmt.Errorf("unknown action type %q", a.Type)
	}
}

// allowDestructiveAction is the final local safety boundary. Controller
// policy, approvals, and node drain are necessary higher-level safeguards,
// but none of them should let an ordinary test process alter its host.
func (e *Executor) allowDestructiveAction(action types.ActionType) error {
	// Undoing is always allowed: restore_accelerator_host can only put back
	// state a previously-ARMED quiesce changed (it replays the crash-safe
	// host snapshot; with no snapshot it restores nothing), so it executes
	// even on a node that has since been disarmed — a shrunk blast radius or
	// a disabled Enabled mode must not strand a quiesced node with its
	// monitoring down because its agent now refuses the restore. The go-test
	// lab guard below still applies to it.
	if action != types.ActionRestoreAcceleratorHost && !e.armed() {
		return fmt.Errorf("%s: destructive actions are disabled on this node; the controller has not armed it (spec.safety.destructiveExecution.nodeSelector) and no explicit agent flag did", action)
	}
	if runningGoTest() && os.Getenv(destructiveLabEnv) != "1" {
		return fmt.Errorf("%s: destructive actions are blocked while running under go test; %s=1 is required on a dedicated hardware lab runner", action, destructiveLabEnv)
	}
	return nil
}

func isDestructive(action types.ActionType) bool {
	switch action {
	case types.ActionGPUReset,
		types.ActionReboot,
		types.ActionQuiesceAcceleratorHost,
		types.ActionRestoreAcceleratorHost,
		types.ActionDriverReload,
		types.ActionDriverReinstall,
		types.ActionRunScript,
		types.ActionPowerCycle:
		return true
	default:
		return false
	}
}

// runningGoTest identifies the standard Go test binary without importing the
// testing package into production code. Normal binaries do not register the
// test.v flag.
func runningGoTest() bool {
	return flag.Lookup("test.v") != nil
}

func (e *Executor) idleCheck(ctx context.Context, a types.Action) error {
	idx, err := paramInt(a.Params, "gpu_index")
	if err != nil {
		return err
	}
	if err := e.driver.EnsureIdle(ctx, idx); err != nil {
		return fmt.Errorf("idle_check: %w", err)
	}
	return nil
}

// resolveResetIndex binds a gpu_reset to a physical device by its stable UUID
// rather than the index the controller captured when the incident opened.
//
// NVIDIA GPU indices are not stable. An XID 79 ("fallen off the bus") drops the
// failing device from enumeration and renumbers every higher-indexed GPU down
// by one; an earlier driver-reload or reboot rung in the same remediation
// ladder can renumber them too. The controller-side evidence gate only proves
// the target UUID still appears in the report — never that it still maps to the
// index in the request. Resetting by that stale index can therefore land on a
// healthy neighbor, and the EnsureIdle/holder preflight would clear it because
// the neighbor is idle on a drained node.
//
// So we re-resolve the index from live inventory at execution time and fail
// closed on any ambiguity: a missing UUID, a UUID absent from current
// inventory, or a resolved index that disagrees with the request. A
// disagreement means the topology changed under the incident and a human must
// look before a destructive action lands.
func (e *Executor) resolveResetIndex(ctx context.Context, a types.Action) (int, error) {
	requestedIndex, err := paramInt(a.Params, "gpu_index")
	if err != nil {
		return 0, err
	}
	uuid := strings.TrimSpace(a.Params["gpu_uuid"])
	if uuid == "" {
		return 0, fmt.Errorf("gpu_reset: gpu_uuid is required to bind the reset to a physical device; refusing to reset GPU index %d by index alone", requestedIndex)
	}
	// MIG decision (docs/mig-decision.md, design.md §2.4d): the remediation
	// unit is the PHYSICAL GPU, and per-instance reset is rejected
	// permanently rather than pending.
	//
	// A "MIG-<uuid>" names a compute instance, not a device the driver can
	// reset: NVIDIA exposes no per-instance reset at all. The only thing
	// that could be done at instance granularity is destroying and
	// recreating the instance, which reconfigures the partitioning and
	// changes what the device plugin advertises to the scheduler — a
	// MIG-manager lifecycle operation, never a remediation step. And a
	// reset issued through the parent would take down every instance on it
	// while the incident's evidence describes exactly one.
	//
	// Remediating the parent is a decided but unbuilt capability, and it is
	// not what this branch withholds: the preflight here cannot see inside
	// MIG at all. EnsureIdle addresses the parent, and whether that reports
	// processes running INSIDE its instances is unverified; the holder scan
	// resolves a device minor that a MIG parent may report as [N/A] and
	// looks for /dev/nvidiaN, while instance consumers hold
	// /dev/nvidia-caps nodes. So "idle" here would be a claim this code
	// cannot support on partitioned hardware. Fail closed; a human routes
	// the ladder at the parent.
	//
	// Reaching this branch at all should be rare: the agent reports a
	// MIG-mode node as TopologySafety=partitioned/Readiness=degraded, which
	// the controller's reset gate already refuses. This is the last of four
	// independent refusals, kept because a UUID can arrive from a
	// hand-authored playbook or a stale incident target.
	if strings.HasPrefix(uuid, "MIG-") {
		return 0, fmt.Errorf("gpu_reset: %s is a MIG instance UUID, not a physical GPU; per-instance reset is not a supported remediation unit — target the parent GPU or escalate", uuid)
	}
	gpus, err := e.driver.ListGPUs(ctx)
	if err != nil {
		return 0, fmt.Errorf("gpu_reset: cannot read GPU inventory to resolve %s: %w", uuid, err)
	}
	resolvedIndex := -1
	for _, g := range gpus {
		if strings.TrimSpace(g.UUID) == uuid {
			resolvedIndex = g.Index
			break
		}
	}
	if resolvedIndex < 0 {
		return 0, fmt.Errorf("gpu_reset: GPU %s is not in current inventory; it may have fallen off the bus or been renumbered — refusing to reset", uuid)
	}
	if resolvedIndex != requestedIndex {
		return 0, fmt.Errorf("gpu_reset: GPU %s is now at index %d but the request targets index %d; topology changed under the incident — refusing to reset, a human must confirm", uuid, resolvedIndex, requestedIndex)
	}
	return resolvedIndex, nil
}

// gpuUUIDResetter is implemented by drivers that can reset a GPU by its stable
// UUID. It is optional so the fake driver and any future driver stay usable
// through the index-based ResetGPU fallback.
type gpuUUIDResetter interface {
	ResetGPUByUUID(ctx context.Context, uuid string) error
}

// resetGPU issues the reset against the device's stable UUID when the driver
// supports it, closing the resolve->reset renumber window that an integer index
// leaves open. resolveResetIndex has already proven the UUID still maps to the
// requested index; passing the UUID here means an index shift in the moment
// before the reset still cannot move it onto a healthy neighbor. Drivers with
// no UUID reset fall back to the index.
func (e *Executor) resetGPU(ctx context.Context, uuid string, index int) error {
	if r, ok := e.driver.(gpuUUIDResetter); ok {
		return r.ResetGPUByUUID(ctx, uuid)
	}
	return e.driver.ResetGPU(ctx, index)
}

// ensureIdle runs the post-drain idle safety net against the same physical
// device the reset will target. It prefers the UUID-addressed check so an
// enumeration shift after the resolve cannot let a healthy neighbor clear the
// gate; drivers without UUID addressing fall back to the freshly-resolved index.
func (e *Executor) ensureIdle(ctx context.Context, uuid string, index int) error {
	if uuid != "" {
		if idler, ok := e.driver.(gpuUUIDIdler); ok {
			return idler.EnsureIdleByUUID(ctx, uuid)
		}
	}
	return e.driver.EnsureIdle(ctx, index)
}

// deviceHolderLister is implemented by drivers that can name the processes
// holding a GPU device node. It is optional so the fake driver and any future
// driver stay usable without it.
type deviceHolderLister interface {
	DeviceHolders(index int) ([]nvml.DeviceHolder, error)
}

// gpuUUIDIdler runs the idle check addressing the device by its stable UUID
// (nvidia-smi accepts a UUID wherever it accepts an index for -i), so the
// preflight cannot be pointed at a neighbor by a post-resolve renumber. Optional,
// preferred when present.
type gpuUUIDIdler interface {
	EnsureIdleByUUID(ctx context.Context, uuid string) error
}

// uuidDeviceHolderLister names the processes holding a device resolved by its
// stable UUID, so the holder/minor resolution targets the same device as the
// reset rather than a renumbered index. Optional, preferred when present.
type uuidDeviceHolderLister interface {
	DeviceHoldersByUUID(uuid string) ([]nvml.DeviceHolder, error)
}

// ensureNoDeviceHolders refuses a reset while any process holds the device.
//
// EnsureIdle only sees compute applications. On a stock GPU Operator node
// nv-hostengine, dcgm-exporter and nvidia-device-plugin each keep an open
// handle on /dev/nvidia0 without running a CUDA context, so the idle check
// passes and nvidia-smi --gpu-reset then fails with exit 19 and the useless
// text "currently in use by another process". Naming the holders here turns
// that dead end into an actionable failure — and into the input for the
// quiesce step that releases them.
func (e *Executor) ensureNoDeviceHolders(uuid string, index int, res *types.ActionResult) error {
	holders, listed, err := e.deviceHolders(uuid, index)
	if err != nil {
		// Fail closed: an unreadable process table cannot clear a reset.
		return fmt.Errorf("gpu_reset: cannot determine which processes hold GPU %d: %w", index, err)
	}
	if !listed || len(holders) == 0 {
		return nil
	}
	res.Output = truncateOutput([]byte(nvml.FormatHolders(holders)))
	return fmt.Errorf("gpu_reset: GPU %d is held by %d process(es) and cannot be reset: %s; release them first (a quiesce step must stop the NVIDIA monitoring stack on this node)",
		index, len(holders), nvml.FormatHolders(holders))
}

// deviceHolders lists the processes holding the reset target, preferring the
// UUID-addressed lister so the holder/minor resolution follows the same device
// as the reset rather than a renumbered index. listed reports whether any lister
// was available at all: a driver with neither cannot name holders and the caller
// treats that as "nothing to enforce", exactly as before.
func (e *Executor) deviceHolders(uuid string, index int) (holders []nvml.DeviceHolder, listed bool, err error) {
	if uuid != "" {
		if lister, ok := e.driver.(uuidDeviceHolderLister); ok {
			holders, err = lister.DeviceHoldersByUUID(uuid)
			return holders, true, err
		}
	}
	if lister, ok := e.driver.(deviceHolderLister); ok {
		holders, err = lister.DeviceHolders(index)
		return holders, true, err
	}
	return nil, false, nil
}

func (e *Executor) waitIdle(ctx context.Context, a types.Action) error {
	if _, ok := ctx.Deadline(); !ok {
		return fmt.Errorf("wait_idle requires a positive action timeout")
	}
	idx, err := paramInt(a.Params, "gpu_index")
	if err != nil {
		return err
	}
	var lastErr error
	for {
		if err := e.driver.EnsureIdle(ctx, idx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		timer := time.NewTimer(waitIdlePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait_idle: GPU %d did not become idle: %w", idx, lastErr)
		case <-timer.C:
		}
	}
}

// runDiag runs `dcgmi diag -r <level>` (DCGM ships with the GPU stack). The
// diag levels are 1 (seconds), 2 (minutes), 3 (tens of minutes).
func (e *Executor) runDiag(ctx context.Context, a types.Action, res *types.ActionResult) error {
	level := a.Params["diag_level"]
	if level == "" {
		level = "1"
	}
	if level != "1" && level != "2" && level != "3" {
		return fmt.Errorf("run_diag: invalid diag_level %q", level)
	}
	out, err := e.run(ctx, "dcgmi", "diag", "-r", level)
	res.Output = truncateOutput(out)
	if err != nil {
		return fmt.Errorf("dcgmi diag -r %s: %w", level, err)
	}
	return nil
}

// collectBundle runs nvidia-bug-report.sh into the bundle directory. The
// resulting path is the action output; shipping the bundle off-node is a
// separate concern (operators fetch it or a future step uploads it).
func (e *Executor) collectBundle(ctx context.Context, a types.Action, res *types.ActionResult) error {
	if err := os.MkdirAll(e.BundleDir, 0o700); err != nil {
		return fmt.Errorf("collect_bundle: %w", err)
	}
	base := filepath.Join(e.BundleDir, "bundle-"+a.ID)
	out, err := e.run(ctx, "nvidia-bug-report.sh", "--output-file", base)
	if err != nil {
		res.Output = truncateOutput(out)
		return fmt.Errorf("nvidia-bug-report.sh: %w", err)
	}
	// The script appends .gz itself.
	res.Output = base + ".gz"
	return nil
}

// reboot is idempotent on the boot_id parameter: when the controller stamps
// the boot ID it observed and the node has already rebooted since (IDs
// differ), the action reports success without rebooting again. Without the
// guard a controller retry after the reboot would bounce the node forever.
func (e *Executor) reboot(ctx context.Context, a types.Action, res *types.ActionResult) error {
	current, err := e.readBootID()
	if err != nil {
		return fmt.Errorf("reboot: reading boot ID: %w", err)
	}
	expected := a.Params["boot_id"]
	if expected == "" {
		return fmt.Errorf("reboot: boot_id param is required (idempotency guard)")
	}
	if expected != current {
		res.Output = fmt.Sprintf("already rebooted: boot ID %s differs from requested %s", current, expected)
		return nil
	}
	command := e.rebootCommand()
	if len(command) == 0 {
		return fmt.Errorf("reboot: no reboot command is configured")
	}
	out, err := e.run(ctx, command[0], command[1:]...)
	if err != nil {
		res.Output = truncateOutput(out)
		return fmt.Errorf("%s: %w", strings.Join(command, " "), err)
	}
	res.Output = "reboot initiated (boot ID " + current + ")"
	return nil
}

func (e *Executor) rebootCommand() []string {
	if len(e.RebootCommand) != 0 {
		return e.RebootCommand
	}
	return defaultRebootCommand()
}

// PreflightReboot reports whether the configured reboot mechanism can actually
// reach the host. A node that cannot reboot must say so at startup, not at the
// moment an operator approves the most destructive step in a playbook — the
// first hardware run failed exactly there, with `systemctl reboot: exit status
// 127` arriving after the approval.
//
// The check runs the configured command's namespace-entry prefix against
// `true`, so it proves reachability without rebooting anything. A command with
// no recognizable prefix cannot be probed and is reported as unverifiable
// rather than silently assumed good.
func (e *Executor) PreflightReboot(ctx context.Context) error {
	command := e.rebootCommand()
	if len(command) == 0 {
		return fmt.Errorf("no reboot command is configured")
	}
	probe, ok := rebootProbeCommand(command)
	if !ok {
		return fmt.Errorf("reboot command %q cannot be preflighted; it will only be exercised by a real reboot", strings.Join(command, " "))
	}
	ctx, cancel := context.WithTimeout(ctx, rebootPreflightTimeout)
	defer cancel()
	if out, err := e.run(ctx, probe[0], probe[1:]...); err != nil {
		return fmt.Errorf("%s: %w (output: %s)", strings.Join(probe, " "), err, truncateOutput(out))
	}
	return nil
}

// rebootProbeCommand turns a reboot command into a harmless equivalent: the
// part that enters the host's namespaces is kept, and whatever it would have
// run there is replaced with `true`.
func rebootProbeCommand(command []string) ([]string, bool) {
	for i, arg := range command {
		if arg == "--" && i+1 < len(command) {
			probe := append([]string(nil), command[:i+1]...)
			return append(probe, "true"), true
		}
	}
	return nil, false
}

// runAllowListedScript executes one operator-provisioned script from
// ScriptsDir. The allow list is the directory content itself: the script
// must already exist there with the execute bit set, and the name is
// validated so configuration can never traverse outside the directory. No
// action parameters reach the script — its behavior is fully owned by
// whoever provisioned the file.
func (e *Executor) runAllowListedScript(ctx context.Context, name string, a types.Action, res *types.ActionResult) error {
	if _, err := scriptName(name); err != nil {
		return err
	}
	path := filepath.Join(e.ScriptsDir, name)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: script %s is not provisioned in %s (operators must place it there explicitly): %w",
			a.Type, name, e.ScriptsDir, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s: %s is not an executable script", a.Type, path)
	}
	out, err := e.run(ctx, path)
	res.Output = truncateOutput(out)
	if err != nil {
		return fmt.Errorf("%s: %s failed: %w", a.Type, name, err)
	}
	return nil
}

// scriptName validates an allow-listed script selector: a bare *.sh
// filename with a conservative character set, no separators, no traversal.
func scriptName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("run_script: param \"script\" is required")
	}
	if !scriptNameRe.MatchString(name) {
		return "", fmt.Errorf("script name %q is not allowed: must match %s", name, scriptNameRe.String())
	}
	return name, nil
}

var scriptNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\.sh$`)

func truncateOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > maxCapturedOutput {
		return s[:maxCapturedOutput] + "\n[truncated]"
	}
	return s
}

func paramInt(params map[string]string, key string) (int, error) {
	s, ok := params[key]
	if !ok {
		return 0, fmt.Errorf("missing required param %q", key)
	}
	// Strict whole-string parse: fmt.Sscanf("%d") accepts trailing garbage
	// ("5abc" -> 5), which would silently misread a malformed parameter. Reject
	// anything that is not a clean integer so the action fails closed.
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("param %q: %w", key, err)
	}
	return n, nil
}

// armedSource resolves the effective arming source: the live function when
// provided, else the static option frozen for the process lifetime.
func armedSource(options Options) func() bool {
	if options.Armed != nil {
		return options.Armed
	}
	static := options.EnableDestructiveActions
	return func() bool { return static }
}
