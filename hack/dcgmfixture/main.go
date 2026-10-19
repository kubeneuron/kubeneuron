// Command dcgmfixture serves a synthetic dcgm-exporter /metrics endpoint for
// the kind GPU observability fixture (hack/kind-gpu-observability.sh).
//
// It is CPU-only and talks to no NVML, DCGM or GPU: every value is a constant
// or a counter that grows linearly with wall-clock time since start. What it
// reproduces is the exposition *shape* of nvidia dcgm-exporter: the
// DCGM_FI_* metric names the recording rules read, the exporter's own labels
// (gpu, UUID, pci_bus_id, device, modelName, Hostname, DCGM_FI_DRIVER_VERSION)
// and the Kubernetes mapping labels (pod, namespace, container). That is
// enough to prove the vmagent label contract and the recording/derived rules
// in configs/vmalert/gpu-recording-rules.yaml wire up, and nothing more.
//
// Deliberately omitted so the harness can prove that an unreported source
// field yields an ABSENT canonical/derived series, never a zero:
//
//   - 153  DCGM_FI_DEV_GPU_TEMP_LIMIT (thermal margin)
//   - 393  DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS
//   - 394  DCGM_FI_DEV_CORRECTABLE_REMAPPED_ROWS
//   - 395  DCGM_FI_DEV_ROW_REMAP_FAILURE
//   - 396  DCGM_FI_DEV_ROW_REMAP_PENDING
//   - 409+ every DCGM_FI_DEV_NVLINK_* counter
//
// The device set is given by -spec, a ';'-separated list of
// "<modelName>:<count>:<slow>[:<unrepairable>]" entries, where <slow> is the
// index (within that model's devices) of the one device that runs with a
// reduced SM clock and a growing power-violation counter, or "-" for none,
// and the optional <unrepairable> is the index of the one device that
// reports DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG (1507) as 1, or "-" for none. The
// unrepairable device is the input for the shadow-routing row of the
// harness: it makes the shipped ShadowGpuMemoryBeyondRepair rule
// (configs/vmalert/gpu-shadow-rules.yaml) fire after its 5m hold without
// changing the rule. Devices are numbered gpu=0.. across all entries in
// order.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Device is one synthetic GPU.
type Device struct {
	Index int
	Model string
	Slow  bool
	// Unrepairable makes the device report DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG
	// = 1; every other value stays what it would be for a healthy device.
	Unrepairable bool
	// Workload mapping labels; all three are empty for an idle device, which
	// is what dcgm-exporter emits while no pod holds the GPU.
	Pod, Namespace, Container string
}

// UUID returns the exporter-style identity for the device: deterministic,
// derived from model and index, never colliding across models.
func (d Device) UUID() string {
	model := strings.ToLower(strings.ReplaceAll(d.Model, " ", "-"))
	return fmt.Sprintf("GPU-fixture-%s-%08x", model, d.Index)
}

// PCIBusID mimics the 0000:00:1E.0 shape dcgm-exporter reports.
func (d Device) PCIBusID() string {
	return fmt.Sprintf("00000000:%02X:00.0", 0x1E+d.Index)
}

const (
	// Clock values in MHz. A slow device runs SM at slowSMClock against the
	// same maximum, so its clock ratio is 1000/1590 = 0.6289 and the peer
	// degradation score 1 - 0.6289/1 = 0.3711 (>= the rule's 0.15 floor). A
	// two-device peer group would score 1 - 2*0.6289/1.6289 = 0.2278, also
	// above the floor, so for the A10G pair only the three-peer floor blocks.
	maxSMClock  = 1590
	slowSMClock = 1000
	memClock    = 5000
	maxMemClock = 5001

	// Clock-event bitmask (field 112). Healthy devices report the idle bit
	// only (1), which decodes to 0 for every reason the rules name; the slow
	// device reports hw_slowdown (8) | sw_thermal (32).
	throttleReasonsIdle = 1
	throttleReasonsSlow = 8 | 32

	// Framebuffer in MiB (field 250-252). fbUsedBase for gpu 0; each later
	// device adds its index so values stay distinct.
	fbTotalMiB  = 15360
	fbUsedBase  = 12345
	powerLimitW = 70.0

	// Per-second growth of the slow device's power-violation counter, in
	// nanoseconds: 0.12 s of violation per second -> throttle share 0.12,
	// above the fail-slow rule's 0.05 floor. Healthy devices stay at 0.
	slowViolationNsPerSecond = 120_000_000

	driverVersion = "580.126.20"
)

// ParseSpec parses the -spec grammar; see the package comment.
func ParseSpec(spec string) ([]Device, error) {
	var devices []Device
	entries := strings.Split(spec, ";")
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 3 && len(parts) != 4 {
			return nil, fmt.Errorf("spec entry %q: want <model>:<count>:<slow|->[:<unrepairable|->]", entry)
		}
		model := strings.TrimSpace(parts[0])
		if model == "" {
			return nil, fmt.Errorf("spec entry %q: empty model", entry)
		}
		count, err := strconv.Atoi(parts[1])
		if err != nil || count < 1 {
			return nil, fmt.Errorf("spec entry %q: count must be a positive integer", entry)
		}
		slow := -1
		if parts[2] != "-" {
			slow, err = strconv.Atoi(parts[2])
			if err != nil || slow < 0 || slow >= count {
				return nil, fmt.Errorf("spec entry %q: slow index must be - or in [0,%d)", entry, count)
			}
		}
		unrepairable := -1
		if len(parts) == 4 && parts[3] != "-" {
			unrepairable, err = strconv.Atoi(parts[3])
			if err != nil || unrepairable < 0 || unrepairable >= count {
				return nil, fmt.Errorf("spec entry %q: unrepairable index must be - or in [0,%d)", entry, count)
			}
		}
		for i := 0; i < count; i++ {
			index := len(devices)
			d := Device{Index: index, Model: model, Slow: i == slow, Unrepairable: i == unrepairable}
			// Every device but the last of each model is held by a workload;
			// the last one is idle so both label states are exercised.
			if i != count-1 {
				d.Pod = fmt.Sprintf("train-%d", index)
				d.Namespace = "ml-fixture"
				d.Container = "trainer"
			}
			devices = append(devices, d)
		}
	}
	if len(devices) == 0 {
		return nil, errors.New("spec names no devices")
	}
	return devices, nil
}

type sample struct {
	name  string
	help  string
	kind  string // gauge or counter
	value func(d Device, elapsed time.Duration) float64
}

func constant(v float64) func(Device, time.Duration) float64 {
	return func(Device, time.Duration) float64 { return v }
}

// samples lists every emitted field in exposition order. Field numbers are
// the dcgm-exporter ones; compare with the catalog and the recording rules.
var samples = []sample{
	{"DCGM_FI_DEV_SM_CLOCK", "SM clock frequency (in MHz).", "gauge", func(d Device, _ time.Duration) float64 {
		if d.Slow {
			return slowSMClock
		}
		return maxSMClock
	}},
	{"DCGM_FI_DEV_MEM_CLOCK", "Memory clock frequency (in MHz).", "gauge", constant(memClock)},
	{"DCGM_FI_DEV_MAX_SM_CLOCK", "Maximum supported SM clock (in MHz).", "gauge", constant(maxSMClock)},
	{"DCGM_FI_DEV_MAX_MEM_CLOCK", "Maximum supported memory clock (in MHz).", "gauge", constant(maxMemClock)},
	{"DCGM_FI_DEV_CLOCK_THROTTLE_REASONS", "Current clock throttle reasons (bitmask of DCGM_CLOCKS_THROTTLE_REASON_*).", "gauge", func(d Device, _ time.Duration) float64 {
		if d.Slow {
			return throttleReasonsSlow
		}
		return throttleReasonsIdle
	}},
	{"DCGM_FI_DEV_MEMORY_TEMP", "Memory temperature (in C).", "gauge", func(d Device, _ time.Duration) float64 { return 50 + float64(d.Index) }},
	{"DCGM_FI_DEV_GPU_TEMP", "GPU temperature (in C).", "gauge", func(d Device, _ time.Duration) float64 { return 60 + float64(d.Index) }},
	{"DCGM_FI_DEV_MEM_MAX_OP_TEMP", "Maximum operating temperature for the memory (in C).", "gauge", constant(95)},
	// 153 DCGM_FI_DEV_GPU_TEMP_LIMIT deliberately omitted.
	{"DCGM_FI_DEV_SLOWDOWN_TEMP", "Slowdown temperature for the device (in C).", "gauge", constant(85)},
	{"DCGM_FI_DEV_SHUTDOWN_TEMP", "Shutdown temperature for the device (in C).", "gauge", constant(90)},
	{"DCGM_FI_DEV_POWER_USAGE", "Power draw (in W).", "gauge", func(d Device, _ time.Duration) float64 { return 42.5 + 0.5*float64(d.Index) }},
	{"DCGM_FI_DEV_ENFORCED_POWER_LIMIT", "Effective power limit that the driver enforces after taking into account all limiters.", "gauge", constant(powerLimitW)},
	{"DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION", "Total energy consumption since boot (in mJ).", "counter", func(d Device, elapsed time.Duration) float64 {
		watts := 42.5 + 0.5*float64(d.Index)
		return 1_000_000 + watts*1000*elapsed.Seconds()
	}},
	{"DCGM_FI_DEV_POWER_VIOLATION", "Throttling duration due to power constraints (in us).", "counter", func(d Device, elapsed time.Duration) float64 {
		if d.Slow {
			return slowViolationNsPerSecond * elapsed.Seconds()
		}
		return 0
	}},
	{"DCGM_FI_DEV_THERMAL_VIOLATION", "Throttling duration due to thermal constraints (in us).", "counter", constant(0)},
	{"DCGM_FI_DEV_SYNC_BOOST_VIOLATION", "Throttling duration due to sync-boost constraints (in us).", "counter", constant(0)},
	{"DCGM_FI_DEV_BOARD_LIMIT_VIOLATION", "Throttling duration due to board limit constraints (in us).", "counter", constant(0)},
	{"DCGM_FI_DEV_RELIABILITY_VIOLATION", "Throttling duration due to reliability constraints (in us).", "counter", constant(0)},
	{"DCGM_FI_DEV_GPU_UTIL", "GPU utilization (in %).", "gauge", constant(87)},
	{"DCGM_FI_DEV_MEM_COPY_UTIL", "Memory utilization (in %).", "gauge", constant(40)},
	{"DCGM_FI_DEV_FB_TOTAL", "Framebuffer memory total (in MiB).", "gauge", constant(fbTotalMiB)},
	{"DCGM_FI_DEV_FB_FREE", "Framebuffer memory free (in MiB).", "gauge", func(d Device, _ time.Duration) float64 { return fbTotalMiB - (fbUsedBase + float64(d.Index)) }},
	{"DCGM_FI_DEV_FB_USED", "Framebuffer memory used (in MiB).", "gauge", func(d Device, _ time.Duration) float64 { return fbUsedBase + float64(d.Index) }},
	{"DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", "Total number of single-bit volatile ECC errors.", "counter", constant(0)},
	{"DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", "Total number of double-bit volatile ECC errors.", "counter", constant(0)},
	{"DCGM_FI_DEV_ECC_SBE_AGG_TOTAL", "Total number of single-bit persistent ECC errors.", "counter", constant(0)},
	{"DCGM_FI_DEV_ECC_DBE_AGG_TOTAL", "Total number of double-bit persistent ECC errors.", "counter", constant(0)},
	{"DCGM_FI_DEV_RETIRED_SBE", "Total number of retired pages due to single-bit errors.", "counter", constant(0)},
	{"DCGM_FI_DEV_RETIRED_DBE", "Total number of retired pages due to double-bit errors.", "counter", constant(0)},
	{"DCGM_FI_DEV_RETIRED_PENDING", "Total number of pages pending retirement.", "gauge", constant(0)},
	// 393-396 remapped rows / row remap state deliberately omitted.
	// 1507: 1 on the one device the spec marks unrepairable, a present 0
	// everywhere else (so the harness can tell a present zero from absence).
	{"DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG", "Whether the device has unrepairable memory.", "gauge", func(d Device, _ time.Duration) float64 {
		if d.Unrepairable {
			return 1
		}
		return 0
	}},
	{"DCGM_FI_DEV_PCIE_MAX_LINK_GEN", "Maximum PCIe link generation.", "gauge", constant(3)},
	{"DCGM_FI_DEV_PCIE_MAX_LINK_WIDTH", "Maximum PCIe link width.", "gauge", constant(16)},
	{"DCGM_FI_DEV_PCIE_LINK_GEN", "Current PCIe link generation.", "gauge", constant(3)},
	{"DCGM_FI_DEV_PCIE_LINK_WIDTH", "Current PCIe link width.", "gauge", constant(16)},
	{"DCGM_FI_DEV_PCIE_REPLAY_COUNTER", "Total number of PCIe retries.", "counter", constant(0)},
	{"DCGM_FI_PROF_PCIE_TX_BYTES", "The number of bytes of active PCIe tx (transmit) data including both header and payload.", "gauge", constant(1_200_000)},
	{"DCGM_FI_PROF_PCIE_RX_BYTES", "The number of bytes of active PCIe rx (read) data including both header and payload.", "gauge", constant(900_000)},
	// 409+ NVLink counters deliberately omitted.
}

// EmittedFields lists the DCGM_FI_* metric names this fixture serves.
func EmittedFields() []string {
	names := make([]string, 0, len(samples))
	for _, s := range samples {
		names = append(names, s.name)
	}
	sort.Strings(names)
	return names
}

// OmittedFields lists the source fields the fixture leaves out on purpose;
// the recording rules read these, so their canonical series must be absent.
var OmittedFields = []string{
	"DCGM_FI_DEV_GPU_TEMP_LIMIT",
	"DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS",
	"DCGM_FI_DEV_CORRECTABLE_REMAPPED_ROWS",
	"DCGM_FI_DEV_ROW_REMAP_FAILURE",
	"DCGM_FI_DEV_ROW_REMAP_PENDING",
	"DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL",
	"DCGM_FI_DEV_NVLINK_CRC_DATA_ERROR_COUNT_TOTAL",
	"DCGM_FI_DEV_NVLINK_REPLAY_ERROR_COUNT_TOTAL",
	"DCGM_FI_DEV_NVLINK_RECOVERY_ERROR_COUNT_TOTAL",
}

func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return strings.ReplaceAll(v, "\n", `\n`)
}

// Render writes the Prometheus text exposition for devices on hostname at
// the given elapsed run time.
func Render(w io.Writer, hostname string, devices []Device, elapsed time.Duration) error {
	for _, s := range samples {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", s.name, s.help, s.name, s.kind); err != nil {
			return err
		}
		for _, d := range devices {
			_, err := fmt.Fprintf(w,
				`%s{gpu="%d",UUID="%s",pci_bus_id="%s",device="nvidia%d",modelName="%s",Hostname="%s",DCGM_FI_DRIVER_VERSION="%s",container="%s",namespace="%s",pod="%s"} %s`+"\n",
				s.name, d.Index, d.UUID(), d.PCIBusID(), d.Index, escapeLabel(d.Model), escapeLabel(hostname), driverVersion,
				escapeLabel(d.Container), escapeLabel(d.Namespace), escapeLabel(d.Pod),
				strconv.FormatFloat(s.value(d, elapsed), 'f', -1, 64))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func main() {
	listen := flag.String("listen", ":9400", "address to serve /metrics on")
	spec := flag.String("spec", "", "device spec: '<model>:<count>:<slow|->;...'")
	hostname := flag.String("hostname", "", "value of the Hostname label (default: NODE_NAME env, then os.Hostname)")
	flag.Parse()

	devices, err := ParseSpec(*spec)
	if err != nil {
		log.Fatalf("dcgmfixture: %v", err)
	}
	host := *hostname
	if host == "" {
		host = os.Getenv("NODE_NAME")
	}
	if host == "" {
		if host, err = os.Hostname(); err != nil {
			log.Fatalf("dcgmfixture: hostname: %v", err)
		}
	}
	start := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := Render(w, host, devices, time.Since(start)); err != nil {
			log.Printf("dcgmfixture: render: %v", err)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("dcgmfixture: serving %d synthetic devices on %s (CPU-only, no DCGM)", len(devices), *listen)
	log.Fatal(server.ListenAndServe())
}
