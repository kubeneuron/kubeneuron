// Command kubeneuron-agent runs on every GPU node: it watches the kernel log
// and NVML for XID errors, pushes events to the controller, and executes
// controller-requested local actions (GPU reset, diagnostics, reboot).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/accelerator/nvidia"
	"github.com/kubeneuron/kubeneuron/internal/agent"
	"github.com/kubeneuron/kubeneuron/internal/agent/dcgm"
	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
)

var version = "dev"

func main() {
	var (
		controllerURL     = flag.String("controller", "https://localhost:8443", "controller agent-API base URL")
		nodeName          = flag.String("node", "", "node name (default: hostname)")
		token             = flag.String("token", "", "static bearer token for controller auth (development only)")
		tokenFile         = flag.String("token-file", "", "projected Pod-bound bearer token file")
		tlsCAFile         = flag.String("tls-ca", "", "CA bundle used to verify the controller")
		tlsCertFile       = flag.String("tls-cert", "", "agent fleet client certificate")
		tlsKeyFile        = flag.String("tls-key", "", "agent fleet client private key")
		allowInsecureHTTP = flag.Bool("allow-insecure-http", false, "allow plaintext controller HTTP for local development only")
		spoolPath         = flag.String("spool", "/var/lib/kube-neuron/spool.jsonl", "event spool file")
		scriptsDir        = flag.String("scripts-dir", "/etc/kube-neuron/scripts", "directory of operator-provisioned remediation scripts")
		rebootCommand     = flag.String(
			"reboot-command",
			"",
			"space-separated command that reboots the host; empty uses the default, which enters PID 1's namespaces and asks systemd. Never taken from a playbook.",
		)
		healthListen         = flag.String("health-listen", ":9402", "health probe listen address")
		registrationInterval = flag.Duration(
			"registration-interval",
			30*time.Second,
			"durable controller registration interval",
		)
		registrationStaleAfter = flag.Duration(
			"registration-stale-after",
			90*time.Second,
			"time without a durable controller registration acknowledgment before readiness fails",
		)
		fakeGPU           = flag.Bool("fake-gpu", false, "use the fake GPU driver (dev/simulator)")
		requireRealDriver = flag.Bool("require-real-driver", false, "refuse to start when nvidia-smi is unavailable instead of falling back to the fake driver; set on every managed GPU node")
		nvidiaSMIPath     = flag.String("nvidia-smi", "", "nvidia-smi binary path (default: nvidia-smi from PATH)")
		enableDestructive = flag.Bool("enable-destructive-actions", false, "permit reset/reboot/driver/script actions on this node; requires a real driver and remains subject to every controller gate")
		nvidiaObservation = flag.Bool(
			"nvidia-observation",
			false,
			"emit observation-only NVIDIA accelerator reports when a real nvidia-smi runtime is present; never enables remediation",
		)
		nvidiaDriverVersion = flag.String(
			"nvidia-driver-version",
			"",
			"deprecated NVIDIA driver profile metadata; a real nvidia-smi preflight attests the driver version instead",
		)
		nvidiaRuntimeVersion = flag.String(
			"nvidia-runtime-version",
			"",
			"expected DCGM runtime version in dcgm-X.Y form for static profiles; cannot make a report ready without a matching local probe",
		)
		nvidiaDCGMPath = flag.String(
			"nvidia-dcgmi",
			dcgm.BundledClientPath,
			"dcgmi binary used for bounded local DCGM runtime attestation. Defaults to the client shipped in the agent image so the attested version is the same on every node; point it at a host client only if you intend that client's version to be what the runtime profile pins.",
		)
		nvidiaDCGMEndpoint = flag.String(
			"nvidia-dcgm-endpoint",
			"",
			"DCGM host engine address for runtime attestation, e.g. 10.0.1.7:5555; empty uses dcgmi's local default. It must be this node's own engine.",
		)
		nvidiaPartitionTopology = flag.String(
			"nvidia-partition-topology",
			"unknown",
			"fallback NVIDIA partition topology for drivers without a current probe: unknown, none, mig, or other; real nvidia-smi MIG mode overrides it",
		)
		nvidiaProfileDigest = flag.String(
			"nvidia-profile-digest",
			"",
			"optional immutable digest of the NVIDIA runtime profile that supplied the observation metadata",
		)
		nvidiaControllerProfile = flag.Bool(
			"nvidia-controller-profile",
			false,
			"obtain the NVIDIA observation profile digest from the authenticated controller; mutually exclusive with --nvidia-profile-digest",
		)
		amdDetection = flag.Bool(
			"amd-detection",
			false,
			"poll amd-smi/rocm-smi as an observation-only AMD detection source; it stays disabled unless one of those binaries is actually present, and it never enables remediation",
		)
		amdSMIPath  = flag.String("amd-smi", "amd-smi", "amd-smi binary used by the AMD detection source (preferred over rocm-smi)")
		rocmSMIPath = flag.String(
			"rocm-smi",
			"rocm-smi",
			"rocm-smi binary used when amd-smi is absent or fails; it reports retired pages and temperature only",
		)
		amdThermalCriticalC = flag.Float64(
			"amd-thermal-critical-c",
			0,
			"hotspot temperature at which an AMD reading with no explicit throttle flag becomes a thermal fault; 0 (default) observes temperatures without ever promoting one, because the critical value is SKU-specific",
		)
		amdCorrectableRateMinDelta = flag.Uint64(
			"amd-correctable-ecc-min-delta",
			0,
			"how many new corrected AMD ECC errors must accumulate before the rate fault reports again; 0 uses the built-in conservative default",
		)
		amdXGMILinkMinDelta = flag.Uint64(
			"amd-xgmi-link-min-delta",
			0,
			"how many new AMD XGMI link errors must accumulate before the fabric fault reports again; 0 uses the built-in default. The counter also moves on corrected link retries, so reporting every increment pages on a healthy fabric",
		)
		amdBadPageThreshold = flag.Uint64(
			"amd-bad-page-threshold",
			0,
			"retired-page count at or above which an AMD GPU is reported as OUT OF SPARE MEMORY (a replace-the-device condition) rather than as having retired a page successfully; 0 (default) never makes that claim, because the bad-page budget is SKU-specific",
		)
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("kubeneuron-agent", version)
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	var driver nvml.GPUDriver
	switch {
	case *fakeGPU:
		if *requireRealDriver {
			log.Error("-fake-gpu and -require-real-driver are mutually exclusive")
			os.Exit(1)
		}
		driver = &nvml.Fake{}
	case nvml.DetectSMI(*nvidiaSMIPath):
		log.Info("using nvidia-smi GPU driver")
		driver = nvml.NewSMI(*nvidiaSMIPath)
	case *requireRealDriver:
		// A managed GPU node must never run on the fake driver: it reports
		// success for ResetGPU and would turn missing tooling into a silent
		// lie. Crash-looping here is the honest failure mode.
		log.Error("nvidia-smi not found and -require-real-driver is set; refusing to start with the fake driver")
		os.Exit(1)
	default:
		log.Warn("nvidia-smi not found; using fake GPU driver (no real GPU telemetry or actions)")
		driver = &nvml.Fake{}
	}
	if *enableDestructive {
		if _, realSMI := driver.(*nvml.SMI); !realSMI {
			// Destructive actions against the fake driver would report
			// success without touching hardware — worse than failing.
			log.Error("-enable-destructive-actions requires the real nvidia-smi driver")
			os.Exit(1)
		}
		log.Warn("DESTRUCTIVE ACTIONS ENABLED on this node: reset/reboot/driver/script actions will execute when every controller gate admits them")
	}
	if *nvidiaObservation {
		if _, realSMI := driver.(*nvml.SMI); !realSMI {
			log.Warn("NVIDIA observation requested but disabled: no real nvidia-smi runtime evidence")
		} else {
			log.Info("NVIDIA observation reporting enabled", "partition_topology", *nvidiaPartitionTopology)
		}
	}

	a, err := agent.New(agent.Config{
		NodeName:                 *nodeName,
		ControllerURL:            *controllerURL,
		Token:                    *token,
		TokenFile:                *tokenFile,
		TLSCAFile:                *tlsCAFile,
		TLSCertFile:              *tlsCertFile,
		TLSKeyFile:               *tlsKeyFile,
		AllowInsecureHTTP:        *allowInsecureHTTP,
		SpoolPath:                *spoolPath,
		ScriptsDir:               *scriptsDir,
		RebootCommand:            strings.Fields(*rebootCommand),
		EnableDestructiveActions: *enableDestructive,
		HealthListenAddress:      *healthListen,
		RegistrationInterval:     *registrationInterval,
		RegistrationStaleAfter:   *registrationStaleAfter,
		NVIDIAObservation: agent.NVIDIAObservationConfig{
			Enabled:              *nvidiaObservation,
			DriverVersion:        *nvidiaDriverVersion,
			RuntimeVersion:       *nvidiaRuntimeVersion,
			DCGMPath:             *nvidiaDCGMPath,
			DCGMEndpoint:         *nvidiaDCGMEndpoint,
			PartitionTopology:    nvidiaPartitionTopologyValue(*nvidiaPartitionTopology),
			ProfileDigest:        *nvidiaProfileDigest,
			UseControllerProfile: *nvidiaControllerProfile,
		},
		AMDDetection: agent.AMDDetectionConfig{
			Enabled:                 *amdDetection,
			AMDSMIPath:              *amdSMIPath,
			ROCmSMIPath:             *rocmSMIPath,
			ThermalCriticalC:        *amdThermalCriticalC,
			CorrectableRateMinDelta: *amdCorrectableRateMinDelta,
			XGMILinkMinDelta:        *amdXGMILinkMinDelta,
			BadPageThreshold:        *amdBadPageThreshold,
		},
	}, driver, log)
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("kubeneuron-agent starting", "version", version, "controller", *controllerURL)
	if err := a.Run(ctx); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func nvidiaPartitionTopologyValue(value string) nvidia.PartitionTopology {
	return nvidia.PartitionTopology(value)
}
