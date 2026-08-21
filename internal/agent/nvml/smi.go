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

	// SysfsRoot is the sysfs read for PCI reset capability, default /sys. The
	// agent container shares the host's sysfs; tests point it at a fixture.
	SysfsRoot string

	// ProcRoot is the procfs scanned for processes holding a GPU device node,
	// default /proc. The agent runs with hostPID, so the default is the host's
	// process table; tests point it at a fixture tree.
	ProcRoot string

	// run executes a command and returns combined output; tests inject it.
	run runner

	// queryTimeout bounds a single inventory refresh. It defaults to
	// smiQueryTimeout; tests shorten it to exercise the deadline. It exists so
	// refresh imposes its OWN deadline even when a caller passes an unbounded
	// context (the XID hot path does), instead of hanging the agent's main loop
	// on a wedged driver.
	queryTimeout time.Duration

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
	return &SMI{Path: path, run: execRunner, queryTimeout: smiQueryTimeout}
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

// refreshTimeout is the deadline refresh imposes on itself, defaulting to
// smiQueryTimeout for any SMI not built through NewSMI.
func (s *SMI) refreshTimeout() time.Duration {
	if s.queryTimeout > 0 {
		return s.queryTimeout
	}
	return smiQueryTimeout
}

// refresh queries the inventory and caches it under the lock.
//
// It derives its OWN bounded context instead of trusting the caller's. The XID
// hot path (handleXID -> GPUByPCIAddr -> refresh) runs on the agent's main Run
// loop with the process-lifetime context and NO deadline; a driver that wedges
// nvidia-smi at fault time (documented for XID 62/79 when a GPU falls off the
// bus) would otherwise block that loop forever, stalling heartbeats, spool
// flush and watcher reopen while /livez stays green so kubelet never restarts
// the agent. The deadline turns that hang into a prompt error the caller's
// existing failure handling can act on, so the XID still gets spooled.
func (s *SMI) refresh(ctx context.Context) ([]smiGPU, error) {
	ctx, cancel := context.WithTimeout(ctx, s.refreshTimeout())
	defer cancel()
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
		uuid := strings.TrimSpace(fields[1])
		// The UUID is validated as strictly as the index, and for a worse
		// reason: it is an IDENTITY, and nvidia-smi emits placeholders.
		//
		// A driver that is up but unhappy prints "[N/A]" or "ERR!" in these
		// columns rather than failing, and every one of them was taken
		// verbatim. Two GPUs then share the UUID "[N/A]" — which becomes their
		// registration entry, their accelerator-report device, and the target
		// key of any incident opened for either. Two physical devices with one
		// incident identity is the failure this whole per-device story exists
		// to avoid.
		//
		// Fail the refresh closed, as an unparseable index already does: an
		// inventory this build cannot trust is not an inventory.
		if !strings.HasPrefix(uuid, "GPU-") && !strings.HasPrefix(uuid, "MIG-") {
			return nil, fmt.Errorf("nvidia-smi reported %q as a GPU UUID in %q; a device identity must be GPU-… or MIG-…", uuid, line)
		}
		gpus = append(gpus, smiGPU{
			info: types.GPUInfo{
				Index: index,
				UUID:  uuid,
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
//
// Prefer ResetGPUByUUID: an integer index is not a stable device identity, and
// an off-the-bus renumber between the caller's resolve and this reset can land
// it on a healthy drained neighbor. ResetGPU stays for drivers (the fake, any
// future one) that have no UUID to offer.
func (s *SMI) ResetGPU(ctx context.Context, index int) error {
	ctx, cancel := context.WithTimeout(ctx, smiResetTimeout)
	defer cancel()
	out, err := s.run(ctx, s.Path, "--gpu-reset", "-i", strconv.Itoa(index))
	if err != nil {
		return fmt.Errorf("gpu-reset %d failed: %w (output: %s)", index, err, firstLine(out))
	}
	return nil
}

// ResetGPUByUUID resets the device identified by its stable UUID. nvidia-smi
// accepts a GPU UUID wherever it accepts an index for -i, so targeting the UUID
// closes the resolve->reset renumber window that an integer index leaves open:
// even if enumeration indices shift the instant before the reset, the UUID
// still names the same physical device. HARDWARE-DEPENDENT: that nvidia-smi
// --gpu-reset accepts a UUID for -i needs confirmation on a real GPU.
func (s *SMI) ResetGPUByUUID(ctx context.Context, uuid string) error {
	ctx, cancel := context.WithTimeout(ctx, smiResetTimeout)
	defer cancel()
	out, err := s.run(ctx, s.Path, "--gpu-reset", "-i", uuid)
	if err != nil {
		return fmt.Errorf("gpu-reset %s failed: %w (output: %s)", uuid, err, firstLine(out))
	}
	return nil
}

// EnsureIdle implements GPUDriver: any compute or graphics process still
// holding the GPU fails the check, so a reset can never kill live work that
// survived the drain.
func (s *SMI) EnsureIdle(ctx context.Context, index int) error {
	return s.ensureIdleSelector(ctx, strconv.Itoa(index), fmt.Sprintf("GPU %d", index))
}

// EnsureIdleByUUID runs the idle check addressing the device by its stable UUID.
// nvidia-smi accepts a GPU UUID wherever it accepts an index for -i, so a reset's
// preflight can target the same physical device as ResetGPUByUUID even if
// enumeration indices shift between the resolve and this check. HARDWARE-DEPENDENT:
// that nvidia-smi accepts a UUID for -i on these query flags needs confirmation
// on a real GPU.
func (s *SMI) EnsureIdleByUUID(ctx context.Context, uuid string) error {
	return s.ensureIdleSelector(ctx, uuid, "GPU "+uuid)
}

// ensureIdleSelector is EnsureIdle parameterized by the -i selector (an index or
// a UUID) and a human label for errors.
func (s *SMI) ensureIdleSelector(ctx context.Context, selector, label string) error {
	ctx, cancel := context.WithTimeout(ctx, smiProbeTimeout)
	defer cancel()
	// compute-apps only. --query-accounted-apps was here and had to go.
	//
	// NVML documents nvmlDeviceGetAccountingPids — which is what that flag
	// reads — as returning processes "in running OR TERMINATED state", retained
	// in a circular buffer until `nvidia-smi -caa` clears it. So on any node
	// with accounting mode enabled (some DCGM deployments turn it on for
	// DCGM_FI_DEV_ACCOUNTING_DATA), once ANY job has ever run on the GPU this
	// probe reports PIDs forever and the device is never idle again.
	//
	// The consequence is not a slow retry. The executor stamps the refusal as
	// not_idle, the controller declines to escalate — correctly, because
	// escalating past an idle guard trades the workload for the device — and
	// quarantines to NEEDS_HUMAN having recorded that live work was spared.
	// A broken probe reported as value delivered is exactly the conflation
	// ErrNotIdle exists to prevent, arriving through a door that comment did
	// not anticipate.
	//
	// Nothing is lost by dropping it: compute-apps reports current attachment,
	// and the /proc holder scan is strictly stronger than either, seeing every
	// process holding the device node rather than only CUDA contexts.
	out, err := s.run(ctx, s.Path, "--query-compute-apps=pid", "--format=csv,noheader", "-i", selector)
	if err != nil {
		return fmt.Errorf("idle check on %s failed: %w (output: %s)", label, err, firstLine(out))
	}
	if pids := strings.TrimSpace(string(out)); pids != "" && !strings.EqualFold(pids, "[N/A]") {
		return fmt.Errorf("%s: processes still attached (%s): %w", label, firstLine([]byte(pids)), ErrNotIdle)
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

// SetPersistenceMode turns persistence mode on or off for ONE GPU.
//
// It is a prerequisite for a reset, not an optimization: with persistence mode
// on, the driver keeps the device initialized and nvidia-smi --gpu-reset fails
// with "currently in use by another process" even on a node with no workload
// at all. Measured on a live T4 — the persistence daemon was holding
// /dev/nvidia0 after every Kubernetes-visible consumer had gone.
//
// The set is scoped to the target with -i. Without it, `nvidia-smi -pm` flips
// persistence for EVERY GPU on the node, so a per-GPU quiesce would disturb
// healthy siblings and restore could not put the exact snapshot back on a
// multi-GPU node. HARDWARE-DEPENDENT: the exact per-GPU -pm scoping needs
// confirmation on a real multi-GPU node.
func (s *SMI) SetPersistenceMode(ctx context.Context, index int, enabled bool) error {
	return s.setPersistenceModeSelector(ctx, strconv.Itoa(index), enabled)
}

// SetPersistenceModeByUUID toggles persistence mode addressing the device by its
// stable UUID, so a quiesce and its later restore stay pinned to one physical GPU
// even if enumeration indices shift in between. nvidia-smi accepts a UUID for -i.
// HARDWARE-DEPENDENT: the exact per-GPU -pm scoping by UUID needs confirmation on
// a real multi-GPU node.
func (s *SMI) SetPersistenceModeByUUID(ctx context.Context, uuid string, enabled bool) error {
	return s.setPersistenceModeSelector(ctx, uuid, enabled)
}

// setPersistenceModeSelector is SetPersistenceMode parameterized by the -i
// selector (an index or a UUID).
func (s *SMI) setPersistenceModeSelector(ctx context.Context, selector string, enabled bool) error {
	ctx, cancel := context.WithTimeout(ctx, smiProbeTimeout)
	defer cancel()
	value := "0"
	if enabled {
		value = "1"
	}
	out, err := s.run(ctx, s.Path, "-pm", value, "-i", selector)
	if err != nil {
		return fmt.Errorf("nvidia-smi -pm %s -i %s: %w (output: %s)", value, selector, err, firstLine(out))
	}
	return nil
}

// PersistenceMode returns whether persistence mode is enabled for one GPU.
// Quiesce snapshots this before changing it so restore does not enable a mode
// the host administrator had deliberately left disabled.
func (s *SMI) PersistenceMode(ctx context.Context, index int) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, smiProbeTimeout)
	defer cancel()
	out, err := s.run(ctx, s.Path, "--query-gpu=persistence_mode", "--format=csv,noheader,nounits", "-i", strconv.Itoa(index))
	if err != nil {
		return false, fmt.Errorf("query persistence mode: %w (output: %s)", err, firstLine(out))
	}
	switch strings.ToLower(strings.TrimSpace(string(out))) {
	case "enabled":
		return true, nil
	case "disabled":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected persistence mode %q", firstLine(out))
	}
}
