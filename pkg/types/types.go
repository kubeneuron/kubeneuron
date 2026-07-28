// Package types contains the wire and domain types shared between the
// controller, the agent, the CLI, and external integrations.
package types

import (
	"fmt"
	"strings"
	"time"
)

const (
	// AgentRegistrationPath is versioned so a new narrow registration POST can
	// never be decoded by the legacy full-Node registration handler, including
	// when separate requests reach different Pods during a rolling update.
	AgentRegistrationPath = "/api/v1/agents/register/narrow-v1"
	// AgentRegistrationProtocol is the exact capability token an agent requires
	// before it sends the narrow registration payload.
	AgentRegistrationProtocol = "kubeneuron-agent-registration/v1"
	// AgentActionLeasePath is the versioned agent action-delivery route. An
	// agent only executes work claimed through this lease-bearing protocol;
	// old agents therefore fail closed during a rolling upgrade instead of
	// completing a reclaimed action without a lease token.
	AgentActionLeasePath = "/api/v1/agents/actions/lease-v1"
	// AgentActionLeaseHeader carries the opaque delivery lease between the
	// controller and the authenticated agent. It is deliberately an HTTP
	// header, not action configuration, so it cannot be mistaken for a
	// playbook parameter or rendered in normal action/audit output.
	AgentActionLeaseHeader = "X-KubeNeuron-Action-Lease"
	// AgentActionLeaseExpiresHeader carries the controller's RFC3339Nano lease
	// expiry. The agent persists it with the opaque token and only resumes a
	// crash-interrupted action while that exact lease remains current.
	AgentActionLeaseExpiresHeader = "X-KubeNeuron-Action-Lease-Expires-At"
	// AgentAcceleratorReportPath is the versioned, observation-only inventory
	// and runtime-preflight report sent by an authenticated node agent. A
	// controller that has not wired this exact protocol must not accept it.
	AgentAcceleratorReportPath = "/api/v1/agents/accelerators/report-v1"
	// AgentAcceleratorProfilePath is the authenticated, controller-owned
	// profile lookup used by an agent before it emits an observation bound to
	// an AcceleratorRuntimeProfile. The node identity comes from the Pod-bound
	// request, never from a query parameter, so an agent cannot ask for another
	// node's selected profile.
	AgentAcceleratorProfilePath = "/api/v1/agents/accelerators/profile-v1"
	// AgentBootIDHeader carries the agent's current node boot identity on
	// action claims and results. The server binds each claim to the boot
	// that made it and rejects a result from a different boot: a reboot
	// mid-execution makes the outcome unknown, not complete.
	AgentBootIDHeader = "X-KubeNeuron-Executor-Boot-Id"
)

// Target identifies what an incident or signal is about: a whole node, or a
// specific GPU on a node. GPUUUID is empty for node-scoped targets.
type Target struct {
	Node     string `json:"node"`
	GPUUUID  string `json:"gpu_uuid,omitempty"`
	GPUIndex int    `json:"gpu_index,omitempty"`
}

// IsGPU reports whether the target is a single GPU rather than a whole node.
func (t Target) IsGPU() bool { return t.GPUUUID != "" }

// ProblemClass is a normalized failure category. Signals from different
// sources (XID events, vmalert alerts) map into the same class so the policy
// engine has a single vocabulary.
type ProblemClass string

const (
	ClassXIDApp          ProblemClass = "xid-app"           // XID 13/31/43/46: usually workload bugs
	ClassECCDBE          ProblemClass = "ecc-dbe"           // XID 48/95, volatile DBE counters
	ClassECCSBERate      ProblemClass = "ecc-sbe-rate"      // XID 92: high correctable-error rate
	ClassECCContained    ProblemClass = "ecc-contained"     // XID 94: contained ECC error
	ClassRowRemapOK      ProblemClass = "row-remap-ok"      // XID 63: remap recorded, reset pending
	ClassRowRemapFailure ProblemClass = "row-remap-failure" // XID 64 or DCGM remap failure
	ClassRowRemapBudget  ProblemClass = "row-remap-budget"  // remapped rows near exhaustion
	ClassNVLink          ProblemClass = "nvlink"            // XID 74, NVLink CRC errors
	ClassFellOffBus      ProblemClass = "fell-off-bus"      // XID 79
	ClassGSPError        ProblemClass = "gsp-error"         // XID 119/120
	ClassThermal         ProblemClass = "thermal"           // thermal throttle / critical temp
	ClassPower           ProblemClass = "power"             // power brake / PSU issues
	ClassPCIe            ProblemClass = "pcie"              // PCIe replay storms
	ClassDriverHang      ProblemClass = "driver-hang"       // nvidia-smi timeout, exporter dead
	ClassGPULost         ProblemClass = "gpu-lost"          // GPU count below inventory
	ClassDiagFailure     ProblemClass = "diag-failure"      // dcgmi diag failed
	ClassAgentDown       ProblemClass = "agent-down"        // kubeneuron-agent heartbeat stale
)

// Severity of a signal.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// SignalSource identifies which pipeline produced a signal.
type SignalSource string

const (
	SourceAgentEvent   SignalSource = "agent-event"  // fast path: agent kmsg/NVML watcher
	SourceAlertmanager SignalSource = "alertmanager" // slow path: vmalert -> Alertmanager webhook
	SourceManual       SignalSource = "manual"       // operator-triggered via API/CLI
)

// Signal is a normalized observation about a target. Both Alertmanager
// alerts and agent events are converted into Signals before they reach the
// incident state machine.
type Signal struct {
	Target     Target            `json:"target"`
	Class      ProblemClass      `json:"class"`
	Severity   Severity          `json:"severity"`
	Source     SignalSource      `json:"source"`
	Evidence   map[string]string `json:"evidence,omitempty"` // e.g. xid, raw kmsg line, alert labels
	ObservedAt time.Time         `json:"observed_at"`
}

// IncidentState is a state of the per-target incident state machine.
type IncidentState string

const (
	StateOpen             IncidentState = "OPEN"
	StateObserving        IncidentState = "OBSERVING"
	StateEvaluating       IncidentState = "EVALUATING"
	StateAwaitingApproval IncidentState = "AWAITING_APPROVAL"
	StateExecuting        IncidentState = "EXECUTING"
	StateVerifying        IncidentState = "VERIFYING"
	StateNeedsHuman       IncidentState = "NEEDS_HUMAN"
	StateResolved         IncidentState = "RESOLVED"
	StateExpired          IncidentState = "EXPIRED"
)

// Terminal reports whether the state ends the incident lifecycle.
func (s IncidentState) Terminal() bool {
	return s == StateResolved || s == StateExpired
}

// Incident is one open problem on one target. At most one non-terminal
// incident exists per (Target, Class); correlated signals attach to it.
type Incident struct {
	ID         string        `json:"id"`
	Target     Target        `json:"target"`
	Class      ProblemClass  `json:"class"`
	State      IncidentState `json:"state"`
	Playbook   string        `json:"playbook,omitempty"`
	StepIndex  int           `json:"step_index"`
	Attempt    int           `json:"attempt"`
	DryRun     bool          `json:"dry_run"`
	SignalSeen int           `json:"signals_seen"`
	OpenedAt   time.Time     `json:"opened_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
	// StateChangedAt is when State last changed. Unlike UpdatedAt it is not
	// bumped by attaching duplicate signals, so timeouts that anchor to a
	// state (approval TTL, verification quiet windows) cannot be postponed
	// by a signal storm.
	StateChangedAt time.Time  `json:"state_changed_at"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
}

// Clone returns a deep copy. Use it whenever an incident crosses a goroutine
// boundary (for example async notification queues) so the reconcile loop can
// keep mutating its copy without racing the reader.
func (i *Incident) Clone() *Incident {
	if i == nil {
		return nil
	}
	c := *i
	if i.ResolvedAt != nil {
		t := *i.ResolvedAt
		c.ResolvedAt = &t
	}
	return &c
}

// AuditEntry records one state transition or action of an incident. The
// audit log is append-only.
type AuditEntry struct {
	ID         int64             `json:"id"`
	IncidentID string            `json:"incident_id"`
	Time       time.Time         `json:"time"`
	FromState  IncidentState     `json:"from_state"`
	ToState    IncidentState     `json:"to_state"`
	Actor      string            `json:"actor"` // "system" or a user identity
	Action     string            `json:"action,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
	Result     string            `json:"result,omitempty"`
	DryRun     bool              `json:"dry_run"`
}

// ActionType enumerates everything an Actuator can be asked to do.
type ActionType string

const (
	ActionGPUReset        ActionType = "gpu_reset"
	ActionIdleCheck       ActionType = "idle_check"
	ActionWaitIdle        ActionType = "wait_idle"
	ActionRunDiag         ActionType = "run_diag"
	ActionCollectBundle   ActionType = "collect_bundle"
	ActionReboot          ActionType = "reboot"
	ActionDriverReload    ActionType = "driver_reload"
	ActionDriverReinstall ActionType = "driver_reinstall"
	ActionRunScript       ActionType = "run_script"
	ActionPowerCycle      ActionType = "power_cycle"
)

// Action is a single unit of work executed on a node. ID is deterministic —
// hash(incident, step, attempt) — so replays after a controller restart are
// idempotent: executors keep a short-lived cache of completed action IDs.
type Action struct {
	ID      string            `json:"id"`
	Type    ActionType        `json:"type"`
	Params  map[string]string `json:"params,omitempty"`
	Timeout time.Duration     `json:"timeout,omitempty"`
}

// QueuedAction is an Action dispatched to a node through the durable work
// queue: the controller enqueues, the node's agent polls it over the
// authenticated channel, executes, and posts the result back.
type QueuedAction struct {
	Node       string        `json:"node"`
	IncidentID string        `json:"incident_id,omitempty"`
	Action     Action        `json:"action"`
	Done       bool          `json:"done"`
	Result     *ActionResult `json:"result,omitempty"`
	// LeaseToken and LeaseExpiresAt are populated only by a claimed action.
	// The token is opaque and must accompany the action result; it prevents a
	// stale agent from completing work after the lease was reclaimed.
	LeaseToken     string    `json:"lease_token,omitempty"`
	LeaseExpiresAt time.Time `json:"lease_expires_at,omitempty"`
	// Attempts counts issued leases: crash/failover replays attach to the
	// same action and surface here instead of dispatching twice silently.
	Attempts int `json:"attempts,omitempty"`
	// ExecutorBootID is the node boot that claimed the action; a result from
	// a different boot is rejected as completion evidence.
	ExecutorBootID string `json:"executor_boot_id,omitempty"`
	// Cancelled marks undelivered work tombstoned by the controller.
	Cancelled bool `json:"cancelled,omitempty"`
}

// ActionResult is the outcome of executing an Action.
type ActionResult struct {
	ActionID   string    `json:"action_id"`
	OK         bool      `json:"ok"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// GPUInfo describes a single GPU on a node.
type GPUInfo struct {
	Index int    `json:"index"`
	UUID  string `json:"uuid"`
	Model string `json:"model,omitempty"`
}

// Node is an inventory entry: everything the controller knows about a node.
type Node struct {
	Name string `json:"name"`
	// UID is the immutable platform node identity. Kubernetes reports and
	// acceleration gates bind to it so a deleted/recreated node with the same
	// name cannot inherit old runtime evidence.
	UID      string            `json:"uid,omitempty"`
	Platform string            `json:"platform"` // "kubernetes" | "baremetal" | ...
	Labels   map[string]string `json:"labels,omitempty"`
	// SSHAddr and BMCAddr are actuation fallbacks for bare-metal nodes when
	// the agent is unreachable. Empty on Kubernetes nodes.
	SSHAddr string    `json:"ssh_addr,omitempty"`
	BMCAddr string    `json:"bmc_addr,omitempty"`
	GPUs    []GPUInfo `json:"gpus,omitempty"`
	BootID  string    `json:"boot_id,omitempty"` // guards reboot idempotency
	Paused  bool      `json:"paused,omitempty"`  // maintenance: no auto-actions
	// AgentLastSeen is the last heartbeat/registration from kubeneuron-agent.
	AgentLastSeen time.Time `json:"agent_last_seen,omitempty"`
}

// AgentRegistration is the inventory data an agent is allowed to own. Fields
// such as platform, labels, actuation addresses, and pause state are managed by
// the controller and must not be writable through the registration endpoint.
type AgentRegistration struct {
	Name   string    `json:"name"`
	GPUs   []GPUInfo `json:"gpus,omitempty"`
	BootID string    `json:"boot_id,omitempty"`
	// NodeUID is installed only by the authenticated HTTP handler. It is
	// intentionally excluded from JSON so an agent cannot self-assert an
	// immutable platform identity in a registration body.
	NodeUID string `json:"-"`
}

// AgentEvent is what kubeneuron-agent pushes to the controller on the fast
// path (POST /api/v1/events).
type AgentEvent struct {
	// EventID is assigned once when the agent captures the event. Spool
	// replay is at-least-once; the controller deduplicates on this ID so a
	// resend after a lost acknowledgment cannot double-count a signal.
	EventID   string    `json:"event_id,omitempty"`
	Node      string    `json:"node"`
	GPUIndex  int       `json:"gpu_index"`
	GPUUUID   string    `json:"gpu_uuid,omitempty"`
	XID       int       `json:"xid"`
	Raw       string    `json:"raw,omitempty"` // original kmsg line
	Timestamp time.Time `json:"timestamp"`
}

// AcceleratorVendor identifies the runtime that owns the reported
// accelerators. The first protocol supports the vendors represented by the
// common accelerator contract. A new vendor requires an explicit protocol
// change rather than being silently treated as executable.
type AcceleratorVendor string

const (
	AcceleratorVendorNVIDIA AcceleratorVendor = "nvidia"
	AcceleratorVendorAMD    AcceleratorVendor = "amd"
	AcceleratorVendorIntel  AcceleratorVendor = "intel"
	AcceleratorVendorGoogle AcceleratorVendor = "google"
)

// Valid reports whether v is supported by this version of the agent report
// protocol.
func (v AcceleratorVendor) Valid() bool {
	switch v {
	case AcceleratorVendorNVIDIA, AcceleratorVendorAMD, AcceleratorVendorIntel, AcceleratorVendorGoogle:
		return true
	default:
		return false
	}
}

// AcceleratorDeviceFamily is the broad device architecture. A physical or
// partitioned GPU and a TPU use the same report envelope, but their topology
// is never inferred from an ordinal.
type AcceleratorDeviceFamily string

const (
	AcceleratorFamilyGPU AcceleratorDeviceFamily = "gpu"
	AcceleratorFamilyTPU AcceleratorDeviceFamily = "tpu"
)

func (f AcceleratorDeviceFamily) valid() bool {
	return f == AcceleratorFamilyGPU || f == AcceleratorFamilyTPU
}

// AcceleratorDeviceKind distinguishes an independently recoverable physical
// device from a logical partition. A partition must not inherit its parent's
// remediation capabilities.
type AcceleratorDeviceKind string

const (
	AcceleratorDevicePhysical  AcceleratorDeviceKind = "physical"
	AcceleratorDevicePartition AcceleratorDeviceKind = "partition"
)

func (k AcceleratorDeviceKind) valid() bool {
	return k == AcceleratorDevicePhysical || k == AcceleratorDevicePartition
}

// AgentAcceleratorDevice is one stable device identity from an agent-owned
// vendor runtime. ID is a UUID or provider identity, never a transient
// ordinal. A partition references a physical device in the same report.
type AgentAcceleratorDevice struct {
	ID               string                  `json:"id"`
	Kind             AcceleratorDeviceKind   `json:"kind"`
	Family           AcceleratorDeviceFamily `json:"family"`
	Model            string                  `json:"model,omitempty"`
	ParentID         string                  `json:"parent_id,omitempty"`
	PartitionProfile string                  `json:"partition_profile,omitempty"`
	Attributes       map[string]string       `json:"attributes,omitempty"`
}

// AcceleratorTopologySafety is the agent's explicit statement about whether
// it completely understands partitioning. Unknown and partitioned topologies
// are safe observations, but they are not permission to reset a physical
// device. Only verified-unpartitioned can support that declaration.
type AcceleratorTopologySafety string

const (
	AcceleratorTopologyUnknown               AcceleratorTopologySafety = "unknown"
	AcceleratorTopologyVerifiedUnpartitioned AcceleratorTopologySafety = "verified-unpartitioned"
	AcceleratorTopologyPartitioned           AcceleratorTopologySafety = "partitioned"
	AcceleratorTopologyUnsafe                AcceleratorTopologySafety = "unsafe"
)

func (s AcceleratorTopologySafety) valid() bool {
	switch s {
	case AcceleratorTopologyUnknown, AcceleratorTopologyVerifiedUnpartitioned,
		AcceleratorTopologyPartitioned, AcceleratorTopologyUnsafe:
		return true
	default:
		return false
	}
}

// AcceleratorAction is a vendor-neutral semantic action. It is a capability
// declaration only: it never authorizes execution and does not bypass the
// controller's Enabled-mode gates.
type AcceleratorAction string

const (
	AcceleratorActionEvacuateWorkloads  AcceleratorAction = "evacuate-workloads"
	AcceleratorActionQuarantineNode     AcceleratorAction = "quarantine-node"
	AcceleratorActionResetDevice        AcceleratorAction = "reset-device"
	AcceleratorActionRestartRuntime     AcceleratorAction = "restart-runtime"
	AcceleratorActionRebootNode         AcceleratorAction = "reboot-node"
	AcceleratorActionCollectDiagnostics AcceleratorAction = "collect-diagnostics"
	AcceleratorActionVerifyHealth       AcceleratorAction = "verify-health"
	AcceleratorActionReplaceNode        AcceleratorAction = "replace-node"
)

func (a AcceleratorAction) valid() bool {
	switch a {
	case AcceleratorActionEvacuateWorkloads, AcceleratorActionQuarantineNode,
		AcceleratorActionResetDevice, AcceleratorActionRestartRuntime,
		AcceleratorActionRebootNode, AcceleratorActionCollectDiagnostics,
		AcceleratorActionVerifyHealth, AcceleratorActionReplaceNode:
		return true
	default:
		return false
	}
}

// AcceleratorTargetScope is the target scope for a semantic capability.
type AcceleratorTargetScope string

const (
	AcceleratorScopeNode           AcceleratorTargetScope = "node"
	AcceleratorScopePhysicalDevice AcceleratorTargetScope = "physical-device"
	AcceleratorScopePartition      AcceleratorTargetScope = "partition"
)

func (s AcceleratorTargetScope) valid() bool {
	return s == AcceleratorScopeNode || s == AcceleratorScopePhysicalDevice || s == AcceleratorScopePartition
}

// AgentAcceleratorCapability declares the target scopes for one semantic
// action that the local runtime has preflighted. The server validates it but
// merely persists it as observation in this protocol version.
type AgentAcceleratorCapability struct {
	Action AcceleratorAction        `json:"action"`
	Scopes []AcceleratorTargetScope `json:"scopes"`
}

// AcceleratorReadiness is the preflight result for the reported runtime. A
// missing or unknown value is invalid rather than implicitly ready.
type AcceleratorReadiness string

const (
	AcceleratorReadinessReady    AcceleratorReadiness = "ready"
	AcceleratorReadinessDegraded AcceleratorReadiness = "degraded"
	AcceleratorReadinessNotReady AcceleratorReadiness = "not-ready"
)

func (r AcceleratorReadiness) valid() bool {
	return r == AcceleratorReadinessReady || r == AcceleratorReadinessDegraded || r == AcceleratorReadinessNotReady
}

// AgentAcceleratorReport is the vendor-neutral, agent-owned runtime report
// for one vendor on one node. It intentionally carries no execution request.
// All topology and capability data is declarative and is later subject to
// controller policy, configured execution mode, and verification gates.
type AgentAcceleratorReport struct {
	Node string `json:"node"`
	// NodeUID is server-stamped from the Pod-bound agent identity. Agents send
	// no value; persistence and controller gates use it to reject evidence from
	// a prior Kubernetes Node object that reused the same name.
	NodeUID          string                       `json:"node_uid,omitempty"`
	Vendor           AcceleratorVendor            `json:"vendor"`
	ObservedAt       time.Time                    `json:"observed_at"`
	Devices          []AgentAcceleratorDevice     `json:"devices"`
	DriverVersion    string                       `json:"driver_version,omitempty"`
	RuntimeVersion   string                       `json:"runtime_version,omitempty"`
	TopologySafety   AcceleratorTopologySafety    `json:"topology_safety"`
	Capabilities     []AgentAcceleratorCapability `json:"capabilities"`
	Readiness        AcceleratorReadiness         `json:"readiness"`
	ReadinessReasons []string                     `json:"readiness_reasons,omitempty"`
	ProfileDigest    string                       `json:"profile_digest,omitempty"`
	// ProfileUID and ProfileGeneration identify the exact Kubernetes
	// AcceleratorRuntimeProfile acknowledged by this observation. They are
	// empty only for unmanaged, observation-only reports; a controller profile
	// gate requires both fields to match its current profile.
	ProfileUID        string `json:"profile_uid,omitempty"`
	ProfileGeneration int64  `json:"profile_generation,omitempty"`
}

// AgentAcceleratorObservationProfile is the deliberately narrow controller
// response used to bind an agent observation to its selected runtime profile.
// It contains no action policy or execution setting: the controller remains
// the sole authority that resolves and evaluates those later.
type AgentAcceleratorObservationProfile struct {
	Vendor            AcceleratorVendor `json:"vendor"`
	ProfileDigest     string            `json:"profile_digest"`
	ProfileUID        string            `json:"profile_uid"`
	ProfileGeneration int64             `json:"profile_generation"`
	RuntimeVersion    string            `json:"runtime_version"`
}

// Validate rejects incomplete profile binding responses. The controller has
// already validated the full runtime profile; this wire validation prevents a
// malformed response from being silently copied into an agent report.
func (p AgentAcceleratorObservationProfile) Validate() error {
	if !p.Vendor.Valid() {
		return fmt.Errorf("accelerator observation profile: unsupported vendor %q", p.Vendor)
	}
	if !validSHA256ProfileDigest(p.ProfileDigest) {
		return fmt.Errorf("accelerator observation profile: profile digest must be a lowercase sha256 digest")
	}
	if strings.TrimSpace(p.ProfileUID) == "" || strings.TrimSpace(p.ProfileUID) != p.ProfileUID {
		return fmt.Errorf("accelerator observation profile: profile UID is required")
	}
	if p.ProfileGeneration <= 0 {
		return fmt.Errorf("accelerator observation profile: profile generation must be positive")
	}
	if strings.TrimSpace(p.RuntimeVersion) == "" || strings.TrimSpace(p.RuntimeVersion) != p.RuntimeVersion {
		return fmt.Errorf("accelerator observation profile: runtime version is required")
	}
	return nil
}

func validSHA256ProfileDigest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+64 {
		return false
	}
	for _, char := range value[len(prefix):] {
		if (char < 'a' || char > 'f') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

// Validate checks report identity, topology, capability, and readiness
// invariants. It intentionally rejects ambiguous input rather than guessing
// that a declaration is safe. Empty device and capability lists are valid
// observations, because a not-ready runtime may be unable to enumerate them.
func (r AgentAcceleratorReport) Validate() error {
	if strings.TrimSpace(r.Node) == "" {
		return fmt.Errorf("accelerator report: node is required")
	}
	if strings.TrimSpace(r.Node) != r.Node {
		return fmt.Errorf("accelerator report: node must not have leading or trailing whitespace")
	}
	if r.NodeUID != "" && (strings.TrimSpace(r.NodeUID) == "" || strings.TrimSpace(r.NodeUID) != r.NodeUID) {
		return fmt.Errorf("accelerator report: node UID must not have leading or trailing whitespace")
	}
	if !r.Vendor.Valid() {
		return fmt.Errorf("accelerator report: unsupported vendor %q", r.Vendor)
	}
	if r.ObservedAt.IsZero() {
		return fmt.Errorf("accelerator report: observed_at is required")
	}
	if r.ProfileUID != "" || r.ProfileGeneration != 0 {
		if strings.TrimSpace(r.ProfileUID) == "" || strings.TrimSpace(r.ProfileUID) != r.ProfileUID {
			return fmt.Errorf("accelerator report: profile UID is required when profile generation is set")
		}
		if r.ProfileGeneration <= 0 {
			return fmt.Errorf("accelerator report: profile generation must be positive when profile UID is set")
		}
	}
	if !r.TopologySafety.valid() {
		return fmt.Errorf("accelerator report: invalid topology safety %q", r.TopologySafety)
	}
	if !r.Readiness.valid() {
		return fmt.Errorf("accelerator report: invalid readiness %q", r.Readiness)
	}
	if r.Readiness == AcceleratorReadinessReady {
		if len(r.ReadinessReasons) != 0 {
			return fmt.Errorf("accelerator report: ready report cannot include readiness reasons")
		}
		if strings.TrimSpace(r.DriverVersion) == "" || strings.TrimSpace(r.RuntimeVersion) == "" {
			return fmt.Errorf("accelerator report: ready report requires driver and runtime versions")
		}
	} else if len(r.ReadinessReasons) == 0 {
		return fmt.Errorf("accelerator report: non-ready report requires readiness reasons")
	}
	for _, reason := range r.ReadinessReasons {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("accelerator report: readiness reasons must not be empty")
		}
	}

	devices := make(map[string]AgentAcceleratorDevice, len(r.Devices))
	for _, device := range r.Devices {
		if strings.TrimSpace(device.ID) == "" {
			return fmt.Errorf("accelerator report: device ID is required")
		}
		if _, duplicate := devices[device.ID]; duplicate {
			return fmt.Errorf("accelerator report: duplicate device ID %q", device.ID)
		}
		if !device.Kind.valid() {
			return fmt.Errorf("accelerator report: device %q has invalid kind %q", device.ID, device.Kind)
		}
		if !device.Family.valid() {
			return fmt.Errorf("accelerator report: device %q has invalid family %q", device.ID, device.Family)
		}
		if device.Kind == AcceleratorDevicePhysical && device.ParentID != "" {
			return fmt.Errorf("accelerator report: physical device %q cannot have a parent", device.ID)
		}
		if device.Kind == AcceleratorDevicePartition && strings.TrimSpace(device.ParentID) == "" {
			return fmt.Errorf("accelerator report: partition %q requires a physical parent", device.ID)
		}
		devices[device.ID] = device
	}
	for _, device := range r.Devices {
		if device.Kind != AcceleratorDevicePartition {
			continue
		}
		parent, found := devices[device.ParentID]
		if !found || parent.Kind != AcceleratorDevicePhysical || parent.Family != device.Family {
			return fmt.Errorf("accelerator report: partition %q has an invalid physical parent %q", device.ID, device.ParentID)
		}
	}
	if r.TopologySafety == AcceleratorTopologyVerifiedUnpartitioned {
		for _, device := range r.Devices {
			if device.Kind == AcceleratorDevicePartition {
				return fmt.Errorf("accelerator report: verified-unpartitioned topology contains partition %q", device.ID)
			}
		}
	}

	seenCapabilities := make(map[AcceleratorAction]map[AcceleratorTargetScope]struct{})
	for _, capability := range r.Capabilities {
		if !capability.Action.valid() {
			return fmt.Errorf("accelerator report: invalid capability action %q", capability.Action)
		}
		if len(capability.Scopes) == 0 {
			return fmt.Errorf("accelerator report: capability %q has no scopes", capability.Action)
		}
		if seenCapabilities[capability.Action] == nil {
			seenCapabilities[capability.Action] = make(map[AcceleratorTargetScope]struct{})
		}
		for _, scope := range capability.Scopes {
			if !scope.valid() {
				return fmt.Errorf("accelerator report: capability %q has invalid scope %q", capability.Action, scope)
			}
			if _, duplicate := seenCapabilities[capability.Action][scope]; duplicate {
				return fmt.Errorf("accelerator report: duplicate capability %q for scope %q", capability.Action, scope)
			}
			if capability.Action == AcceleratorActionResetDevice && scope == AcceleratorScopePhysicalDevice && r.TopologySafety != AcceleratorTopologyVerifiedUnpartitioned {
				return fmt.Errorf("accelerator report: physical device reset requires verified-unpartitioned topology")
			}
			seenCapabilities[capability.Action][scope] = struct{}{}
		}
	}
	return nil
}

// ApprovalDecision is a human decision on a pending approval.
type ApprovalDecision string

const (
	ApprovalApproved ApprovalDecision = "approved"
	ApprovalRejected ApprovalDecision = "rejected"
)

// Approval records who decided what, and through which channel.
type Approval struct {
	IncidentID string           `json:"incident_id"`
	StepName   string           `json:"step_name"`
	Decision   ApprovalDecision `json:"decision"`
	Actor      string           `json:"actor"`
	Channel    string           `json:"channel"` // "slack" | "cli" | "web"
	At         time.Time        `json:"at"`
}
