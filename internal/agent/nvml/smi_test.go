package nvml

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

const smiInventory = ` 0, GPU-aaaa-1111, NVIDIA H100 80GB HBM3, 00000000:3B:00.0
1, GPU-bbbb-2222, NVIDIA H100 80GB HBM3, 00000000:AF:00.0
`

// scriptedRunner returns canned output per subcommand.
type scriptedRunner struct {
	calls []string
	fail  map[string]error
}

func (s *scriptedRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(args, " ")
	s.calls = append(s.calls, call)
	for prefix, err := range s.fail {
		if strings.HasPrefix(call, prefix) {
			return []byte("simulated failure detail"), err
		}
	}
	if strings.HasPrefix(call, "--query-gpu=index") {
		return []byte(smiInventory), nil
	}
	return nil, nil
}

func newTestSMI(fail map[string]error) (*SMI, *scriptedRunner) {
	r := &scriptedRunner{fail: fail}
	s := NewSMI("")
	s.run = r.run
	return s, r
}

func TestSMIListGPUs(t *testing.T) {
	s, _ := newTestSMI(nil)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	gpus, err := s.ListGPUs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 2 {
		t.Fatalf("gpus = %d, want 2", len(gpus))
	}
	if gpus[0].UUID != "GPU-aaaa-1111" || gpus[0].Index != 0 || !strings.Contains(gpus[0].Model, "H100") {
		t.Fatalf("gpu[0] = %+v", gpus[0])
	}
}

func TestSMIGPUByPCIAddrMatchesKmsgFormat(t *testing.T) {
	s, _ := newTestSMI(nil)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	// kmsg prints lowercase, no function, sometimes with PCI: prefix already
	// stripped by the watcher.
	for addr, wantUUID := range map[string]string{
		"0000:3b:00": "GPU-aaaa-1111",
		"0000:af:00": "GPU-bbbb-2222",
		"3b:00":      "GPU-aaaa-1111", // domainless spelling
	} {
		gpu, err := s.GPUByPCIAddr(context.Background(), addr)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		if gpu.UUID != wantUUID {
			t.Fatalf("%s -> %s, want %s", addr, gpu.UUID, wantUUID)
		}
	}
	if _, err := s.GPUByPCIAddr(context.Background(), "0000:ff:00"); err == nil {
		t.Fatal("unknown PCI address must fail after one refresh")
	}
}

func TestSMIResetAndHealthy(t *testing.T) {
	s, r := newTestSMI(nil)
	if err := s.ResetGPU(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range r.calls {
		if call == "--gpu-reset -i 1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("calls = %v, want gpu-reset -i 1", r.calls)
	}
	if err := s.Healthy(context.Background()); err != nil {
		t.Fatal(err)
	}

	failing, _ := newTestSMI(map[string]error{"--query-gpu=count": errors.New("hang")})
	if err := failing.Healthy(context.Background()); err == nil || !strings.Contains(err.Error(), "simulated failure detail") {
		t.Fatalf("wedged driver probe = %v, want failure with output detail", err)
	}
}

// TestSMIRefreshImposesItsOwnDeadline is the regression for the unbounded
// nvidia-smi in the XID hot path. refresh runs on the agent's main Run loop with
// the process-lifetime context (no deadline); a driver that wedges nvidia-smi
// when a GPU falls off the bus would otherwise hang that loop forever. refresh
// must bound itself and return promptly instead.
func TestSMIRefreshImposesItsOwnDeadline(t *testing.T) {
	s := NewSMI("")
	s.queryTimeout = 20 * time.Millisecond
	// A wedged driver: nvidia-smi never returns on its own; it ends only when the
	// context refresh derived is cancelled.
	s.run = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	done := make(chan error, 1)
	go func() {
		// context.Background() carries NO deadline, exactly like the hot path.
		_, err := s.GPUByPCIAddr(context.Background(), "0000:3b:00")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("GPUByPCIAddr must return a timeout error when nvidia-smi wedges, not succeed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GPUByPCIAddr hung well past refresh's own deadline; the main loop would be wedged")
	}
}

// TestSMIResetGPUByUUID is the regression for the reset landing on a renumbered
// neighbor: the reset must target the stable UUID, not an integer index.
func TestSMIResetGPUByUUID(t *testing.T) {
	s, r := newTestSMI(nil)
	if err := s.ResetGPUByUUID(context.Background(), "GPU-aaaa-1111"); err != nil {
		t.Fatal(err)
	}
	if !hasCall(r.calls, "--gpu-reset -i GPU-aaaa-1111") {
		t.Fatalf("calls = %v, want the reset issued by UUID", r.calls)
	}

	failing, _ := newTestSMI(map[string]error{"--gpu-reset": errors.New("reset failed")})
	if err := failing.ResetGPUByUUID(context.Background(), "GPU-bbbb-2222"); err == nil || !strings.Contains(err.Error(), "GPU-bbbb-2222") {
		t.Fatalf("failed reset = %v, want the UUID named in the error", err)
	}
}

// TestSMISetPersistenceModeScopesToTargetGPU is the regression for the node-wide
// persistence toggle: -pm must carry -i <index> so a per-GPU quiesce cannot flip
// persistence for every GPU on the node.
func TestSMISetPersistenceModeScopesToTargetGPU(t *testing.T) {
	s, r := newTestSMI(nil)
	if err := s.SetPersistenceMode(context.Background(), 2, true); err != nil {
		t.Fatal(err)
	}
	if !hasCall(r.calls, "-pm 1 -i 2") {
		t.Fatalf("calls = %v, want persistence enabled only on GPU 2", r.calls)
	}
	if err := s.SetPersistenceMode(context.Background(), 5, false); err != nil {
		t.Fatal(err)
	}
	if !hasCall(r.calls, "-pm 0 -i 5") {
		t.Fatalf("calls = %v, want persistence disabled only on GPU 5", r.calls)
	}
}

func TestSMIPartitionTopologyFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		output  string
		want    string
		wantErr bool
	}{
		"all disabled": {output: "Disabled\nDisabled\n", want: "none"},
		"one enabled":  {output: "Disabled\nEnabled\n", want: "mig"},
		// A MIG-incapable device (T4, V100, consumer parts) reports N/A.
		// That is evidence it cannot be partitioned, not missing evidence:
		// treating it as unknown would make every such GPU permanently
		// ineligible for a reset that requires verified topology.
		"MIG-incapable device":   {output: "[N/A]\n[N/A]\n", want: "none"},
		"lowercase n/a":          {output: "n/a\nn/a\n", want: "none"},
		"mixed N/A and disabled": {output: "[N/A]\nDisabled\n", want: "none"},
		"unrecognised state":     {output: "Weird\nWeird\n", want: "unknown"},
		"partial device list":    {output: "Disabled\n", want: "unknown", wantErr: true},
		"partial list sees MIG":  {output: "Enabled\n", want: "mig"},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestSMI(nil)
			s.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if strings.HasPrefix(strings.Join(args, " "), "--query-gpu=index") {
					return []byte(smiInventory), nil
				}
				if strings.HasPrefix(strings.Join(args, " "), "--query-gpu=mig.mode.current") {
					return []byte(tc.output), nil
				}
				return nil, nil
			}
			if err := s.Init(); err != nil {
				t.Fatal(err)
			}
			got, err := s.PartitionTopology(context.Background())
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("PartitionTopology() = %q, %v; want %q, error=%t", got, err, tc.want, tc.wantErr)
			}
		})
	}

	s, _ := newTestSMI(map[string]error{"--query-gpu=mig.mode.current": errors.New("query failed")})
	got, err := s.PartitionTopology(context.Background())
	if err == nil || got != "unknown" || !strings.Contains(err.Error(), "simulated failure detail") {
		t.Fatalf("failed MIG probe = %q, %v; want unknown with output detail", got, err)
	}
}

func TestSMIDriverVersionRequiresCompleteUniformEvidence(t *testing.T) {
	for name, tc := range map[string]struct {
		output  string
		want    string
		wantErr bool
	}{
		"uniform": {output: "570.86.15\n570.86.15\n", want: "570.86.15"},
		"mixed":   {output: "570.86.15\n550.54.15\n", wantErr: true},
		"partial": {output: "570.86.15\n", wantErr: true},
		"missing": {output: "[N/A]\n[N/A]\n", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestSMI(nil)
			s.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				query := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(query, "--query-gpu=index"):
					return []byte(smiInventory), nil
				case strings.HasPrefix(query, "--query-gpu=driver_version"):
					return []byte(tc.output), nil
				default:
					return nil, nil
				}
			}
			got, err := s.DriverVersion(context.Background())
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("DriverVersion() = %q, %v; want %q, error=%t", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// TestSMIEnsureIdleByUUID is the regression for the reset preflight being bound
// to a stale index while the reset targets a UUID: the idle check must address
// the same stable UUID with -i.
func TestSMIEnsureIdleByUUID(t *testing.T) {
	var selector string
	s := NewSMI("")
	s.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		for i, a := range args {
			if a == "-i" && i+1 < len(args) {
				selector = args[i+1]
			}
		}
		return nil, nil
	}
	if err := s.EnsureIdleByUUID(context.Background(), "GPU-aaaa-1111"); err != nil {
		t.Fatal(err)
	}
	if selector != "GPU-aaaa-1111" {
		t.Fatalf("idle check -i selector = %q, want the UUID", selector)
	}

	busy := NewSMI("")
	busy.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "query-compute-apps") {
			return []byte("12345\n"), nil
		}
		return nil, nil
	}
	if err := busy.EnsureIdleByUUID(context.Background(), "GPU-bbbb-2222"); err == nil ||
		!strings.Contains(err.Error(), "GPU-bbbb-2222") || !strings.Contains(err.Error(), "not idle") {
		t.Fatalf("busy GPU = %v, want a not-idle error naming the UUID", err)
	}
}

// TestSMISetPersistenceModeByUUID is the regression for restore re-applying
// persistence by a stale index: -pm must carry -i <uuid> so the toggle stays on
// the original device across an enumeration shift.
func TestSMISetPersistenceModeByUUID(t *testing.T) {
	s, r := newTestSMI(nil)
	if err := s.SetPersistenceModeByUUID(context.Background(), "GPU-aaaa-1111", true); err != nil {
		t.Fatal(err)
	}
	if !hasCall(r.calls, "-pm 1 -i GPU-aaaa-1111") {
		t.Fatalf("calls = %v, want persistence enabled by UUID", r.calls)
	}
	if err := s.SetPersistenceModeByUUID(context.Background(), "GPU-bbbb-2222", false); err != nil {
		t.Fatal(err)
	}
	if !hasCall(r.calls, "-pm 0 -i GPU-bbbb-2222") {
		t.Fatalf("calls = %v, want persistence disabled by UUID", r.calls)
	}
}

func TestSMIInitFailsWithoutGPUs(t *testing.T) {
	s, _ := newTestSMI(map[string]error{"--query-gpu=index": fmt.Errorf("no devices")})
	if err := s.Init(); err == nil {
		t.Fatal("Init must fail when nvidia-smi fails")
	}
}

func TestNormalizePCI(t *testing.T) {
	for in, want := range map[string]string{
		"00000000:3B:00.0": "0000:3b:00",
		"0000:3b:00":       "0000:3b:00",
		"3b:00":            "0000:3b:00",
		"0001:AF:00.0":     "0001:af:00",
	} {
		if got := NormalizePCI(in); got != want {
			t.Errorf("NormalizePCI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSMIEnsureIdle(t *testing.T) {
	// No processes: idle.
	idle, _ := newTestSMI(nil)
	if err := idle.EnsureIdle(context.Background(), 0); err != nil {
		t.Fatalf("idle GPU = %v", err)
	}

	// Compute process attached: busy.
	busy := NewSMI("")
	busy.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "query-compute-apps") {
			return []byte("12345\n"), nil
		}
		return nil, nil
	}
	if err := busy.EnsureIdle(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "not idle") {
		t.Fatalf("busy GPU = %v, want not-idle error", err)
	}

	// Probe failure fails closed.
	failing, _ := newTestSMI(map[string]error{"--query-compute-apps=pid": errors.New("hang")})
	if err := failing.EnsureIdle(context.Background(), 0); err == nil {
		t.Fatal("failed probe must fail closed")
	}
}
