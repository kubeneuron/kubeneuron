// Package agent wires the kubeneuron-agent: the kmsg XID watcher, the NVML
// driver, the local action executor, and the event push loop toward the
// controller (with a file-backed spool for outages).
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/accelerator"
	"github.com/kubeneuron/kubeneuron/internal/accelerator/nvidia"
	"github.com/kubeneuron/kubeneuron/internal/agent/actionjournal"
	"github.com/kubeneuron/kubeneuron/internal/agent/dcgm"
	"github.com/kubeneuron/kubeneuron/internal/agent/executor"
	"github.com/kubeneuron/kubeneuron/internal/agent/kmsg"
	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/internal/agent/spool"
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
	DCGMPath             string
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
	// EnableDestructiveActions arms reset/reboot/driver/script execution on
	// this node. It requires the real nvidia-smi driver and is never set by
	// the operator until the Enabled admission gate ships; every action
	// still passes the controller's safety, capability, and approval gates.
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
}

// Agent is the per-node daemon.
type Agent struct {
	cfg      Config
	driver   nvml.GPUDriver
	executor *executor.Executor
	spool    *spool.Spool
	journal  *actionjournal.Journal
	// journalLock remains held for the Agent lifetime. It ensures two local
	// processes cannot concurrently act on the same durable journal.
	journalLock *os.File
	watcher     *kmsg.Watcher
	client      *http.Client
	log         *slog.Logger
	nvidia      *nvidiaObservation

	bootIDOnce sync.Once
	bootID     string

	registrationMu      sync.Mutex
	lastRegistrationAck time.Time
	registrationAckSeq  uint64
	registrationLost    bool
	now                 func() time.Time
	listen              func(network, address string) (net.Listener, error)
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
	sp, err := spool.Open(cfg.SpoolPath)
	if err != nil {
		return nil, fmt.Errorf("opening spool: %w", err)
	}
	journalLock, err := acquireActionJournalLock(cfg.ActionJournalPath)
	if err != nil {
		return nil, fmt.Errorf("locking action journal: %w", err)
	}
	journal, err := actionjournal.Open(cfg.ActionJournalPath)
	if err != nil {
		_ = releaseActionJournalLock(journalLock)
		return nil, fmt.Errorf("opening action journal: %w", err)
	}
	exec := executor.NewWithOptions(driver, executor.Options{
		EnableDestructiveActions: cfg.EnableDestructiveActions,
	})
	if cfg.ScriptsDir != "" {
		exec.ScriptsDir = cfg.ScriptsDir
	}
	agent := &Agent{
		cfg:         cfg,
		driver:      driver,
		executor:    exec,
		spool:       sp,
		journal:     journal,
		journalLock: journalLock,
		watcher:     kmsg.NewWatcher(),
		client:      client,
		log:         log,
		now:         time.Now,
		listen:      net.Listen,
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
			runtimeProber:            dcgm.New(cfg.NVIDIAObservation.DCGMPath),
			driverVersion:            cfg.NVIDIAObservation.DriverVersion,
			runtimeVersion:           cfg.NVIDIAObservation.RuntimeVersion,
			profileDigest:            cfg.NVIDIAObservation.ProfileDigest,
			useControllerProfile:     cfg.NVIDIAObservation.UseControllerProfile,
			useObservedDriverVersion: true,
		}
	}
	return agent, nil
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
			a.handleXID(ctx, ev)
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

// handleXID maps a kernel XID event to a GPU and pushes it to the controller.
func (a *Agent) handleXID(ctx context.Context, ev kmsg.XIDEvent) {
	gpu, err := a.driver.GPUByPCIAddr(ctx, ev.PCIAddr)
	if err != nil {
		a.log.Warn("cannot resolve GPU for XID", "pci", ev.PCIAddr, "xid", ev.XID, "err", err)
		// An unattributed event must say so: index 0 is a real GPU, and
		// evidence blaming it for another device's XID misleads operators.
		gpu.Index = -1
		gpu.UUID = ""
	}
	agentEv := types.AgentEvent{
		EventID:   newEventID(),
		Node:      a.cfg.NodeName,
		GPUIndex:  gpu.Index,
		GPUUUID:   gpu.UUID,
		XID:       ev.XID,
		Raw:       ev.Raw,
		Timestamp: ev.Timestamp,
	}
	if err := a.post(ctx, "/api/v1/events", agentEv); err != nil {
		a.log.Warn("event push failed, spooling", "xid", ev.XID, "err", err)
		if err := a.spool.Append(agentEv); err != nil {
			a.log.Error("spool append failed", "err", err)
		} else {
			metrics.AgentEventsSpooled.Inc()
		}
	} else {
		metrics.AgentEventsPosted.Inc()
	}
	metrics.AgentSpoolDepth.Set(float64(a.spool.Len()))
}

// flushSpool drains the spool in batches until it is empty, a send fails, or
// the per-tick time budget runs out — so an outage backlog clears in minutes
// while the registration heartbeat is never starved.
func (a *Agent) flushSpool(ctx context.Context) {
	flushCtx, cancel := context.WithTimeout(ctx, spoolFlushBudget)
	defer cancel()
	for {
		sent, err := a.spool.ReplayBatch(flushCtx, spoolReplayBatchSize, func(ctx context.Context, event types.AgentEvent) error {
			return a.post(ctx, "/api/v1/events", event)
		})
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
				a.log.Warn("spool replay stopped", "err", err, "remaining", a.spool.Len())
			}
			return
		}
		if sent > 0 {
			metrics.AgentEventsPosted.Add(float64(sent))
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
	if err := a.requireRegistrationCapability(ctx); err != nil {
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
		Name:   a.cfg.NodeName,
		GPUs:   gpus,
		BootID: string(bytes.TrimSpace(bootID)),
	}
	if err := a.postExpectStatus(ctx, types.AgentRegistrationPath, registration, http.StatusNoContent); err != nil {
		a.recordRegistrationFailure(err)
		return err
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
		} else if report.RuntimeVersion != profile.RuntimeVersion {
			report.Readiness = types.AcceleratorReadinessDegraded
			report.ReadinessReasons = append(report.ReadinessReasons,
				"locally attested NVIDIA runtime version does not match the controller profile")
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

// requireRegistrationCapability verifies the exact protocol served on the
// versioned narrow-registration path. The versioned POST path itself keeps a
// legacy full-Node handler from decoding this payload, even when requests in a
// rolling update reach different controller Pods.
func (a *Agent) requireRegistrationCapability(ctx context.Context) error {
	const path = types.AgentRegistrationPath
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
	want := types.AgentRegistrationProtocol + "\n"
	if string(body) != want {
		return fmt.Errorf("GET %s: incompatible registration protocol capability %q", path, body)
	}
	return nil
}

func (a *Agent) post(ctx context.Context, path string, v any) error {
	return a.postExpectStatus(ctx, path, v, 0)
}

func (a *Agent) postExpectStatus(ctx context.Context, path string, v any, expectedStatus int) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ControllerURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := a.authorize(req); err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if expectedStatus != 0 && resp.StatusCode != expectedStatus {
		return fmt.Errorf("POST %s: expected %d, got %s", path, expectedStatus, resp.Status)
	}
	if expectedStatus == 0 && resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: %s", path, resp.Status)
	}
	return nil
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
	if lastAck.IsZero() || a.registrationLost || now.Sub(lastAck) < a.cfg.RegistrationStaleAfter {
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
