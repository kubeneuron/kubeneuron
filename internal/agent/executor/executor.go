// Package executor runs controller-requested actions locally on the node
// and enforces idempotency: action IDs are deterministic, and completed IDs
// are cached so a controller retry after a crash re-returns the original
// result instead of re-executing.
package executor

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
)

// Options controls capabilities that must never be enabled implicitly.
type Options struct {
	// EnableDestructiveActions permits actions that can alter node or device
	// state. It must be enabled explicitly by the production agent
	// configuration. It is still blocked from go test binaries unless
	// KUBENEURON_DESTRUCTIVE_LAB=1 is set for a dedicated hardware lab run.
	EnableDestructiveActions bool
}

// Executor executes actions on the local node.
type Executor struct {
	driver nvml.GPUDriver
	// BundleDir receives collect_bundle output; default defaultBundleDir.
	BundleDir string
	// ScriptsDir holds operator-provisioned scripts; default defaultScriptsDir.
	ScriptsDir string

	// run executes a subprocess and returns combined output; tests inject it.
	run runner
	// readBootID returns the current boot ID; tests inject it.
	readBootID func() (string, error)
	// enableDestructiveActions is deliberately opt-in. A normal executor is
	// observation-only with respect to host and GPU state.
	enableDestructiveActions bool

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
	return &Executor{
		driver:                   driver,
		BundleDir:                defaultBundleDir,
		ScriptsDir:               defaultScriptsDir,
		run:                      execRunner,
		readBootID:               currentBootID,
		enableDestructiveActions: options.EnableDestructiveActions,
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
		idx, err := paramInt(a.Params, "gpu_index")
		if err != nil {
			return err
		}
		// Final safety net after the drain: never reset a GPU that still has
		// processes attached.
		if err := e.driver.EnsureIdle(ctx, idx); err != nil {
			return err
		}
		return e.driver.ResetGPU(ctx, idx)
	case types.ActionRunDiag:
		return e.runDiag(ctx, a, res)
	case types.ActionCollectBundle:
		return e.collectBundle(ctx, a, res)
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
	if !e.enableDestructiveActions {
		return fmt.Errorf("%s: destructive actions are disabled; enable them explicitly in the agent configuration", action)
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
	out, err := e.run(ctx, "systemctl", "reboot")
	if err != nil {
		res.Output = truncateOutput(out)
		return fmt.Errorf("systemctl reboot: %w", err)
	}
	res.Output = "reboot initiated (boot ID " + current + ")"
	return nil
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
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("param %q: %w", key, err)
	}
	return n, nil
}
