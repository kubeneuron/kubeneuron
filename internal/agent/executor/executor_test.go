package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type scripted struct {
	calls   []string
	output  string
	err     error
	outputs []string
	errs    []error
}

func (s *scripted) run(_ context.Context, name string, args ...string) ([]byte, error) {
	i := len(s.calls)
	s.calls = append(s.calls, name+" "+strings.Join(args, " "))
	output, err := s.output, s.err
	if i < len(s.outputs) {
		output = s.outputs[i]
	}
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return []byte(output), err
}

func newTestExecutor(t *testing.T, bootID string, run *scripted) *Executor {
	t.Helper()
	e := New(&nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-1"}}})
	e.BundleDir = t.TempDir()
	e.run = run.run
	e.readBootID = func() (string, error) { return bootID, nil }
	return e
}

func TestIdempotencyCacheReplaysResult(t *testing.T) {
	run := &scripted{output: "diag ok"}
	e := newTestExecutor(t, "boot-1", run)
	a := types.Action{ID: "a1", Type: types.ActionRunDiag, Params: map[string]string{"diag_level": "1"}}

	first, err := e.Execute(context.Background(), a)
	if err != nil || !first.OK {
		t.Fatalf("first execute = %+v, %v", first, err)
	}
	second, err := e.Execute(context.Background(), a)
	if err != nil || second.Output != first.Output {
		t.Fatalf("replay = %+v, %v", second, err)
	}
	if len(run.calls) != 1 {
		t.Fatalf("subprocess calls = %d, want 1 (replay must not re-execute)", len(run.calls))
	}
}

func TestConcurrentActionIDExecutesOnlyOnce(t *testing.T) {
	e := newTestExecutor(t, "boot-1", &scripted{})
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	e.run = func(context.Context, string, ...string) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return []byte("diag ok"), nil
	}
	action := types.Action{ID: "one-at-a-time", Type: types.ActionRunDiag}

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := e.Execute(context.Background(), action)
			if err != nil || result == nil || !result.OK {
				results <- fmt.Errorf("Execute() = %#v, %v", result, err)
			}
		}()
	}
	<-started
	select {
	case <-time.After(50 * time.Millisecond):
	case err := <-results:
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent executions = %d, want 1", got)
	}
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		t.Error(err)
	}
}

func TestIdleActionsUseGPUDriver(t *testing.T) {
	e := newTestExecutor(t, "boot-1", &scripted{})
	for _, action := range []types.Action{
		{ID: "idle", Type: types.ActionIdleCheck, Params: map[string]string{"gpu_index": "0"}},
		{ID: "wait", Type: types.ActionWaitIdle, Params: map[string]string{"gpu_index": "0"}, Timeout: time.Second},
	} {
		result, err := e.Execute(context.Background(), action)
		if err != nil || result == nil || !result.OK {
			t.Fatalf("%s = %#v, %v", action.Type, result, err)
		}
	}
	if _, err := e.Execute(context.Background(), types.Action{ID: "wait-no-timeout", Type: types.ActionWaitIdle, Params: map[string]string{"gpu_index": "0"}}); err == nil {
		t.Fatal("wait_idle without an action timeout must fail")
	}
}

func TestRunDiagValidatesLevelAndCapturesOutput(t *testing.T) {
	run := &scripted{output: "Deployment: Pass"}
	e := newTestExecutor(t, "boot-1", run)

	res, err := e.Execute(context.Background(), types.Action{
		ID: "d1", Type: types.ActionRunDiag, Params: map[string]string{"diag_level": "2"},
	})
	if err != nil || !strings.Contains(res.Output, "Pass") {
		t.Fatalf("diag = %+v, %v", res, err)
	}
	if run.calls[0] != "dcgmi diag -r 2" {
		t.Fatalf("call = %q", run.calls[0])
	}

	if _, err := e.Execute(context.Background(), types.Action{
		ID: "d2", Type: types.ActionRunDiag, Params: map[string]string{"diag_level": "9"},
	}); err == nil {
		t.Fatal("invalid diag level must fail")
	}
}

func TestCollectBundleReturnsPath(t *testing.T) {
	run := &scripted{}
	e := newTestExecutor(t, "boot-1", run)

	res, err := e.Execute(context.Background(), types.Action{ID: "b1", Type: types.ActionCollectBundle})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Output, e.BundleDir) || !strings.HasSuffix(res.Output, "bundle-b1.gz") {
		t.Fatalf("bundle path = %q", res.Output)
	}
	if !strings.HasPrefix(run.calls[0], "nvidia-bug-report.sh --output-file") {
		t.Fatalf("call = %q", run.calls[0])
	}
}

func TestDestructiveActionsRequireExplicitEnablement(t *testing.T) {
	run := &scripted{}
	e := newTestExecutor(t, "boot-1", run)

	for _, actionType := range []types.ActionType{
		types.ActionGPUReset,
		types.ActionReboot,
		types.ActionDriverReload,
		types.ActionDriverReinstall,
		types.ActionRunScript,
		types.ActionPowerCycle,
	} {
		t.Run(string(actionType), func(t *testing.T) {
			res, err := e.Execute(context.Background(), types.Action{
				ID:     "disabled-" + string(actionType),
				Type:   actionType,
				Params: map[string]string{"gpu_index": "0", "boot_id": "boot-1", "script": "safe.sh"},
			})
			if err == nil || res == nil || res.OK || !strings.Contains(res.Error, "disabled") {
				t.Fatalf("Execute(%s) = %+v, %v; want disabled destructive-action error", actionType, res, err)
			}
		})
	}
	if len(run.calls) != 0 {
		t.Fatalf("subprocess calls = %v, want none", run.calls)
	}
}

func TestDestructiveActionsStayBlockedInGoTestAfterExplicitEnablement(t *testing.T) {
	t.Setenv(destructiveLabEnv, "")
	run := &scripted{}
	e := NewWithOptions(&nvml.Fake{}, Options{EnableDestructiveActions: true})
	e.run = run.run
	e.readBootID = func() (string, error) { return "boot-1", nil }

	res, err := e.Execute(context.Background(), types.Action{
		ID: "test-binary-reboot", Type: types.ActionReboot, Params: map[string]string{"boot_id": "boot-1"},
	})
	if err == nil || res == nil || res.OK || !strings.Contains(res.Error, destructiveLabEnv) {
		t.Fatalf("Execute(reboot) = %+v, %v; want go test lab guard", res, err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("subprocess calls = %v, want none", run.calls)
	}
}

func TestRebootGuardedByBootID(t *testing.T) {
	// Node already rebooted: requested boot ID differs from current.
	run := &scripted{}
	e := newTestExecutor(t, "boot-NEW", run)
	res := &types.ActionResult{}
	err := e.reboot(context.Background(), types.Action{
		ID: "r1", Type: types.ActionReboot, Params: map[string]string{"boot_id": "boot-OLD"},
	}, res)
	if err != nil {
		t.Fatalf("guarded reboot = %+v, %v", res, err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("calls = %v, want none (already rebooted)", run.calls)
	}
	if !strings.Contains(res.Output, "already rebooted") {
		t.Fatalf("output = %q", res.Output)
	}

	// Matching boot ID: reboot proceeds.
	run2 := &scripted{}
	e2 := newTestExecutor(t, "boot-SAME", run2)
	res = &types.ActionResult{}
	err = e2.reboot(context.Background(), types.Action{
		ID: "r2", Type: types.ActionReboot, Params: map[string]string{"boot_id": "boot-SAME"},
	}, res)
	if err != nil {
		t.Fatalf("reboot = %+v, %v", res, err)
	}
	// The host's init is only reachable through PID 1's namespaces; a bare
	// "systemctl reboot" from the distroless agent image exits 127.
	if len(run2.calls) != 1 || run2.calls[0] != "/bin/nsenter -t 1 -m -u -i -n -p -- systemctl reboot" {
		t.Fatalf("calls = %v", run2.calls)
	}

	// Missing boot ID: refused outright — never reaches systemctl. Without
	// the guard param a replayed action could reboot the node forever.
	run3 := &scripted{}
	e3 := newTestExecutor(t, "boot-ANY", run3)
	res = &types.ActionResult{}
	err = e3.reboot(context.Background(), types.Action{
		ID: "r3", Type: types.ActionReboot,
	}, res)
	if err == nil || !strings.Contains(err.Error(), "boot_id") {
		t.Fatalf("unguarded reboot = %+v, %v, want boot_id-required failure", res, err)
	}
	if len(run3.calls) != 0 {
		t.Fatalf("calls = %v, want none (refused)", run3.calls)
	}
}

func TestFailedActionCachesFailure(t *testing.T) {
	run := &scripted{err: errors.New("dcgmi missing")}
	e := newTestExecutor(t, "boot-1", run)
	a := types.Action{ID: "f1", Type: types.ActionRunDiag}

	res, err := e.Execute(context.Background(), a)
	if err == nil || res.OK {
		t.Fatalf("failure = %+v, %v", res, err)
	}
	// A replay returns the recorded failure without re-running the command.
	res2, err2 := e.Execute(context.Background(), a)
	if err2 != nil {
		t.Fatalf("cached replay must not return a transport error: %v", err2)
	}
	if res2.OK || res2.Error == "" {
		t.Fatalf("cached failure = %+v", res2)
	}
	if len(run.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(run.calls))
	}
}

func TestAllowListedScripts(t *testing.T) {
	run := &scripted{output: "reloaded"}
	e := newTestExecutor(t, "boot-1", run)
	e.ScriptsDir = t.TempDir()

	// Not provisioned: fails with an actionable message, nothing executes.
	err := e.runAllowListedScript(context.Background(), "driver_reload.sh", types.Action{ID: "s1", Type: types.ActionDriverReload}, &types.ActionResult{})
	if err == nil || !strings.Contains(err.Error(), "not provisioned") {
		t.Fatalf("missing script = %v, want not-provisioned error", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("calls = %v, want none", run.calls)
	}

	// Provisioned executable script runs by its fixed name.
	path := e.ScriptsDir + "/driver_reload.sh"
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	res := &types.ActionResult{}
	err = e.runAllowListedScript(context.Background(), "driver_reload.sh", types.Action{ID: "s2", Type: types.ActionDriverReload}, res)
	if err != nil {
		t.Fatalf("provisioned script = %+v, %v", res, err)
	}
	if len(run.calls) != 1 || !strings.HasSuffix(run.calls[0], "driver_reload.sh ") && !strings.HasSuffix(run.calls[0], "driver_reload.sh") {
		t.Fatalf("calls = %v", run.calls)
	}

	// Non-executable file is rejected.
	if err := os.WriteFile(e.ScriptsDir+"/driver_reinstall.sh", []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.runAllowListedScript(context.Background(), "driver_reinstall.sh", types.Action{ID: "s3", Type: types.ActionDriverReinstall}, &types.ActionResult{}); err == nil ||
		!strings.Contains(err.Error(), "not an executable script") {
		t.Fatalf("non-executable = %v", err)
	}
}

func TestRunScriptNameValidation(t *testing.T) {
	run := &scripted{}
	e := newTestExecutor(t, "boot-1", run)
	e.ScriptsDir = t.TempDir()

	for _, bad := range []string{"", "../etc/passwd.sh", "/abs/path.sh", "no-extension", "UPPER.sh", "a/b.sh", "..sh"} {
		_, err := scriptName(bad)
		if err == nil {
			t.Fatalf("script name %q must be rejected", bad)
		}
		if len(run.calls) != 0 {
			t.Fatalf("calls = %v after %q, want none", run.calls, bad)
		}
	}

	if err := os.WriteFile(e.ScriptsDir+"/clean_fabric.sh", []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	res := &types.ActionResult{}
	err := e.runAllowListedScript(context.Background(), "clean_fabric.sh", types.Action{ID: "good", Type: types.ActionRunScript}, res)
	if err != nil {
		t.Fatalf("valid script = %+v, %v", res, err)
	}
}

// TestParamIntRejectsTrailingGarbage is the regression test for paramInt
// accepting "5abc" as 5 under fmt.Sscanf("%d"). A malformed controller-authored
// parameter must fail closed rather than being silently misread.
func TestParamIntRejectsTrailingGarbage(t *testing.T) {
	for _, good := range []struct {
		in   string
		want int
	}{{"5", 5}, {"  42 ", 42}, {"-3", -3}, {"0", 0}} {
		got, err := paramInt(map[string]string{"k": good.in}, "k")
		if err != nil || got != good.want {
			t.Fatalf("paramInt(%q) = (%d, %v), want (%d, nil)", good.in, got, err, good.want)
		}
	}
	for _, bad := range []string{"5abc", "5 6", "0x10", "", "abc", "5.0", "1_000", "+"} {
		if got, err := paramInt(map[string]string{"k": bad}, "k"); err == nil {
			t.Fatalf("paramInt(%q) = (%d, nil), want an error (fail closed)", bad, got)
		}
	}
	if _, err := paramInt(map[string]string{}, "missing"); err == nil {
		t.Fatal("paramInt with a missing key must error")
	}
}

// Round-9: undoing is always allowed. An UNARMED executor must not refuse
// restore_accelerator_host on the arming boundary (it can only replay a
// prior quiesce's crash-safe snapshot) — under go test only the lab guard
// may block it. Every other destructive action still fails the arming check
// first.
func TestUnarmedExecutorAllowsHostRestoreOnly(t *testing.T) {
	e := NewWithOptions(&nvml.Fake{}, Options{}) // unarmed

	err := e.allowDestructiveAction(types.ActionRestoreAcceleratorHost)
	if err == nil {
		t.Fatal("under go test the lab guard must still block execution")
	}
	if strings.Contains(err.Error(), "destructive actions are disabled on this node") {
		t.Fatalf("restore was refused on the ARMING boundary: %v — undo must be allowed unarmed", err)
	}
	for _, act := range []types.ActionType{types.ActionReboot, types.ActionGPUReset, types.ActionQuiesceAcceleratorHost} {
		err := e.allowDestructiveAction(act)
		if err == nil || !strings.Contains(err.Error(), "destructive actions are disabled on this node") {
			t.Fatalf("%s unarmed = %v, want the arming refusal", act, err)
		}
	}
}
