package nvml

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// SMI is the real GPUDriver, backed by the nvidia-smi binary that ships with
// every NVIDIA driver installation. Shelling out keeps the agent a CGO-free
// static binary (the go-nvml cgo bindings would break the scratch images);
// nvidia-smi is also exactly what a human operator would run, so behavior is
// easy to reason about and reproduce.
type SMI struct {
	// Path is the nvidia-smi binary, default "nvidia-smi" from PATH.
	Path string

	// run executes a command and returns combined output; tests inject it.
	run runner

	mu   sync.Mutex
	gpus []smiGPU
}

// runner abstracts subprocess execution for tests.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

type smiGPU struct {
	info types.GPUInfo
	pci  string // normalized PCI address
}

const (
	smiQueryTimeout = 10 * time.Second
	smiResetTimeout = 2 * time.Minute
	smiProbeTimeout = 5 * time.Second
)

var _ GPUDriver = (*SMI)(nil)

// NewSMI builds an nvidia-smi-backed driver. path empty means "nvidia-smi".
func NewSMI(path string) *SMI {
	if path == "" {
		path = "nvidia-smi"
	}
	return &SMI{Path: path, run: execRunner}
}

// DetectSMI reports whether nvidia-smi is available on this node.
func DetectSMI(path string) bool {
	if path == "" {
		path = "nvidia-smi"
	}
	_, err := exec.LookPath(path)
	return err == nil
}

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Init verifies the driver responds and loads the GPU inventory.
func (s *SMI) Init() error {
	ctx, cancel := context.WithTimeout(context.Background(), smiQueryTimeout)
	defer cancel()
	if _, err := s.refresh(ctx); err != nil {
		return fmt.Errorf("nvidia-smi: %w", err)
	}
	return nil
}

// Shutdown implements GPUDriver; nothing is held open.
func (s *SMI) Shutdown() error { return nil }

// refresh queries the inventory and caches it under the lock.
func (s *SMI) refresh(ctx context.Context) ([]smiGPU, error) {
	out, err := s.run(ctx, s.Path,
		"--query-gpu=index,uuid,name,pci.bus_id", "--format=csv,noheader")
	if err != nil {
		return nil, fmt.Errorf("query-gpu failed: %w (output: %s)", err, firstLine(out))
	}
	var gpus []smiGPU
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 4 {
			return nil, fmt.Errorf("unexpected query-gpu line %q", line)
		}
		index, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			return nil, fmt.Errorf("unexpected GPU index in %q", line)
		}
		gpus = append(gpus, smiGPU{
			info: types.GPUInfo{
				Index: index,
				UUID:  strings.TrimSpace(fields[1]),
				Model: strings.TrimSpace(fields[2]),
			},
			pci: NormalizePCI(strings.TrimSpace(fields[3])),
		})
	}
	if len(gpus) == 0 {
		return nil, fmt.Errorf("no GPUs reported")
	}
	s.mu.Lock()
	s.gpus = gpus
	s.mu.Unlock()
	return gpus, nil
}

// ListGPUs implements GPUDriver.
func (s *SMI) ListGPUs(ctx context.Context) ([]types.GPUInfo, error) {
	gpus, err := s.refresh(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]types.GPUInfo, len(gpus))
	for i, g := range gpus {
		out[i] = g.info
	}
	return out, nil
}

// GPUByPCIAddr implements GPUDriver. XID lines print addresses like
// "0000:3b:00" while nvidia-smi reports "00000000:3B:00.0"; both normalize
// to the same "dddd:bb:dd" key.
func (s *SMI) GPUByPCIAddr(ctx context.Context, pciAddr string) (types.GPUInfo, error) {
	want := NormalizePCI(pciAddr)
	s.mu.Lock()
	cached := s.gpus
	s.mu.Unlock()
	if gpu, ok := findByPCI(cached, want); ok {
		return gpu, nil
	}
	// Miss: refresh once (hotplug/driver restart) and retry.
	refreshed, err := s.refresh(ctx)
	if err != nil {
		return types.GPUInfo{}, err
	}
	if gpu, ok := findByPCI(refreshed, want); ok {
		return gpu, nil
	}
	return types.GPUInfo{}, fmt.Errorf("no GPU with PCI address %s (normalized %s)", pciAddr, want)
}

func findByPCI(gpus []smiGPU, pci string) (types.GPUInfo, bool) {
	for _, g := range gpus {
		if g.pci == pci {
			return g.info, true
		}
	}
	return types.GPUInfo{}, false
}

// ResetGPU implements GPUDriver. The caller drains workloads first; the
// driver refuses the reset itself when processes still hold the GPU.
func (s *SMI) ResetGPU(ctx context.Context, index int) error {
	ctx, cancel := context.WithTimeout(ctx, smiResetTimeout)
	defer cancel()
	out, err := s.run(ctx, s.Path, "--gpu-reset", "-i", strconv.Itoa(index))
	if err != nil {
		return fmt.Errorf("gpu-reset %d failed: %w (output: %s)", index, err, firstLine(out))
	}
	return nil
}

// EnsureIdle implements GPUDriver: any compute or graphics process still
// holding the GPU fails the check, so a reset can never kill live work that
// survived the drain.
func (s *SMI) EnsureIdle(ctx context.Context, index int) error {
	ctx, cancel := context.WithTimeout(ctx, smiProbeTimeout)
	defer cancel()
	idx := strconv.Itoa(index)
	for _, query := range []string{"--query-compute-apps=pid", "--query-accounted-apps=pid"} {
		out, err := s.run(ctx, s.Path, query, "--format=csv,noheader", "-i", idx)
		if err != nil {
			// Accounted-apps requires accounting mode; only the compute-apps
			// probe is authoritative. Fail closed on its errors alone.
			if query == "--query-compute-apps=pid" {
				return fmt.Errorf("idle check on GPU %d failed: %w (output: %s)", index, err, firstLine(out))
			}
			continue
		}
		if pids := strings.TrimSpace(string(out)); pids != "" && !strings.EqualFold(pids, "[N/A]") {
			return fmt.Errorf("GPU %d is not idle: processes still attached (%s)", index, firstLine([]byte(pids)))
		}
	}
	return nil
}

// WatchEvents implements GPUDriver. The nvidia-smi driver has no event
// stream; kmsg is the detection source on this driver.
func (s *SMI) WatchEvents(ctx context.Context) (<-chan types.AgentEvent, error) {
	return nil, fmt.Errorf("nvidia-smi driver has no event stream; kmsg is the detection source")
}

// Healthy implements GPUDriver: a bounded trivial query. A wedged driver
// hangs nvidia-smi, which the deadline converts into an error.
func (s *SMI) Healthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, smiProbeTimeout)
	defer cancel()
	out, err := s.run(ctx, s.Path, "--query-gpu=count", "--format=csv,noheader")
	if err != nil {
		return fmt.Errorf("driver health probe failed: %w (output: %s)", err, firstLine(out))
	}
	return nil
}

// PartitionTopology reports a fail-closed summary of current NVIDIA MIG
// mode. Any enabled GPU makes the node partitioned; only an explicit Disabled
// result for every GPU is unpartitioned. [N/A], unknown output, a partial
// result, and a failed query are intentionally not interpreted as safe.
//
// The string result keeps the low-level driver independent of the NVIDIA
// accelerator adapter. That adapter maps "none", "mig", and "unknown" to
// its typed topology contract.
func (s *SMI) PartitionTopology(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, smiProbeTimeout)
	defer cancel()
	// Refresh first so the expected line count comes from the same bounded
	// preflight attempt, not a startup-era cache that could predate hotplug or
	// a driver reload.
	gpus, err := s.refresh(ctx)
	if err != nil {
		return "unknown", fmt.Errorf("MIG mode inventory refresh failed: %w", err)
	}
	out, err := s.run(ctx, s.Path, "--query-gpu=mig.mode.current", "--format=csv,noheader")
	if err != nil {
		return "unknown", fmt.Errorf("MIG mode query failed: %w (output: %s)", err, firstLine(out))
	}
	values := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(values) == 0 || (len(values) == 1 && strings.TrimSpace(values[0]) == "") {
		return "unknown", fmt.Errorf("MIG mode query returned no GPUs")
	}
	knownGPUCount := len(gpus)
	allDisabled := true
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "enabled":
			return "mig", nil
		case "disabled":
			// Continue: every device must explicitly report Disabled.
		case "[n/a]", "n/a":
			// The driver reports N/A for devices that cannot do MIG at all
			// (T4, V100, consumer parts). A device without partitioning
			// support cannot be partitioned, so this is positive evidence of
			// an unpartitioned device — not missing evidence. Note the
			// difference from a failed query above, which stays unknown.
		case "":
			return "unknown", fmt.Errorf("MIG mode query returned an empty GPU state")
		default:
			allDisabled = false
		}
	}
	if knownGPUCount > 0 && len(values) != knownGPUCount {
		return "unknown", fmt.Errorf("MIG mode query returned %d GPUs, want %d", len(values), knownGPUCount)
	}
	if allDisabled {
		return "none", nil
	}
	return "unknown", nil
}

// DriverVersion returns one verified driver version shared by every currently
// enumerated GPU. A mixed, missing, or partial result is not a runtime
// attestation: callers receive an error rather than an arbitrary device's
// version. As with MIG mode, the inventory is refreshed first so the expected
// count is from this bounded observation attempt.
func (s *SMI) DriverVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, smiProbeTimeout)
	defer cancel()
	gpus, err := s.refresh(ctx)
	if err != nil {
		return "", fmt.Errorf("driver version inventory refresh failed: %w", err)
	}
	out, err := s.run(ctx, s.Path, "--query-gpu=driver_version", "--format=csv,noheader")
	if err != nil {
		return "", fmt.Errorf("driver version query failed: %w (output: %s)", err, firstLine(out))
	}
	values := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(values) != len(gpus) {
		return "", fmt.Errorf("driver version query returned %d GPUs, want %d", len(values), len(gpus))
	}
	var version string
	for _, value := range values {
		current := strings.TrimSpace(value)
		if current == "" || strings.EqualFold(current, "[N/A]") {
			return "", fmt.Errorf("driver version query returned an empty GPU version")
		}
		if version == "" {
			version = current
			continue
		}
		if current != version {
			return "", fmt.Errorf("driver version query returned mixed versions %q and %q", version, current)
		}
	}
	return version, nil
}

// NormalizePCI reduces the PCI address spellings seen in kmsg XID lines and
// nvidia-smi output to one comparable "dddd:bb:dd" form: lowercase, function
// suffix dropped, domain trimmed/padded to four hex digits.
func NormalizePCI(addr string) string {
	a := strings.ToLower(strings.TrimSpace(addr))
	if i := strings.LastIndexByte(a, '.'); i >= 0 {
		a = a[:i] // drop the PCI function (".0")
	}
	parts := strings.Split(a, ":")
	if len(parts) == 2 {
		parts = append([]string{"0000"}, parts...) // no domain printed
	}
	if len(parts) != 3 {
		return a
	}
	domain := strings.TrimLeft(parts[0], "0")
	if len(domain) > 4 {
		return a
	}
	domain = strings.Repeat("0", 4-len(domain)) + domain
	return domain + ":" + parts[1] + ":" + parts[2]
}

func firstLine(out []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}
