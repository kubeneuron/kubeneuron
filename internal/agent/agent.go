// Package agent wires the kubeneuron-agent: the kmsg kernel-fault watcher
// (NVRM Xid and amdgpu families), the NVML driver, the polled second sources
// (DCGM/nvidia-smi and amd-smi/rocm-smi), the local action executor, and the
// event push loop toward the controller (with a file-backed spool for
// outages).
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/accelerator"
	"github.com/kubeneuron/kubeneuron/internal/accelerator/nvidia"
	"github.com/kubeneuron/kubeneuron/internal/agent/actionjournal"
	"github.com/kubeneuron/kubeneuron/internal/agent/amdhealth"
	"github.com/kubeneuron/kubeneuron/internal/agent/dcgm"
	"github.com/kubeneuron/kubeneuron/internal/agent/executor"
	"github.com/kubeneuron/kubeneuron/internal/agent/gpuhealth"
	"github.com/kubeneuron/kubeneuron/internal/agent/kmsg"
	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/internal/agent/spool"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	defaultHealthListenAddress    = ":9402"
	defaultRegistrationInterval   = 30 * time.Second
	defaultRegistrationStaleAfter = 90 * time.Second
	healthShutdownTimeout         = 5 * time.Second
	registrationCapabilityLimit   = 128
	spoolReplayBatchSize          = 64
	// spoolFlushBudget bounds how long one heartbeat tick may spend draining
	// the spool, so a large backlog is drained in minutes without ever
	// starving the registration loop.
	spoolFlushBudget  = 15 * time.Second
	maxTokenFileBytes = 1 << 20
	// nvidiaReportTimeout bounds one observation-only preflight. A wedged
	// driver must leave a failed report to retry later, not hold the agent's
	// registration/event loop indefinitely.
	nvidiaReportTimeout = 15 * time.Second
	// acceleratorProfileResponseLimit bounds a controller response before it
	// is decoded into the narrow profile-binding wire type.
	acceleratorProfileResponseLimit = 64 << 10
	// actionPollInterval is how often the agent asks the controller for
	// queued work. Polling rides the authenticated channel, so the node
	// needs no listener or serving certificate of its own.
	actionPollInterval       = 5 * time.Second
	maxActionBytes           = 1 << 20
	defaultActionJournalPath = "/var/lib/kube-neuron/actions.jsonl"
	// detectionDedupWindow is how long one underlying fault (same GPU + XID)
	// is remembered so a second detection source cannot re-report it. It is
	// short: a genuinely recurrent fault after this window is a new signal the
	// controller's escalation ladder should still see.
	detectionDedupWindow = 2 * time.Minute
)

// NVIDIAObservationConfig explicitly opts a real NVIDIA runtime into the
// versioned, observation-only accelerator report protocol. Enabled alone is
// deliberately insufficient: New also requires the actual nvidia-smi driver,
// so the development Fake driver can never be reported as NVIDIA hardware.
//
// DriverVersion and RuntimeVersion are display/profile metadata. The real
// nvidia-smi driver attests DriverVersion on every preflight, but configured
// RuntimeVersion is never itself runtime evidence: a dedicated local DCGM
// probe must attest an exact match before the report can be ready.
// PartitionTopology is a fail-closed fallback for drivers without a current
// topology probe. The real nvidia-smi driver reads current MIG mode each
// preflight and overrides this configured value.
type NVIDIAObservationConfig struct {
	Enabled        bool
	DriverVersion  string
	RuntimeVersion string
	// DCGMPath identifies the reviewed dcgmi binary used for a bounded local
	// runtime attestation. Empty means dcgmi from PATH.
	DCGMPath string
	// DCGMEndpoint is this node's DCGM host engine, e.g. "10.0.1.7:5555".
	// Empty uses dcgmi's local default. It must name this node: attestation
	// from another node's engine is evidence about the wrong hardware.
	DCGMEndpoint         string
	PartitionTopology    nvidia.PartitionTopology
	ProfileDigest        string
	UseControllerProfile bool
}

// Config configures the agent.
type Config struct {
	// NodeName defaults to the hostname.
	NodeName string
	// ControllerURL is the controller's base URL, e.g. https://controller:8443.
	ControllerURL string
	// Token authenticates requests. TokenFile is preferred because Kubernetes
	// rotates projected Pod-bound tokens in place.
	Token string
	// TokenFile is reread for every request so projected-token rotation does not
	// require an agent restart.
	TokenFile string
	// TLSCAFile verifies the controller's serving certificate. TLSCertFile and
	// TLSKeyFile identify this installation's agent fleet to the controller.
	TLSCAFile   string
	TLSCertFile string
	TLSKeyFile  string
	// AllowInsecureHTTP exists only for explicitly configured local development
	// stacks. The operator never enables it.
	AllowInsecureHTTP bool
	// SpoolPath is the on-disk event queue, default /var/lib/kube-neuron/spool.jsonl.
	SpoolPath string
	// ActionJournalPath is the crash-safe write-ahead journal for remediation
	// actions, default /var/lib/kube-neuron/actions.jsonl. It must remain on
	// durable node-local storage: a restarted agent uses it to avoid replaying
	// an action whose side effect may already have happened.
	ActionJournalPath string
	// ScriptsDir holds operator-provisioned remediation scripts, default
	// /etc/kube-neuron/scripts.
	ScriptsDir string
	// RebootCommand overrides how the host is rebooted. Empty uses the default,
	// which enters PID 1's namespaces and asks systemd. Set it for hosts with a
	// different init; it is never derived from a playbook or action parameter.
	RebootCommand []string
	// EnableDestructiveActions statically PINS the agent armed for its whole
	// process lifetime (a bare-metal or lab override; it requires the real
	// nvidia-smi driver). When false — the operator-managed default — the
	// agent boots UNARMED and adopts the controller-served arming answer
	// delivered with every v2 registration response, computed from
	// spec.safety.destructiveExecution.nodeSelector against this node's
	// labels. Every action still passes the controller's safety, capability,
	// and approval gates; this is the node-local boundary.
	EnableDestructiveActions bool
	// HealthListenAddress is the address serving /livez and /readyz.
	HealthListenAddress string
	// RegistrationInterval controls durable registration retries/heartbeats.
	RegistrationInterval time.Duration
	// RegistrationStaleAfter is how long the most recent durable controller
	// acknowledgment keeps /readyz ready.
	RegistrationStaleAfter time.Duration
	// NVIDIAObservation controls an optional NVIDIA runtime report. It has no
	// relationship to action execution or /readyz: report delivery failures
	// are retried independently and can never make this agent ready or execute
	// a remediation action.
	NVIDIAObservation NVIDIAObservationConfig
	// AMDDetection controls the optional AMD detection source. Like every
	// other second source it is observation-only and can never execute an
	// action.
	AMDDetection AMDDetectionConfig
}

// AMDDetectionConfig opts a node into the AMD detection source. Enabled alone
// is deliberately insufficient: New also requires amd-smi or rocm-smi to
// actually resolve on the node, so a declaration in a DaemonSet manifest can
// never turn a node with no AMD tooling into a source of AMD faults. That is
// the same real-binary-evidence rule the NVIDIA paths follow — there is no fake
// AMD driver to fall back to, by design.
type AMDDetectionConfig struct {
	Enabled bool
	// AMDSMIPath and ROCmSMIPath name the binaries; empty uses PATH lookup of
	// "amd-smi" / "rocm-smi". A binary that does not resolve stays disabled.
	AMDSMIPath  string
	ROCmSMIPath string
	// ThermalCriticalC is the hotspot temperature at or above which a reading
	// with no explicit throttle flag becomes a thermal fault. Zero (the
	// default) means a bare temperature is never promoted to a fault, because
	// the critical value is SKU-specific and this code cannot know it.
	ThermalCriticalC float64
	// CorrectableRateMinDelta is how many new corrected ECC errors must
	// accumulate before the rate fault reports again; zero uses the package
	// default.
	CorrectableRateMinDelta uint64
	// XGMILinkMinDelta is how many new XGMI link errors must accumulate before
	// the fabric fault reports again; zero uses the package default. The
	// counter also moves on corrected link retries, so with no delta a healthy
	// fabric under load raises a critical fault.
	XGMILinkMinDelta uint64
	// BadPageThreshold is the retired-page count at or above which the device
	// is reported as out of spare memory rather than as having retired a page
	// successfully. Zero (the default) never makes that claim, because the
	// bad-page budget is SKU-specific and this code cannot know it.
	BadPageThreshold uint64
}

// Agent is the per-node daemon.
type Agent struct {
	cfg      Config
	driver   nvml.GPUDriver
	executor *executor.Executor
	// armed is the LIVE arming state the executor consults per dispatch.
	// Seeded from cfg.EnableDestructiveActions; when that flag is false, each
	// v2 registration response's controller-served answer is adopted here. A
	// true flag PINS it for the process lifetime (a bare-metal override).
	armed   *atomic.Bool
	spool   *spool.Spool
	journal *actionjournal.Journal
	// journalLock remains held for the Agent lifetime. It ensures two local
	// processes cannot concurrently act on the same durable journal.
	journalLock *os.File
	watcher     *kmsg.Watcher
	health      *gpuhealth.Watcher
	amdHealth   *amdhealth.Watcher
	client      *http.Client
	log         *slog.Logger
	nvidia      *nvidiaObservation

	bootIDOnce sync.Once
	bootID     string
	// foreignVendorOnce keeps the "no inventory for this vendor" warning to one
	// line per process; the limitation is permanent, so repeating it per event
	// would only bury the events themselves.
	foreignVendorOnce sync.Once

	// detectionMu guards the short-window deduplication of detections across
	// every source (kmsg, the DCGM/nvidia-smi poll, the amd-smi/rocm-smi poll),
	// so one underlying fault seen by more than one does not open two
	// incidents.
	detectionMu   sync.Mutex
	detectionSeen map[string]time.Time

	registrationMu       sync.Mutex
	lastRegistrationAck  time.Time
	registrationAckSeq   uint64
	registrationLost     bool
	firstRegistrationTry time.Time
	now                  func() time.Time
	listen               func(network, address string) (net.Listener, error)
}

// nvidiaPreflighter makes report construction testable without treating a
// Fake GPUDriver as an actual NVIDIA runtime. Production construction is
// guarded in New by the concrete nvidia-smi driver check below.
type nvidiaPreflighter interface {
	Preflight(context.Context) nvidia.PreflightReport
}

type runtimeVersionProber interface {
	Version(context.Context) (string, error)
	GPUCount(context.Context) (int, error)
}

type nvidiaObservation struct {
	preflight                nvidiaPreflighter
	runtimeProber            runtimeVersionProber
	dcgmPath                 string
	driverVersion            string
	runtimeVersion           string
	profileDigest            string
	useControllerProfile     bool
	useObservedDriverVersion bool
}

// New assembles an agent.
func New(cfg Config, driver nvml.GPUDriver, log *slog.Logger) (*Agent, error) {
	if cfg.EnableDestructiveActions {
		if _, realSMI := driver.(*nvml.SMI); !realSMI {
			// Defense in depth beside the CLI guard: no constructor path may
			// arm destructive actions against a driver that fakes success.
			return nil, fmt.Errorf("destructive actions require the real nvidia-smi driver")
		}
	}
	if cfg.NVIDIAObservation.UseControllerProfile && !cfg.NVIDIAObservation.Enabled {
		return nil, fmt.Errorf("NVIDIA controller profile requires NVIDIA observation to be enabled")
	}
	if cfg.NVIDIAObservation.UseControllerProfile && strings.TrimSpace(cfg.NVIDIAObservation.ProfileDigest) != "" {
		return nil, fmt.Errorf("NVIDIA controller profile and static profile digest are mutually exclusive")
	}
	if cfg.NodeName == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil, err
		}
		cfg.NodeName = host
	}
	client, err := controllerHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.SpoolPath == "" {
		cfg.SpoolPath = "/var/lib/kube-neuron/spool.jsonl"
	}
	if cfg.ActionJournalPath == "" {
		cfg.ActionJournalPath = defaultActionJournalPath
	}
	if cfg.HealthListenAddress == "" {
		cfg.HealthListenAddress = defaultHealthListenAddress
	}
	if cfg.RegistrationInterval == 0 {
		cfg.RegistrationInterval = defaultRegistrationInterval
	}
	if cfg.RegistrationInterval < 0 {
		return nil, fmt.Errorf("registration interval must be positive")
	}
	if cfg.RegistrationStaleAfter == 0 {
		cfg.RegistrationStaleAfter = defaultRegistrationStaleAfter
	}
	if cfg.RegistrationStaleAfter < cfg.RegistrationInterval {
		return nil, fmt.Errorf("registration stale-after must be at least the registration interval")
	}
	// Acquire the node-singleton lock BEFORE opening (and self-healing) the spool.
	// The spool's torn-tail repair truncates an uncommitted tail; if a second
	// agent ran that repair while the first was mid-Append, it could truncate the
	// running agent's in-flight tail and the first agent's fsync would then
	// "succeed" on a record that no longer exists, letting the cursor ack an event
	// that was silently lost. Holding the exclusive lock first means only one
	// agent ever repairs or writes the spool. Linux releases the flock on process
	// death, so a genuinely dead prior agent does not wedge startup.
	journalLock, err := acquireActionJournalLock(cfg.ActionJournalPath)
	if err != nil {
		return nil, fmt.Errorf("locking action journal: %w", err)
	}
	sp, err := spool.Open(cfg.SpoolPath)
	if err != nil {
		_ = releaseActionJournalLock(journalLock)
		return nil, fmt.Errorf("opening spool: %w", err)
	}
	sp.Logger = log
	journal, err := actionjournal.Open(cfg.ActionJournalPath)
	if err != nil {
		_ = releaseActionJournalLock(journalLock)
		return nil, fmt.Errorf("opening action journal: %w", err)
	}
	armed := &atomic.Bool{}
	armed.Store(cfg.EnableDestructiveActions)
	exec := executor.NewWithOptions(driver, executor.Options{
		Armed: armed.Load,
	})
	if cfg.ScriptsDir != "" {
		exec.ScriptsDir = cfg.ScriptsDir
	}
	if len(cfg.RebootCommand) != 0 {
		exec.RebootCommand = append([]string(nil), cfg.RebootCommand...)
	}
	if cfg.EnableDestructiveActions {
		// A node that cannot reboot must say so now, not after an operator has
		// approved the most destructive step a playbook has. Reachability is
		// environment-specific, so this reports rather than refuses to start:
		// every other destructive action stays available.
		if err := exec.PreflightReboot(context.Background()); err != nil {
			log.Error("host reboot mechanism is not reachable; Reboot steps on this node will fail",
				"command", strings.Join(exec.RebootCommand, " "), "err", err)
		} else {
			log.Info("host reboot mechanism verified", "command", strings.Join(exec.RebootCommand, " "))
		}
	}
	watcher := kmsg.NewWatcher()
	// Persist the kmsg resume cursor beside the spool on durable node-local
	// storage, so a restart resumes from the last consumed XID instead of
	// seeking to the tail and silently dropping everything printed while down.
	watcher.CursorPath = filepath.Join(filepath.Dir(cfg.SpoolPath), "kmsg-cursor.json")
	watcher.Logger = log
	agent := &Agent{
		cfg:         cfg,
		driver:      driver,
		executor:    exec,
		armed:       armed,
		spool:       sp,
		journal:     journal,
		journalLock: journalLock,
		watcher:     watcher,
		client:      client,
		log:         log,
		now:         time.Now,
		listen:      net.Listen,
	}
	// Bind the kmsg cursor to the current boot. /dev/kmsg sequence numbers
	// restart near zero after a reboot, so a cursor from a prior boot must not
	// suppress the new boot's XIDs; a boot-ID mismatch makes the watcher fail
	// safe to tail-seek. Reuses the same host boot-ID source as registration.
	watcher.BootID = agent.currentBootID()
	// The DCGM/nvidia-smi health poll is a second, observation-only detection
	// source beside kmsg. It runs only on a real nvidia-smi runtime: the Fake
	// driver must never fabricate events, mirroring --require-real-driver, so a
	// CPU-only install simply has no second source rather than crashing.
	if smi, ok := driver.(*nvml.SMI); ok {
		health := gpuhealth.New(cfg.NodeName, cfg.NVIDIAObservation.DCGMPath, cfg.NVIDIAObservation.DCGMEndpoint, smi.Path)
		health.Logger = log
		health.ResolveGPU = agent.resolveGPUByIndex
		// Persist which retained XIDs have already been emitted, beside the spool
		// and kmsg cursor, boot-scoped like the cursor. Without this a routine
		// agent restart re-emits DCGM's retained last-XID on every node and
		// re-opens incidents for long-remediated faults.
		health.StatePath = filepath.Join(filepath.Dir(cfg.SpoolPath), "gpuhealth-state.json")
		health.BootID = agent.currentBootID()
		agent.health = health
	}
	// The AMD detection source is independent of the NVIDIA driver: an AMD node
	// runs no nvidia-smi, so gating it on the NVIDIA runtime would make it
	// permanently dead exactly where it is needed. Its evidence gate is its own
	// tooling, checked below, and it never touches the executor.
	if cfg.AMDDetection.Enabled {
		amd := amdhealth.New(cfg.NodeName, amdToolPath(cfg.AMDDetection.AMDSMIPath, "amd-smi"),
			amdToolPath(cfg.AMDDetection.ROCmSMIPath, "rocm-smi"))
		if !amd.Enabled() {
			// Requested but impossible. A source that polls nothing forever
			// looks like coverage on a dashboard, so refuse it out loud instead
			// of wiring a permanently silent watcher.
			log.Warn("AMD detection requested but disabled: neither amd-smi nor rocm-smi is present on this node")
		} else {
			amd.Logger = log
			amd.ThermalCriticalC = cfg.AMDDetection.ThermalCriticalC
			amd.CorrectableRateMinDelta = cfg.AMDDetection.CorrectableRateMinDelta
			amd.XGMILinkMinDelta = cfg.AMDDetection.XGMILinkMinDelta
			amd.BadPageThreshold = cfg.AMDDetection.BadPageThreshold
			agent.amdHealth = amd
			log.Info("AMD detection source enabled", "amd_smi", amd.AMDSMIPath, "rocm_smi", amd.ROCmSMIPath,
				"thermal_critical_c", cfg.AMDDetection.ThermalCriticalC)
		}
	}
	if cfg.NVIDIAObservation.Enabled {
		// A configuration declaration cannot make the simulator or another
		// vendor's GPUDriver into NVIDIA evidence. The command only constructs
		// *nvml.SMI after finding nvidia-smi, and this concrete check keeps the
		// same boundary for all programmatic callers.
		if _, realSMI := driver.(*nvml.SMI); !realSMI {
			log.Warn("NVIDIA observation disabled: no real nvidia-smi runtime evidence")
			return agent, nil
		}
		adapter, err := nvidia.New(nvidia.Config{
			NodeName:          cfg.NodeName,
			DriverVersion:     cfg.NVIDIAObservation.DriverVersion,
			RuntimeVersion:    cfg.NVIDIAObservation.RuntimeVersion,
			PartitionTopology: cfg.NVIDIAObservation.PartitionTopology,
			Now: func() time.Time {
				return agent.now()
			},
		}, driver)
		if err != nil {
			_ = releaseActionJournalLock(journalLock)
			return nil, fmt.Errorf("configure NVIDIA observation: %w", err)
		}
		agent.nvidia = &nvidiaObservation{
			preflight:                adapter,
			runtimeProber:            dcgm.NewWithEndpoint(cfg.NVIDIAObservation.DCGMPath, cfg.NVIDIAObservation.DCGMEndpoint),
			dcgmPath:                 cfg.NVIDIAObservation.DCGMPath,
			driverVersion:            cfg.NVIDIAObservation.DriverVersion,
			runtimeVersion:           cfg.NVIDIAObservation.RuntimeVersion,
			profileDigest:            cfg.NVIDIAObservation.ProfileDigest,
			useControllerProfile:     cfg.NVIDIAObservation.UseControllerProfile,
			useObservedDriverVersion: true,
		}
	}
	return agent, nil
}

// amdToolPath falls back to the bare tool name so amdhealth resolves it on
// PATH. An empty configured path must not disable the tool silently — that
// would make the default configuration a no-op source.
func amdToolPath(configured, defaultName string) string {
	if p := strings.TrimSpace(configured); p != "" {
		return p
	}
	return defaultName
}

// acquireActionJournalLock takes an exclusive, process-scoped lock on the
// companion journal lock file. Linux releases a flock automatically if the
// process dies, while a second live agent fails closed instead of racing the
// first agent's executor.
func acquireActionJournalLock(journalPath string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(journalPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func releaseActionJournalLock(lock *os.File) error {
	if lock == nil {
		return nil
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		_ = lock.Close()
		return err
	}
	return lock.Close()
}

func controllerHTTPClient(cfg Config) (*http.Client, error) {
	parsed, err := url.Parse(cfg.ControllerURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("controller URL must be an absolute HTTP(S) URL")
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	switch parsed.Scheme {
	case "http":
		if !cfg.AllowInsecureHTTP {
			return nil, fmt.Errorf("plain HTTP controller URL requires explicit insecure development mode")
		}
		if cfg.TLSCAFile != "" || cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" {
			return nil, fmt.Errorf("TLS files cannot be combined with a plain HTTP controller URL")
		}
	case "https":
		if cfg.Token == "" && cfg.TokenFile == "" {
			return nil, fmt.Errorf("HTTPS controller authentication requires a token or token file")
		}
		if cfg.Token != "" && cfg.TokenFile != "" {
			return nil, fmt.Errorf("controller token and token file are mutually exclusive")
		}
		if cfg.TLSCAFile == "" || cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
			return nil, fmt.Errorf("HTTPS controller authentication requires CA, client certificate, and client key files")
		}
		caPEM, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read controller CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("controller CA file contains no certificates")
		}
		metrics.RecordCertBundleExpiry("controller-server-ca", caPEM)
		certificate, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load agent client certificate: %w", err)
		}
		if leafPEM, err := os.ReadFile(cfg.TLSCertFile); err == nil {
			metrics.RecordCertBundleExpiry("fleet-client-leaf", leafPEM)
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			RootCAs:      roots,
			Certificates: []tls.Certificate{certificate},
		}}
	default:
		return nil, fmt.Errorf("controller URL scheme must be https")
	}
	return client, nil
}

// Run starts the agent loops and blocks until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.driver.Init(); err != nil {
		return fmt.Errorf("nvml init: %w", err)
	}
	defer func() { _ = a.driver.Shutdown() }()

	healthServer, healthErr, err := a.startHealthServer()
	if err != nil {
		return err
	}
	defer a.shutdownHealthServer(healthServer)

	if err := a.register(ctx); err != nil {
		a.log.Warn("initial registration failed, will retry", "err", err)
	}
	a.reportNVIDIA(ctx)

	events, err := a.watcher.Watch(ctx)
	if err != nil {
		// Non-fatal: the retry tick below keeps reopening the watcher.
		a.log.Error("kmsg watcher unavailable, will retry", "err", err)
	}

	// The DCGM/nvidia-smi second source polls on its own schedule. It never
	// returns an error (an unusable source degrades to observed-only), and a
	// nil channel simply never fires in the select below.
	var healthEvents <-chan types.AgentEvent
	if a.health != nil {
		if ch, herr := a.health.Watch(ctx); herr != nil {
			a.log.Error("gpu health second source unavailable", "err", herr)
		} else {
			healthEvents = ch
		}
	}

	// The AMD source is wired exactly like the NVIDIA one: same event type,
	// same handler, same dedup. Its only distinction downstream is the metric
	// label, so a fleet can see which vendor's tooling is actually reporting.
	var amdEvents <-chan types.AgentEvent
	if a.amdHealth != nil {
		if ch, aerr := a.amdHealth.Watch(ctx); aerr != nil {
			a.log.Error("AMD detection source unavailable", "err", aerr)
		} else {
			amdEvents = ch
		}
	}

	// Queued actions execute on their own loop: a long-running action (a
	// reset or reboot) must not stall event capture or heartbeats.
	go a.actionLoop(ctx)

	retry := time.NewTicker(a.cfg.RegistrationInterval)
	defer retry.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-healthErr:
			if err == nil {
				return fmt.Errorf("health server stopped unexpectedly")
			}
			return fmt.Errorf("health server: %w", err)
		case ev, ok := <-events:
			if !ok {
				// Watcher died; the retry tick reopens it. Detection must
				// never silently stay dark until an agent restart.
				events = nil
				a.log.Error("kmsg watcher stopped, will reopen")
				continue
			}
			if a.handleKernelEvent(ctx, ev) {
				// The watcher cursor is advanced only after this event is
				// accepted by the controller or fsynced to the local spool.
				// A cursor persistence failure is safe: the event can replay,
				// whereas acknowledging it early could lose it on restart.
				if err := ev.Acknowledge(); err != nil {
					a.log.Warn("kmsg cursor acknowledge failed; event may replay", "err", err)
				}
			}
		case ev, ok := <-healthEvents:
			if !ok {
				// The second source stopped; kmsg remains the primary path.
				// Detection must not silently go dark, so this is loud.
				healthEvents = nil
				a.log.Error("gpu health second source stopped")
				continue
			}
			a.handleDetection(ctx, ev, "gpuhealth")
		case ev, ok := <-amdEvents:
			if !ok {
				// Same rule as the NVIDIA second source: a source going dark is
				// loud, because silence is indistinguishable from health.
				amdEvents = nil
				a.log.Error("AMD detection source stopped")
				continue
			}
			a.handleDetection(ctx, ev, "amdhealth")
		case <-retry.C:
			if events == nil && ctx.Err() == nil {
				if ch, err := a.watcher.Watch(ctx); err != nil {
					a.log.Error("kmsg watcher reopen failed, will retry", "err", err)
				} else {
					a.log.Info("kmsg watcher reopened")
					events = ch
				}
			}
			if err := a.register(ctx); err != nil {
				a.log.Debug("registration heartbeat failed", "err", err)
			}
			a.reportNVIDIA(ctx)
			a.flushSpool(ctx)
		}
	}
}

// handleKernelEvent maps a kernel fault line to a GPU and pushes it to the
// controller. Both vendors' kernel families arrive here; only the NVIDIA XID
// path may consult the NVML inventory for attribution.
func (a *Agent) handleKernelEvent(ctx context.Context, ev kmsg.Event) bool {
	detection := types.AgentEvent{
		Node:      a.cfg.NodeName,
		GPUIndex:  -1,
		PCIAddr:   ev.PCIAddr,
		XID:       ev.XID,
		Fault:     ev.Fault,
		Raw:       ev.Raw,
		Timestamp: ev.Timestamp,
	}
	if ev.Fault != nil && ev.Fault.Vendor != string(types.AcceleratorVendorNVIDIA) {
		// The only inventory this agent has is NVIDIA's. Asking it to resolve
		// an AMD PCI address could only ever produce a wrong answer or an
		// error, and an AMD fault filed against an NVIDIA GPU's index/UUID
		// would point every downstream decision — incident target, holder
		// check, remediation — at the wrong device. Vendor-scoped inventory is
		// §1.3 and does not exist yet, so the honest result is an event that is
		// addressed by PCI address and attributed to nothing.
		a.logForeignVendorKernelFault(ev)
		return a.handleDetection(ctx, detection, "kmsg-"+ev.Fault.Vendor)
	}
	gpu, err := a.driver.GPUByPCIAddr(ctx, ev.PCIAddr)
	if err != nil {
		a.log.Warn("cannot resolve GPU for XID", "pci", ev.PCIAddr, "xid", ev.XID, "err", err)
		// An unattributed event must say so: index 0 is a real GPU, and
		// evidence blaming it for another device's XID misleads operators.
		gpu.Index = -1
		gpu.UUID = ""
	}
	detection.GPUIndex = gpu.Index
	detection.GPUUUID = gpu.UUID
	return a.handleDetection(ctx, detection, "kmsg")
}

// logForeignVendorKernelFault records that a non-NVIDIA kernel fault could not
// be attributed to a device. The limitation is reported loudly ONCE — an
// operator must know this agent cannot name AMD devices — and then per event at
// debug, so a ring-timeout storm cannot drown the log.
func (a *Agent) logForeignVendorKernelFault(ev kmsg.Event) {
	a.foreignVendorOnce.Do(func() {
		a.log.Warn("kernel fault from a non-NVIDIA accelerator: this agent has no inventory for that vendor, so its events stay unattributed (addressed by PCI address only)",
			"vendor", ev.Fault.Vendor, "code", ev.Fault.Code, "pci", ev.PCIAddr)
	})
	a.log.Debug("unattributed vendor kernel fault",
		"vendor", ev.Fault.Vendor, "code", ev.Fault.Code, "pci", ev.PCIAddr, "raw", ev.Raw)
}

// handleDetection is the shared path every detection source funnels through: it
// deduplicates a fault seen by more than one source within a short window, then
// posts (spooling on failure). The source label is observability only; it never
// changes handling or enables any action.
func (a *Agent) handleDetection(ctx context.Context, ev types.AgentEvent, source string) bool {
	metrics.AgentDetections.WithLabelValues(source).Inc()
	// The coarse node+XID cross-source anchor can only safely collapse an
	// unattributed observation into a later attributed one on a single-GPU node
	// (see duplicateDetection). Determine that off the driver inventory here,
	// OUTSIDE the dedup lock and only when it could matter (an attributed event),
	// so the bounded inventory probe never blocks the dedup fast path or holds the
	// lock across a subprocess.
	singleGPU := false
	if isAttributed(ev) {
		singleGPU = a.nodeIsSingleGPU(ctx)
	}
	if a.duplicateDetection(ev, a.now(), singleGPU) {
		metrics.AgentDetectionsDeduplicated.WithLabelValues(source).Inc()
		a.log.Debug("duplicate detection suppressed",
			"source", source, "xid", ev.XID, "gpu_index", ev.GPUIndex, "gpu_uuid", ev.GPUUUID)
		// The original equivalent detection was already accepted by the
		// controller or spool before it entered the dedup set, so treating this
		// duplicate as durably covered cannot lose an event.
		return true
	}
	if ev.EventID == "" {
		ev.EventID = newEventID()
	}
	durable := false
	if err := a.post(ctx, "/api/v1/events", ev); err != nil {
		if errors.Is(err, errEventRejected) {
			// The controller refused this exact payload; spooling it would put a
			// permanent head-of-line blocker in front of every later detection.
			// Drop it loudly. Deliberately NOT remembered into the dedup set: the
			// sibling source's representation of the same fault (a different,
			// controller-acceptable encoding) must stay deliverable — remembering
			// the rejected copy would silence the healthy source too, widening
			// the loss beyond the malformed payload. The re-emit/re-reject churn
			// this allows is bounded by the source's own detection pacing and is
			// visible in the rejected counter.
			metrics.AgentEventsRejected.Inc()
			a.log.Error("event permanently rejected by the controller; dropping it (NOT spooling)",
				"source", source, "xid", ev.XID, "gpu_index", ev.GPUIndex, "err", err)
			return true
		}
		a.log.Warn("event push failed, spooling", "source", source, "xid", ev.XID, "err", err)
		if err := a.spool.Append(ev); err != nil {
			a.log.Error("spool append failed", "err", err)
		} else {
			metrics.AgentEventsSpooled.Inc()
			durable = true
		}
	} else {
		metrics.AgentEventsPosted.Inc()
		durable = true
	}
	if durable {
		a.rememberDetection(ev, a.now())
	}
	metrics.AgentSpoolDepth.Set(float64(a.spool.Len()))
	return durable
}

// duplicateDetection reports whether an equivalent fault (same GPU and XID) was
// already accepted by the controller or the durable spool within
// detectionDedupWindow. It deduplicates across sources — the kmsg watcher and
// the DCGM/nvidia-smi poll routinely observe the same underlying fault — so two
// sources cannot open two incidents for one event. Failed deliveries are never
// remembered, since suppressing their retry would lose the only copy.
func (a *Agent) duplicateDetection(ev types.AgentEvent, now time.Time, singleGPU bool) bool {
	a.detectionMu.Lock()
	defer a.detectionMu.Unlock()
	a.expireDetectionsLocked(now)
	// An event's own precise key is the per-GPU identity when it is attributed,
	// and the node+native fallback when it is not. This is source-native, so a
	// recurrence of the SAME fault from the same or another source collapses,
	// while two distinct XIDs that share a class do not.
	if a.seenWithinLocked(detectionKey(ev), now) {
		return true
	}
	if isAttributed(ev) {
		// Cross-source, cross-representation: an XID and the neutral fault for the
		// same condition on the same GPU share a ProblemClass. Probe the OTHER
		// representation's class anchor so a kmsg XID and the nvidia-smi neutral
		// fault collapse to one incident, while two XIDs of one class do not.
		if class, ok := detect.FaultClass(ev); ok &&
			a.seenWithinLocked(classAnchorKey(ev, otherRepresentationTag(ev), class), now) {
			return true
		}
		// Everything below this point exists to collapse this ATTRIBUTED
		// observation into a PRIOR UNATTRIBUTED one of the same fault (the XID-79
		// case: the device falls off the bus, so the fast kmsg path records index
		// -1/uuid "" while the later DCGM poll resolves index|uuid|79). Both keys
		// are only ever WRITTEN by unattributed events, so a hit here always means
		// "the vague observation went first".
		//
		// That collapse is not free, and for a long time this window paid for it
		// with the one thing the fleet could least afford to lose. Suppressing the
		// attributed observation here means the controller never learns the device's
		// UUID: the incident it already opened stays addressed by PCI address alone,
		// and an empty GPU UUID is read downstream as a PERMANENT infeasibility. The
		// playbook cordons the node, drains every tenant job off it, reaches the
		// reset rung, refuses it, and parks the node for a human — although the
		// exact device had been identified two seconds after the fault.
		//
		// So when this event carries a PCI address it is NOT a duplicate, it is a
		// promotion, and it must be delivered. The controller matches it to the
		// open incident on that same address and promotes the incident onto this
		// UUID, which collapses the two observations into one incident just as this
		// window used to — only without throwing the device identity away. The
		// extra traffic is exactly one event per device per fault per window: the
		// attributed observation's own precise key is remembered on delivery, so a
		// repeat of it is still suppressed above.
		if ev.PCIAddr != "" {
			return false
		}
		// No bus address on this event, so nothing downstream can tie it to the
		// earlier observation and no promotion is possible — the choice here is
		// only between one incident and two. The coarse node+XID anchor cannot
		// tell two GPUs apart: on a multi-GPU node GPU1's unattributed XID 79
		// would suppress GPU0's later ATTRIBUTED XID 79 and lose GPU0's fault
		// entirely. So it is restricted to a single-GPU node, where node+XID is
		// unambiguous. On a multi-GPU node a duplicate incident (safe) is
		// preferred over a lost fault.
		//
		// KNOWN RESIDUAL COST: on a single-GPU node this still trades the UUID for
		// a single incident, so an attributed observation that arrives without a
		// bus address cannot promote. Closing it needs the device address on the
		// attributed side too (the vendor inventory knows it), which is a change
		// to the driver inventory contract rather than to this window.
		if singleGPU && a.seenWithinLocked(nodeFaultKey(ev), now) {
			return true
		}
	}
	return false
}

// nodeIsSingleGPU reports whether the node has exactly one GPU. It fails closed:
// if the inventory cannot be read, it returns false so the coarse node+XID
// collapse is NOT applied and a genuine per-GPU fault is never suppressed.
func (a *Agent) nodeIsSingleGPU(ctx context.Context) bool {
	gpus, err := a.driver.ListGPUs(ctx)
	if err != nil || len(gpus) == 0 {
		return false
	}
	return len(gpus) == 1
}

func (a *Agent) rememberDetection(ev types.AgentEvent, now time.Time) {
	a.detectionMu.Lock()
	defer a.detectionMu.Unlock()
	a.expireDetectionsLocked(now)
	// Store the event's own precise key: an attributed event under its per-GPU
	// key, an unattributed one under its fallback key (node+PCI+native when the
	// PCI address is known, else node+native).
	a.detectionSeen[detectionKey(ev)] = now
	if isAttributed(ev) {
		// Record this GPU's class anchor under this event's OWN representation, so
		// the other representation of the same condition (an XID vs the neutral
		// fault) dedups against it in duplicateDetection. Two events of the same
		// representation never probe their own tag, so distinct XIDs sharing a
		// class are not collapsed here.
		if class, ok := detect.FaultClass(ev); ok {
			a.detectionSeen[classAnchorKey(ev, representationTag(ev), class)] = now
		}
		return
	}
	// An unattributed event that carries a PCI address is remembered under the
	// coarse node+native key as well, so a LATER attributed observation of the
	// same node fault — which may not carry a PCI address (the DCGM poll resolves
	// an index/UUID but not the bus address) — still collapses via the cross-source
	// probe. Distinct unattributed GPUs stay separate because their own lookup
	// uses the precise per-PCI key, never this coarse anchor.
	if ev.PCIAddr != "" {
		a.detectionSeen[nodeFaultKey(ev)] = now
	}
}

// isAttributed reports whether an event carries a trustworthy per-GPU identity.
// An unresolved kmsg event (device gone) uses index -1 and an empty UUID.
func isAttributed(ev types.AgentEvent) bool {
	return ev.GPUIndex >= 0 && ev.GPUUUID != ""
}

// detectionKey is the PRECISE dedup identity for an event: the per-GPU key when
// attributed, else the unattributed fallback. The fallback folds in the PCI
// address when the event carries one, so two distinct GPUs that fall off the bus
// with the same fault on one node (both unattributed: index -1, empty UUID) do
// not collapse into a single detection and lose one GPU's fault. It uses the
// source-native identity (raw XID / vendor+code), NOT the ProblemClass, so two
// distinct XIDs that share a class are never over-collapsed by same-source dedup.
func detectionKey(ev types.AgentEvent) string {
	if !isAttributed(ev) {
		if ev.PCIAddr != "" {
			return pciFaultKey(ev)
		}
		return nodeFaultKey(ev)
	}
	return fmt.Sprintf("%d|%s|%s", ev.GPUIndex, ev.GPUUUID, nativeToken(ev))
}

// pciFaultKey is the per-device fallback identity for an event carrying a PCI
// address but no resolved UUID: node+PCI+native. Because the PCI address names
// one physical device, an attributed observation that carries a PCI address can
// safely collapse against a prior unattributed one under this key without any
// risk of suppressing a different GPU's fault — unlike the coarse node+XID key.
// The address is normalized through the one shared rule rather than compared
// raw: kmsg prints "0000:c3:00.0" and a vendor poll may report "00000000:C3:00.0"
// for the same slot, and two spellings of one device would key two detections,
// so the fault would be posted twice and the recurrence guard would never fire.
func pciFaultKey(ev types.AgentEvent) string {
	return fmt.Sprintf("node|%s|%s|%s", ev.Node, types.NormalizePCIAddress(ev.PCIAddr), nativeToken(ev))
}

// nodeFaultKey is the coarse node+fault fallback identity, used both when a PCI
// address is truly absent and as the cross-source anchor that lets a later
// attributed observation (which may not carry a PCI address) collapse against a
// prior unattributed one of the same node fault — the XID-79 fell-off-the-bus
// case, where both observations carry the SAME native XID. It uses the native
// token so an unattributed XID 13 and an attributed XID 31 (distinct XIDs) are
// not collapsed by a shared class.
func nodeFaultKey(ev types.AgentEvent) string {
	return fmt.Sprintf("node|%s|%s", ev.Node, nativeToken(ev))
}

// nativeToken is the source-native fault identity: the vendor+code for a neutral
// fault, else the raw XID. It is the PRECISE per-GPU dedup identity, so two
// distinct XIDs that merely share a ProblemClass (13/31/43/46 -> ClassXIDApp,
// 48/95 -> ClassECCDBE, 119/120 -> ClassGSPError) keep distinct keys and are
// never collapsed into one detection. The class is used only for the separate
// cross-source anchors below.
func nativeToken(ev types.AgentEvent) string {
	if ev.Fault != nil {
		return "fault|" + ev.Fault.Vendor + "|" + ev.Fault.Code
	}
	return "xid|" + strconv.Itoa(ev.XID)
}

// representationTag distinguishes the two encodings one condition can arrive in:
// a genuine XID (kmsg NRVM line, DCGM last-XID) versus a vendor-neutral fault
// (the nvidia-smi ECC/remap fallback). The cross-source class anchor is keyed by
// this tag so an XID and a neutral fault of the SAME class bridge to one another,
// while two XIDs (same tag) never do.
func representationTag(ev types.AgentEvent) string {
	if ev.Fault != nil {
		return "fault"
	}
	return "xid"
}

func otherRepresentationTag(ev types.AgentEvent) string {
	if ev.Fault != nil {
		return "xid"
	}
	return "fault"
}

// classAnchorKey is the GPU-scoped, class-based, representation-tagged
// cross-source anchor. An attributed event stores it under its OWN representation
// and probes it under the OTHER, so a kmsg XID 48 and an nvidia-smi neutral
// "ecc-dbe" on one GPU (the same condition, two encodings) dedup to one incident,
// while a kmsg XID 13 and a kmsg XID 31 on one GPU (same class, same encoding)
// stay two distinct detections.
func classAnchorKey(ev types.AgentEvent, tag string, class types.ProblemClass) string {
	return fmt.Sprintf("classanchor|%s|%d|%s|%s", tag, ev.GPUIndex, ev.GPUUUID, class)
}

// expireDetectionsLocked drops dedup entries older than the window. Called with
// detectionMu held.
func (a *Agent) expireDetectionsLocked(now time.Time) {
	if a.detectionSeen == nil {
		a.detectionSeen = make(map[string]time.Time)
		return
	}
	for k, seenAt := range a.detectionSeen {
		if now.Sub(seenAt) > detectionDedupWindow {
			delete(a.detectionSeen, k)
		}
	}
}

// seenWithinLocked reports whether key was recorded inside the dedup window.
// Called with detectionMu held.
func (a *Agent) seenWithinLocked(key string, now time.Time) bool {
	last, ok := a.detectionSeen[key]
	return ok && now.Sub(last) < detectionDedupWindow
}

// resolveGPUByIndex maps a DCGM GPU index to inventory so the second source can
// attribute a UUID. A failed or missing lookup returns ok=false and the caller
// keeps the trustworthy index alone.
func (a *Agent) resolveGPUByIndex(ctx context.Context, index int) (types.GPUInfo, bool) {
	gpus, err := a.driver.ListGPUs(ctx)
	if err != nil {
		return types.GPUInfo{}, false
	}
	for _, gpu := range gpus {
		if gpu.Index == index {
			return gpu, true
		}
	}
	return types.GPUInfo{}, false
}

// flushSpool drains the spool in batches until it is empty, a send fails, or
// the per-tick time budget runs out — so an outage backlog clears in minutes
// while the registration heartbeat is never starved.
func (a *Agent) flushSpool(ctx context.Context) {
	flushCtx, cancel := context.WithTimeout(ctx, spoolFlushBudget)
	defer cancel()
	for {
		rejected := 0
		sent, err := a.spool.ReplayBatch(flushCtx, spoolReplayBatchSize, func(ctx context.Context, event types.AgentEvent) error {
			err := a.post(ctx, "/api/v1/events", event)
			if errors.Is(err, errEventRejected) {
				// Replay is strictly head-of-line: one permanently rejected
				// event would block every detection behind it forever. Drop it
				// loudly and keep draining. Counted below, only once the batch's
				// removal from the spool commits — a failed rewrite leaves these
				// events in place to be re-rejected, and counting here would
				// tally them twice.
				rejected++
				a.log.Error("spooled event permanently rejected by the controller; dropping it",
					"event_id", event.EventID, "xid", event.XID, "err", err)
				return nil
			}
			return err
		})
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
				a.log.Warn("spool replay stopped", "err", err, "remaining", a.spool.Len())
			}
			return
		}
		if sent > 0 {
			metrics.AgentEventsPosted.Add(float64(sent - rejected))
			metrics.AgentEventsRejected.Add(float64(rejected))
			metrics.AgentSpoolDepth.Set(float64(a.spool.Len()))
		}
		if sent < spoolReplayBatchSize {
			return // drained
		}
	}
}

// currentBootID reads the node boot identity once; empty when unavailable
// (non-Linux dev hosts). The server treats an empty boot ID as "no guard".
func (a *Agent) currentBootID() string {
	a.bootIDOnce.Do(func() {
		data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err != nil {
			return
		}
		a.bootID = strings.TrimSpace(string(data))
	})
	return a.bootID
}

// newEventID returns a random capture-time event identity; the controller
// deduplicates at-least-once replays on it.
func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a time-based ID; uniqueness is per node and per capture.
		return fmt.Sprintf("t-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// actionLoop polls the controller's work queue and executes actions
// sequentially. Sequential execution is deliberate: node-level remediation
// steps must never race each other on one node.
func (a *Agent) actionLoop(ctx context.Context) {
	tick := time.NewTicker(actionPollInterval)
	defer tick.Stop()
	// Resume a locally durable outcome before asking the controller for another
	// action. This uses the original persisted lease, so a restart cannot
	// invalidate another live agent's claim by polling again.
	a.recoverQueuedActions(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.recoverQueuedActions(ctx)
			for a.runQueuedAction(ctx) {
				// Drain the queue before sleeping again.
			}
		}
	}
}

// runQueuedAction fetches and processes at most one controller action. The
// local journal is the source of truth for execution: intent and the exact
// controller lease are fsynced before the running marker, and a durable result
// is always retried before the executor is allowed to see that action ID again.
// It reports whether an action was processed.
func (a *Agent) runQueuedAction(ctx context.Context) bool {
	claimed, ok := a.fetchAction(ctx)
	if !ok {
		return false
	}
	action := claimed.action
	if _, err := a.journal.RecordReceived(action); err != nil {
		// In particular, do not let an action-ID conflict evade the durable
		// intent record. The controller will retry the lease and the operator
		// can investigate the conflicting request without a second side effect.
		a.log.Error("cannot record queued action intent", "action", action.ID, "type", action.Type, "err", err)
		return false
	}
	entry, err := a.journal.SetClaim(action.ID, claimed.leaseToken, claimed.leaseExpiresAt)
	if err != nil {
		a.log.Error("cannot record queued action lease", "action", action.ID, "type", action.Type, "err", err)
		return false
	}
	return a.processJournaledAction(ctx, entry)
}

// recoverQueuedActions resumes locally durable actions before normal polling.
// Only the exact persisted lease can authorize recovery. An expired or absent
// claim is deliberately left untouched until the controller reclaims it and
// sends a new lease through fetchAction.
func (a *Agent) recoverQueuedActions(ctx context.Context) {
	for _, entry := range a.journal.ListRecoverable() {
		if !claimStillValid(entry, time.Now()) {
			a.log.Debug("journaled action waits for controller lease reclaim", "action", entry.Action.ID, "state", entry.State)
			continue
		}
		if !a.processJournaledAction(ctx, entry) {
			// Retrying this persisted lease on the next tick is safer than
			// processing a later entry after its predecessor could not report.
			return
		}
	}
}

// processJournaledAction resumes an action only while its persisted lease is
// valid. StateRunning is never executed: it is converted to an explicit
// unknown outcome before reporting.
func (a *Agent) processJournaledAction(ctx context.Context, entry actionjournal.Entry) bool {
	if !claimStillValid(entry, time.Now()) {
		return false
	}
	claimed := claimedAction{
		action:         entry.Action,
		leaseToken:     entry.LeaseToken,
		leaseExpiresAt: entry.LeaseExpiresAt,
	}
	switch entry.State {
	case actionjournal.StateReceived:
		return a.executeQueuedAction(ctx, claimed)
	case actionjournal.StateOutcomeKnown:
		if entry.Result == nil {
			a.log.Error("action journal has known outcome without result", "action", entry.Action.ID)
			return false
		}
		return a.reportJournaledAction(ctx, claimed, entry, *entry.Result)
	case actionjournal.StateOutcomeUnknown:
		return a.reportJournaledAction(ctx, claimed, entry, unknownActionResult(entry))
	case actionjournal.StateRunning:
		// A running marker means an earlier process may have crossed the side
		// effect boundary. It normally becomes outcome-unknown when the journal
		// opens after a restart; make the same conservative conversion if the
		// marker is encountered while this process is alive.
		unknownEntry, err := a.journal.MarkOutcomeUnknown(entry.Action.ID)
		if err != nil {
			a.log.Error("cannot mark interrupted action outcome unknown", "action", entry.Action.ID, "err", err)
			return false
		}
		return a.reportJournaledAction(ctx, claimed, unknownEntry, unknownActionResult(unknownEntry))
	case actionjournal.StateReported:
		return true
	default:
		a.log.Error("action journal returned unsupported state", "action", entry.Action.ID, "state", entry.State)
		return false
	}
}

// executeQueuedAction performs an action only after both its intent and the
// running marker have been fsynced. A missing executor result is deliberately
// treated as unknown: even if the context was canceled, the executor might
// have reached a device or host side effect before returning.
func (a *Agent) executeQueuedAction(ctx context.Context, claimed claimedAction) bool {
	action := claimed.action
	if _, err := a.journal.MarkRunning(action.ID); err != nil {
		a.log.Error("cannot record queued action as running", "action", action.ID, "type", action.Type, "err", err)
		return false
	}

	a.log.Info("executing queued action", "action", action.ID, "type", action.Type)
	result, err := a.executor.Execute(ctx, action)
	if err != nil {
		a.log.Warn("queued action failed", "action", action.ID, "type", action.Type, "err", err)
	}
	if result == nil {
		entry, markErr := a.journal.MarkOutcomeUnknown(action.ID)
		if markErr != nil {
			a.log.Error("cannot record queued action outcome as unknown", "action", action.ID, "err", markErr)
			return false
		}
		return a.reportJournaledAction(ctx, claimed, entry, unknownActionResult(entry))
	}

	entry, err := a.journal.RecordOutcome(action.ID, *result)
	if err != nil {
		// The action ran, but without a durable outcome it cannot safely be
		// retried. Persist an explicit unknown marker before reporting that
		// ambiguity to the controller.
		a.log.Error("cannot record queued action outcome", "action", action.ID, "err", err)
		unknown, unknownErr := a.journal.MarkOutcomeUnknown(action.ID)
		if unknownErr != nil {
			a.log.Error("cannot record queued action outcome as unknown", "action", action.ID, "err", unknownErr)
			return false
		}
		return a.reportJournaledAction(ctx, claimed, unknown, unknownActionResult(unknown))
	}
	return a.reportJournaledAction(ctx, claimed, entry, *result)
}

// reportJournaledAction posts an outcome that was recorded before the network
// request. On a failed POST a restart can replay it with this exact persisted
// lease; after expiry, the agent waits for a normal controller re-claim before
// it attempts another POST.
func (a *Agent) reportJournaledAction(ctx context.Context, claimed claimedAction, entry actionjournal.Entry, result types.ActionResult) bool {
	if entry.LeaseToken != claimed.leaseToken || !entry.LeaseExpiresAt.Equal(claimed.leaseExpiresAt) {
		a.log.Error("journaled action lease changed before result post", "action", entry.Action.ID)
		return false
	}
	resultPath := fmt.Sprintf("%s/%s/result", types.AgentActionLeasePath, entry.Action.ID)
	if err := a.postActionResult(ctx, resultPath, entry.LeaseToken, &result); err != nil {
		a.log.Warn("posting action result failed, will retry", "action", entry.Action.ID, "state", entry.State, "err", err)
		return false
	}
	if _, err := a.journal.MarkReported(entry.Action.ID); err != nil {
		// The controller acknowledged the result, but we need the journal to
		// remember that acknowledgement before considering this action settled.
		a.log.Error("cannot record action result acknowledgement", "action", entry.Action.ID, "err", err)
		return false
	}
	return true
}

func claimStillValid(entry actionjournal.Entry, now time.Time) bool {
	return entry.LeaseToken != "" && !entry.LeaseExpiresAt.IsZero() && entry.LeaseExpiresAt.After(now)
}

func unknownActionResult(entry actionjournal.Entry) types.ActionResult {
	timestamp := entry.UpdatedAt
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	return types.ActionResult{
		ActionID:   entry.Action.ID,
		OK:         false,
		Error:      "action outcome is unknown after an interrupted agent; automatic retry refused",
		StartedAt:  timestamp,
		FinishedAt: timestamp,
	}
}

// fetchAction asks the controller for the oldest queued action.
type claimedAction struct {
	action         types.Action
	leaseToken     string
	leaseExpiresAt time.Time
}

func (a *Agent) fetchAction(ctx context.Context) (claimedAction, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.ControllerURL+types.AgentActionLeasePath, nil)
	if err != nil {
		return claimedAction{}, false
	}
	if bootID := a.currentBootID(); bootID != "" {
		req.Header.Set(types.AgentBootIDHeader, bootID)
	}
	if err := a.authorize(req); err != nil {
		return claimedAction{}, false
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return claimedAction{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return claimedAction{}, false
	}
	if resp.StatusCode != http.StatusOK {
		a.log.Debug("action poll failed", "status", resp.Status)
		return claimedAction{}, false
	}
	leaseToken := resp.Header.Get(types.AgentActionLeaseHeader)
	if leaseToken == "" {
		a.log.Warn("action poll returned no lease token")
		return claimedAction{}, false
	}
	leaseExpiresAt, err := time.Parse(time.RFC3339Nano, resp.Header.Get(types.AgentActionLeaseExpiresHeader))
	if err != nil || !leaseExpiresAt.After(time.Now()) {
		a.log.Warn("action poll returned invalid or expired lease", "err", err)
		return claimedAction{}, false
	}
	var action types.Action
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxActionBytes))
	if err := dec.Decode(&action); err != nil || action.ID == "" {
		a.log.Warn("action poll returned undecodable action", "err", err)
		return claimedAction{}, false
	}
	return claimedAction{action: action, leaseToken: leaseToken, leaseExpiresAt: leaseExpiresAt}, true
}

// register reports identity, GPU inventory, and boot ID to the controller.
func (a *Agent) register(ctx context.Context) error {
	// Prefer the v2 protocol (arming always declared); fall back to v1
	// against an older controller, OMITTING the arming field entirely — the
	// v1 handler strict-decodes and must keep rejecting extended payloads.
	// The probe runs every registration tick, so a controller upgrade is
	// picked up within one tick with no capability caching.
	path := types.AgentRegistrationPath
	var armed *bool
	v2 := false
	if err := a.requireCapability(ctx, types.AgentRegistrationV2Path, types.AgentRegistrationV2Protocol); err == nil {
		path = types.AgentRegistrationV2Path
		v2 = true
		// Report the CURRENT effective state — what this process would do if
		// handed a destructive action right now — not the boot flag. The
		// controller's admission gate reasons about this declaration.
		effective := a.armed.Load()
		armed = &effective
	} else if err := a.requireCapability(ctx, types.AgentRegistrationPath, types.AgentRegistrationProtocol); err != nil {
		a.recordRegistrationFailure(err)
		return err
	}
	gpus, err := a.driver.ListGPUs(ctx)
	if err != nil {
		a.recordRegistrationFailure(err)
		return err
	}
	bootID, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	registration := types.AgentRegistration{
		Name:             a.cfg.NodeName,
		GPUs:             gpus,
		BootID:           string(bytes.TrimSpace(bootID)),
		DestructiveArmed: armed,
	}
	served, err := a.postExpectStatusHeader(ctx, path, registration, http.StatusNoContent, types.AgentArmingHeader)
	if err != nil {
		a.recordRegistrationFailure(err)
		return err
	}
	// Adopt the controller-served arming answer, unless the flag pinned this
	// process armed. An absent header (an older controller, or the v1 route)
	// keeps the current state — which defaults to unarmed at boot.
	if v2 && !a.cfg.EnableDestructiveActions {
		switch types.AgentArming(served) {
		case types.AgentArmingArmed:
			if _, realSMI := a.driver.(*nvml.SMI); !realSMI {
				// The same defense-in-depth as the static flag's constructor
				// guard: nothing may arm destructive actions against a driver
				// that fakes success.
				a.log.Error("controller served an armed answer but this agent runs a non-real GPU driver; staying unarmed")
				break
			}
			if a.armed.CompareAndSwap(false, true) {
				a.log.Warn("controller armed this node's destructive executor",
					"reason", "node matches spec.safety.destructiveExecution.nodeSelector")
			}
		case types.AgentArmingUnarmed:
			if a.armed.CompareAndSwap(true, false) {
				a.log.Warn("controller DISARMED this node's destructive executor",
					"reason", "node no longer matches spec.safety.destructiveExecution.nodeSelector")
			}
		}
	}
	a.recordRegistrationAcknowledgment()
	return nil
}

// reportNVIDIA publishes the current NVIDIA preflight snapshot when, and only
// when, the agent was explicitly configured with a real nvidia-smi runtime.
// It is deliberately independent of registration acknowledgments: a failed
// report must not affect /readyz, action polling, or any remediation path.
// The next registration tick reconstructs a fresh report and retries it.
func (a *Agent) reportNVIDIA(ctx context.Context) {
	if a.nvidia == nil {
		return
	}
	profile := types.AgentAcceleratorObservationProfile{
		Vendor:        types.AcceleratorVendorNVIDIA,
		ProfileDigest: a.nvidia.profileDigest,
	}
	if a.nvidia.useControllerProfile {
		selected, found, err := a.fetchAcceleratorObservationProfile(ctx, types.AcceleratorVendorNVIDIA)
		if err != nil {
			a.log.Warn("NVIDIA controller profile lookup failed; will retry", "err", err)
			return
		}
		if !found {
			a.log.Debug("NVIDIA observation held: no controller runtime profile selects this node")
			return
		}
		profile = selected
	}
	report, err := a.nvidiaAcceleratorReportWithProfile(ctx, profile)
	if err != nil {
		a.log.Warn("NVIDIA accelerator preflight report failed; will retry", "err", err)
		return
	}
	if err := a.postExpectStatus(ctx, types.AgentAcceleratorReportPath, report, http.StatusNoContent); err != nil {
		a.log.Warn("NVIDIA accelerator report post failed; will retry", "err", err)
	}
}

// nvidiaAcceleratorReport translates the NVIDIA adapter's internal snapshot
// into the versioned, vendor-neutral wire report. The conversion is explicit
// so a future adapter addition cannot silently become an executable claim.
// It does not call the executor and it validates the completed wire value
// before the caller can post it.
func (a *Agent) nvidiaAcceleratorReport(ctx context.Context) (types.AgentAcceleratorReport, error) {
	if a.nvidia == nil {
		return types.AgentAcceleratorReport{}, fmt.Errorf("NVIDIA observation is not configured")
	}
	return a.nvidiaAcceleratorReportWithProfile(ctx, types.AgentAcceleratorObservationProfile{
		Vendor:         types.AcceleratorVendorNVIDIA,
		ProfileDigest:  a.nvidia.profileDigest,
		RuntimeVersion: a.nvidia.runtimeVersion,
	})
}

// nvidiaAcceleratorReportWithProfile builds a report with the profile binding
// chosen for this single publication. Controller-profile mode deliberately
// passes the freshly authenticated response as a local value instead of
// mutating shared agent configuration between heartbeat ticks.
func (a *Agent) nvidiaAcceleratorReportWithProfile(ctx context.Context, profile types.AgentAcceleratorObservationProfile) (types.AgentAcceleratorReport, error) {
	if a.nvidia == nil || a.nvidia.preflight == nil {
		return types.AgentAcceleratorReport{}, fmt.Errorf("NVIDIA observation is not configured")
	}
	if err := ctx.Err(); err != nil {
		return types.AgentAcceleratorReport{}, err
	}
	preflightCtx, cancel := context.WithTimeout(ctx, nvidiaReportTimeout)
	defer cancel()
	snapshot := a.nvidia.preflight.Preflight(preflightCtx)

	observedAt := snapshot.Inventory.ObservedAt
	if observedAt.IsZero() {
		// A failed inventory probe is still a timestamped observation. Keep the
		// current attempt visible rather than fabricating a successful inventory.
		observedAt = a.now().UTC()
	}
	if observedAt.IsZero() {
		return types.AgentAcceleratorReport{}, fmt.Errorf("NVIDIA observation clock returned zero time")
	}

	topology, err := reportTopology(snapshot.Topology)
	if err != nil {
		return types.AgentAcceleratorReport{}, err
	}
	devices, err := reportDevices(snapshot.Inventory.Devices)
	if err != nil {
		return types.AgentAcceleratorReport{}, err
	}
	capabilities, err := reportCapabilities(snapshot.Capabilities)
	if err != nil {
		return types.AgentAcceleratorReport{}, err
	}

	driverVersion := firstNonBlank(snapshot.Inventory.DriverVersion, a.nvidia.driverVersion)
	if a.nvidia.useObservedDriverVersion {
		driverVersion = snapshot.Inventory.DriverVersion
	}
	runtimeVersion := a.nvidia.runtimeVersion
	runtimeAttested := false
	if a.nvidia.runtimeProber != nil {
		version, probeErr := a.nvidia.runtimeProber.Version(preflightCtx)
		if probeErr != nil {
			snapshot.Reasons = append(snapshot.Reasons, fmt.Sprintf("DCGM runtime version probe failed: %v", probeErr))
		} else {
			gpuCount, discoveryErr := a.nvidia.runtimeProber.GPUCount(preflightCtx)
			if discoveryErr != nil {
				snapshot.Reasons = append(snapshot.Reasons, fmt.Sprintf("DCGM discovery probe failed: %v", discoveryErr))
			} else if gpuCount != len(snapshot.Inventory.Devices) {
				snapshot.Reasons = append(snapshot.Reasons,
					fmt.Sprintf("DCGM discovery found %d GPUs, nvidia-smi inventory found %d", gpuCount, len(snapshot.Inventory.Devices)))
			} else {
				runtimeVersion = version
				runtimeAttested = true
			}
		}
	}
	report := types.AgentAcceleratorReport{
		Node:              a.cfg.NodeName,
		Vendor:            types.AcceleratorVendorNVIDIA,
		ObservedAt:        observedAt,
		Devices:           devices,
		DeviceHolders:     a.deviceHolders(),
		DriverVersion:     driverVersion,
		RuntimeVersion:    runtimeVersion,
		TopologySafety:    topology,
		Capabilities:      capabilities,
		ReadinessReasons:  append([]string(nil), snapshot.Reasons...),
		ProfileDigest:     profile.ProfileDigest,
		ProfileUID:        profile.ProfileUID,
		ProfileGeneration: profile.ProfileGeneration,
	}

	switch snapshot.Readiness {
	case nvidia.PreflightEligible:
		// The wire protocol makes versions a prerequisite for ready. Do not
		// turn a successful health probe into an unverifiable runtime profile.
		if strings.TrimSpace(report.DriverVersion) == "" || strings.TrimSpace(report.RuntimeVersion) == "" || strings.TrimSpace(profile.RuntimeVersion) == "" {
			report.Readiness = types.AcceleratorReadinessDegraded
			report.ReadinessReasons = append(report.ReadinessReasons,
				"NVIDIA runtime profile is missing driver_version or runtime_version")
		} else if !runtimeAttested {
			report.Readiness = types.AcceleratorReadinessDegraded
			report.ReadinessReasons = append(report.ReadinessReasons,
				"NVIDIA runtime is not attested by matching local DCGM version and discovery probes")
		} else if !config.RuntimeVersionSatisfies(report.RuntimeVersion, profile.RuntimeVersion) {
			report.Readiness = types.AcceleratorReadinessDegraded
			// Name the binary that produced the version. When a node carries
			// its own dcgmi, the mismatch is usually "which client answered",
			// not "which engine runs" — and that is invisible otherwise.
			report.ReadinessReasons = append(report.ReadinessReasons, fmt.Sprintf(
				"locally attested NVIDIA runtime version %q from %s does not match the controller profile %q",
				report.RuntimeVersion, a.nvidia.dcgmClientPath(), profile.RuntimeVersion))
		} else if len(report.ReadinessReasons) != 0 {
			return types.AgentAcceleratorReport{}, fmt.Errorf("NVIDIA eligible preflight included readiness reasons")
		} else {
			report.Readiness = types.AcceleratorReadinessReady
		}
	case nvidia.PreflightObservedOnly:
		report.Readiness = types.AcceleratorReadinessDegraded
	case nvidia.PreflightBlocked:
		report.Readiness = types.AcceleratorReadinessNotReady
	default:
		report.Readiness = types.AcceleratorReadinessNotReady
		report.ReadinessReasons = append(report.ReadinessReasons,
			fmt.Sprintf("unrecognized NVIDIA preflight readiness %q", snapshot.Readiness))
	}
	if report.Readiness != types.AcceleratorReadinessReady && len(report.ReadinessReasons) == 0 {
		report.ReadinessReasons = []string{"NVIDIA preflight did not establish a ready runtime"}
	}
	if err := report.Validate(); err != nil {
		return types.AgentAcceleratorReport{}, fmt.Errorf("invalid NVIDIA accelerator report: %w", err)
	}
	return report, nil
}

func reportTopology(topology nvidia.PartitionTopology) (types.AcceleratorTopologySafety, error) {
	switch topology {
	case nvidia.PartitionTopologyUnknown:
		return types.AcceleratorTopologyUnknown, nil
	case nvidia.PartitionTopologyNone:
		return types.AcceleratorTopologyVerifiedUnpartitioned, nil
	case nvidia.PartitionTopologyMIG:
		return types.AcceleratorTopologyPartitioned, nil
	case nvidia.PartitionTopologyOther:
		return types.AcceleratorTopologyUnsafe, nil
	default:
		return "", fmt.Errorf("unsupported NVIDIA partition topology %q", topology)
	}
}

func reportDevices(devices []accelerator.Device) ([]types.AgentAcceleratorDevice, error) {
	reportDevices := make([]types.AgentAcceleratorDevice, 0, len(devices))
	for _, device := range devices {
		kind, err := reportDeviceKind(device.Kind)
		if err != nil {
			return nil, err
		}
		family, err := reportDeviceFamily(device.Family)
		if err != nil {
			return nil, err
		}
		reportDevices = append(reportDevices, types.AgentAcceleratorDevice{
			ID:               device.ID,
			Kind:             kind,
			Family:           family,
			Model:            device.Model,
			ParentID:         device.ParentID,
			PartitionProfile: device.PartitionProfile,
			Attributes:       cloneStringMap(device.Attributes),
		})
	}
	return reportDevices, nil
}

func reportDeviceKind(kind accelerator.DeviceKind) (types.AcceleratorDeviceKind, error) {
	switch kind {
	case accelerator.DevicePhysical:
		return types.AcceleratorDevicePhysical, nil
	case accelerator.DevicePartition:
		return types.AcceleratorDevicePartition, nil
	default:
		return "", fmt.Errorf("unsupported accelerator device kind %q", kind)
	}
}

func reportDeviceFamily(family accelerator.Family) (types.AcceleratorDeviceFamily, error) {
	switch family {
	case accelerator.FamilyGPU:
		return types.AcceleratorFamilyGPU, nil
	case accelerator.FamilyTPU:
		return types.AcceleratorFamilyTPU, nil
	default:
		return "", fmt.Errorf("unsupported accelerator device family %q", family)
	}
}

func reportCapabilities(capabilities accelerator.CapabilitySet) ([]types.AgentAcceleratorCapability, error) {
	reportCapabilities := make([]types.AgentAcceleratorCapability, 0, len(capabilities))
	for _, capability := range capabilities {
		action, err := reportAction(capability.Action)
		if err != nil {
			return nil, err
		}
		scopes := make([]types.AcceleratorTargetScope, 0, len(capability.Scopes))
		for _, scope := range capability.Scopes {
			mappedScope, err := reportScope(scope)
			if err != nil {
				return nil, err
			}
			scopes = append(scopes, mappedScope)
		}
		reportCapabilities = append(reportCapabilities, types.AgentAcceleratorCapability{
			Action: action,
			Scopes: scopes,
		})
	}
	return reportCapabilities, nil
}

func reportAction(action accelerator.Action) (types.AcceleratorAction, error) {
	switch action {
	case accelerator.ActionEvacuateWorkloads:
		return types.AcceleratorActionEvacuateWorkloads, nil
	case accelerator.ActionQuarantineNode:
		return types.AcceleratorActionQuarantineNode, nil
	case accelerator.ActionResetDevice:
		return types.AcceleratorActionResetDevice, nil
	case accelerator.ActionRestartRuntime:
		return types.AcceleratorActionRestartRuntime, nil
	case accelerator.ActionRebootNode:
		return types.AcceleratorActionRebootNode, nil
	case accelerator.ActionCollectDiagnostics:
		return types.AcceleratorActionCollectDiagnostics, nil
	case accelerator.ActionVerifyHealth:
		return types.AcceleratorActionVerifyHealth, nil
	case accelerator.ActionReplaceNode:
		return types.AcceleratorActionReplaceNode, nil
	default:
		return "", fmt.Errorf("unsupported accelerator action %q", action)
	}
}

func reportScope(scope accelerator.TargetScope) (types.AcceleratorTargetScope, error) {
	switch scope {
	case accelerator.ScopeNode:
		return types.AcceleratorScopeNode, nil
	case accelerator.ScopePhysicalDevice:
		return types.AcceleratorScopePhysicalDevice, nil
	case accelerator.ScopePartition:
		return types.AcceleratorScopePartition, nil
	default:
		return "", fmt.Errorf("unsupported accelerator target scope %q", scope)
	}
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// fetchAcceleratorObservationProfile obtains the controller-selected immutable
// profile digest for this authenticated node. A 204 response means no profile
// selects the node and is intentionally not converted into a report with a
// fallback local digest. That keeps an operator-managed DaemonSet default-deny
// during rollout, selector changes, and configuration outages.
func (a *Agent) fetchAcceleratorObservationProfile(ctx context.Context, vendor types.AcceleratorVendor) (types.AgentAcceleratorObservationProfile, bool, error) {
	if !vendor.Valid() {
		return types.AgentAcceleratorObservationProfile{}, false, fmt.Errorf("unsupported accelerator profile vendor %q", vendor)
	}
	path := types.AgentAcceleratorProfilePath + "?vendor=" + url.QueryEscape(string(vendor))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.ControllerURL+path, nil)
	if err != nil {
		return types.AgentAcceleratorObservationProfile{}, false, err
	}
	if err := a.authorize(req); err != nil {
		return types.AgentAcceleratorObservationProfile{}, false, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return types.AgentAcceleratorObservationProfile{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return types.AgentAcceleratorObservationProfile{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return types.AgentAcceleratorObservationProfile{}, false, fmt.Errorf("GET %s: expected %d or %d, got %s", types.AgentAcceleratorProfilePath, http.StatusOK, http.StatusNoContent, resp.Status)
	}
	var profile types.AgentAcceleratorObservationProfile
	dec := json.NewDecoder(io.LimitReader(resp.Body, acceleratorProfileResponseLimit+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&profile); err != nil {
		return types.AgentAcceleratorObservationProfile{}, false, fmt.Errorf("decode accelerator observation profile: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return types.AgentAcceleratorObservationProfile{}, false, fmt.Errorf("decode accelerator observation profile: %w", err)
	}
	if profile.Vendor != vendor {
		return types.AgentAcceleratorObservationProfile{}, false, fmt.Errorf("accelerator observation profile returned vendor %q, want %q", profile.Vendor, vendor)
	}
	if err := profile.Validate(); err != nil {
		return types.AgentAcceleratorObservationProfile{}, false, err
	}
	return profile, true, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected second JSON value")
		}
		return err
	}
	return nil
}

// requireCapability verifies the exact protocol token served on a versioned
// registration path before the agent posts to it. The versioned POST path
// itself keeps a legacy handler from decoding the payload, even when requests
// in a rolling update reach different controller Pods; the exact-match token
// is the mixed-version corruption guard for the GET half.
func (a *Agent) requireCapability(ctx context.Context, path, protocol string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.ControllerURL+path, nil)
	if err != nil {
		return err
	}
	if err := a.authorize(req); err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: expected %d, got %s", path, http.StatusOK, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, registrationCapabilityLimit+1))
	if err != nil {
		return fmt.Errorf("read registration capability: %w", err)
	}
	want := protocol + "\n"
	if string(body) != want {
		return fmt.Errorf("GET %s: incompatible registration protocol capability %q", path, body)
	}
	return nil
}

func (a *Agent) post(ctx context.Context, path string, v any) error {
	return a.postExpectStatus(ctx, path, v, 0)
}

// errEventRejected wraps a controller response that permanently rejects THIS
// payload: no retry can ever succeed, so the sender must drop the event
// (loudly) instead of replaying it forever. Only a response carrying the
// explicit AgentEventRejectedHeader marker qualifies — the controller sets it
// exclusively on semantic verdicts about the payload (e.g. the XID+Fault
// double-identity rejection). A bare 400 is NOT poison: during a rolling
// upgrade an older controller 400s every event from a newer agent (strict
// JSON decoding), and a middlebox can 400/413 an event the controller would
// accept; those must keep spooling and drain once the skew clears. Auth
// statuses (401/403) likewise stay retryable.
var errEventRejected = errors.New("the controller permanently rejected this event")

func (a *Agent) postExpectStatus(ctx context.Context, path string, v any, expectedStatus int) error {
	_, err := a.postExpectStatusHeader(ctx, path, v, expectedStatus, "")
	return err
}

// postExpectStatusHeader is postExpectStatus that additionally returns the
// named response header's value on success ("" when absent or unnamed).
func (a *Agent) postExpectStatusHeader(ctx context.Context, path string, v any, expectedStatus int, header string) (string, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ControllerURL+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := a.authorize(req); err != nil {
		return "", err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if expectedStatus != 0 && resp.StatusCode != expectedStatus {
		return "", fmt.Errorf("POST %s: expected %d, got %s", path, expectedStatus, resp.Status)
	}
	if expectedStatus == 0 && resp.StatusCode >= 300 {
		if resp.StatusCode >= 400 && resp.Header.Get(types.AgentEventRejectedHeader) != "" {
			return "", fmt.Errorf("POST %s: %s (%s): %w", path, resp.Status,
				resp.Header.Get(types.AgentEventRejectedHeader), errEventRejected)
		}
		return "", fmt.Errorf("POST %s: %s", path, resp.Status)
	}
	if header == "" {
		return "", nil
	}
	return resp.Header.Get(header), nil
}

// postActionResult submits a result for one explicitly leased action. The
// token is kept out of the result payload so it cannot be mistaken for an
// auditable action parameter or accidentally persisted in the action output.
func (a *Agent) postActionResult(ctx context.Context, path, leaseToken string, result *types.ActionResult) error {
	if leaseToken == "" {
		return fmt.Errorf("action result has no lease token")
	}
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ControllerURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(types.AgentActionLeaseHeader, leaseToken)
	if bootID := a.currentBootID(); bootID != "" {
		req.Header.Set(types.AgentBootIDHeader, bootID)
	}
	if result.Refusal != "" {
		req.Header.Set(types.AgentActionRefusalHeader, result.Refusal)
	}
	if err := a.authorize(req); err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("POST %s: expected %d, got %s", path, http.StatusNoContent, resp.Status)
	}
	return nil
}

func (a *Agent) authorize(req *http.Request) error {
	token := a.cfg.Token
	if a.cfg.TokenFile != "" {
		file, err := os.Open(a.cfg.TokenFile)
		if err != nil {
			return fmt.Errorf("open controller token file: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxTokenFileBytes+1))
		closeErr := file.Close()
		if readErr != nil {
			return fmt.Errorf("read controller token file: %w", readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close controller token file: %w", closeErr)
		}
		if len(data) > maxTokenFileBytes {
			return fmt.Errorf("controller token file exceeds %d bytes", maxTokenFileBytes)
		}
		token = string(data)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		if a.cfg.AllowInsecureHTTP {
			return nil
		}
		return fmt.Errorf("controller bearer token is empty")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return fmt.Errorf("controller bearer token contains whitespace")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (a *Agent) startHealthServer() (*http.Server, <-chan error, error) {
	listener, err := a.listen("tcp", a.cfg.HealthListenAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("listen for health probes on %s: %w", a.cfg.HealthListenAddress, err)
	}
	server := &http.Server{
		Handler:           a.healthHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	return server, serveErr, nil
}

func (a *Agent) shutdownHealthServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), healthShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		a.log.Warn("health server shutdown failed", "err", err)
	}
}

func (a *Agent) healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "live\n")
	})
	mux.Handle("GET /metrics", metrics.Handler())
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		lastAck, ackSequence, current := a.registrationAcknowledgmentSnapshot()
		if !current {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "no recent durable controller registration acknowledgment\n")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "controller registration acknowledged\n")
		_, _ = fmt.Fprintf(w, "last_ack_unix_nano=%d\n", lastAck.UnixNano())
		_, _ = fmt.Fprintf(w, "ack_sequence=%d\n", ackSequence)
	})
	return mux
}

func (a *Agent) registrationAcknowledgmentSnapshot() (time.Time, uint64, bool) {
	a.registrationMu.Lock()
	defer a.registrationMu.Unlock()
	lastAck := a.lastRegistrationAck
	if lastAck.IsZero() || lastAck.UnixNano() <= 0 || a.registrationAckSeq == 0 {
		return time.Time{}, 0, false
	}
	return lastAck, a.registrationAckSeq, a.now().Sub(lastAck) < a.cfg.RegistrationStaleAfter
}

func (a *Agent) recordRegistrationAcknowledgment() {
	metrics.AgentRegistrationAcks.Inc()
	now := a.now()
	a.registrationMu.Lock()
	initial := a.lastRegistrationAck.IsZero()
	recovered := a.registrationLost
	a.lastRegistrationAck = now
	if a.registrationAckSeq != ^uint64(0) {
		a.registrationAckSeq++
	}
	a.registrationLost = false
	a.registrationMu.Unlock()

	switch {
	case initial:
		a.log.Info("controller registration acknowledged")
	case recovered:
		a.log.Info("controller registration acknowledgment recovered")
	}
}

func (a *Agent) recordRegistrationFailure(registrationErr error) {
	now := a.now()
	a.registrationMu.Lock()
	lastAck := a.lastRegistrationAck
	if lastAck.IsZero() {
		// Never acknowledged: an agent that boots into a persistent failure
		// (a rejected client certificate, a wrong controller URL) must not
		// stay silent at the default log level forever — the boot-time
		// "initial registration failed" line may have caught a transient
		// error, not the persistent one. Measure staleness from the first
		// failed attempt and report ONCE, with the current error.
		if a.firstRegistrationTry.IsZero() {
			a.firstRegistrationTry = now
		}
		if a.registrationLost || now.Sub(a.firstRegistrationTry) < a.cfg.RegistrationStaleAfter {
			a.registrationMu.Unlock()
			return
		}
		a.registrationLost = true
		a.registrationMu.Unlock()

		a.log.Warn(
			"controller registration never acknowledged",
			"since", a.firstRegistrationTry,
			"stale_after", a.cfg.RegistrationStaleAfter,
			"err", registrationErr,
		)
		return
	}
	if a.registrationLost || now.Sub(lastAck) < a.cfg.RegistrationStaleAfter {
		a.registrationMu.Unlock()
		return
	}
	a.registrationLost = true
	a.registrationMu.Unlock()

	a.log.Warn(
		"controller registration acknowledgment lost",
		"last_ack", lastAck,
		"stale_after", a.cfg.RegistrationStaleAfter,
		"err", registrationErr,
	)
}

// dcgmClientPath names the DCGM client that answered the attestation probe.
// The default is the client shipped in the agent image; an operator can point
// the agent at one installed on the node, and which one answered is exactly
// what an unexpected version mismatch needs to disclose.
func (o nvidiaObservation) dcgmClientPath() string {
	if path := strings.TrimSpace(o.dcgmPath); path != "" {
		return path
	}
	return "dcgmi from PATH"
}

// deviceHolders reports the processes holding a GPU device node right now.
//
// This rides the ordinary accelerator report rather than waiting for a reset,
// so the controller can refuse a doomed reset playbook before it cordons and
// drains anything. A driver that cannot answer, or a process table that cannot
// be read, reports nil rather than an empty list: "we did not look" and
// "nothing holds the device" must never be indistinguishable to a gate.
func (a *Agent) deviceHolders() []types.AgentDeviceHolder {
	lister, ok := a.driver.(interface {
		DeviceHolders(index int) ([]nvml.DeviceHolder, error)
	})
	if !ok {
		return nil
	}
	holders, err := lister.DeviceHolders(-1)
	if err != nil {
		return nil
	}
	out := make([]types.AgentDeviceHolder, 0, len(holders))
	for _, h := range holders {
		out = append(out, types.AgentDeviceHolder{PID: h.PID, Command: h.Command, Device: h.Device})
	}
	return out
}
