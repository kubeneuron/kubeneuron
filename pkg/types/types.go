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
	// AgentRegistrationV2Path/Protocol are the v2 registration route beside
	// v1. v2's contract is "arming is always declared": the payload MUST carry
	// destructive_armed. A new route, not a new field on v1 — the v1 handler
	// strict-decodes (unknown fields 400) and the v1 capability is an exact
	// whole-body match, so neither side of v1 can be extended without breaking
	// one direction of a rolling upgrade. Agents probe v2 first and fall back
	// to v1 (omitting the field) against older controllers.
	AgentRegistrationV2Path     = "/api/v1/agents/register/narrow-v2"
	AgentRegistrationV2Protocol = "kubeneuron-agent-registration/v2"
	// AgentArmingHeader carries the controller's arming answer on a v2
	// registration response: whether THIS node's agent should arm its
	// destructive executor, computed from the compiled
	// spec.safety.destructiveExecution.nodeSelector against the node's
	// labels. The agent adopts the answer each registration tick (unless its
	// arming was pinned by an explicit --enable-destructive-actions flag),
	// which is what lets ONE DaemonSet cover the whole fleet: arming is data
	// served over the authenticated channel, not scheduling geometry. An
	// absent header (an older controller) changes nothing — the agent keeps
	// its current state, which defaults to unarmed.
	AgentArmingHeader = "X-KubeNeuron-Agent-Arming"
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
	// AgentActionRefusalHeader carries an action's machine-readable refusal
	// code alongside its result.
	//
	// A HEADER, not a field on the result body, for the reason stated at the
	// v2 registration route above: the result route strict-decodes, so an
	// unknown field is a 400. A new agent posting to a controller that
	// predates the code — which happens on rollback, and whenever agents are
	// upgraded first — would have had every result rejected, retried, and
	// finally timed out. An unknown HEADER is ignored by every version, in
	// both directions, with no negotiation and no second route.
	AgentActionRefusalHeader = "X-KubeNeuron-Action-Refusal"
	// AgentEventRejectedHeader marks a controller response that SEMANTICALLY
	// rejects the posted event: retrying can never succeed, so the agent must
	// drop the event instead of spooling or replaying it. The header — not the
	// bare status code — is the poison signal, because a 400 alone also means
	// "this controller is older than the agent" (strict JSON decoding during a
	// rolling upgrade) or a middlebox rejection, and those events must keep
	// spooling and drain once the skew clears.
	AgentEventRejectedHeader = "X-KubeNeuron-Event-Rejected"
)

// Target identifies what an incident or signal is about: a whole node, or a
// specific GPU on a node. GPUUUID is empty for node-scoped targets.
type Target struct {
	Node     string `json:"node"`
	GPUUUID  string `json:"gpu_uuid,omitempty"`
	GPUIndex int    `json:"gpu_index,omitempty"`
	// PCIAddr is the device's normalized bus address (types.NormalizePCIAddress),
	// empty when the signal's source did not name one.
	//
	// It exists because GPUUUID alone is not a complete device identity at the
	// moment it is needed most. A kernel fault that knocks a GPU off the bus
	// names the device by PCI address and CANNOT name it by UUID: nvidia-smi
	// no longer lists it. Before this field, every such incident was addressed
	// as "some GPU on this node", which had two consequences an operator paid
	// for. Two different unattributed GPUs on one node became one incident,
	// because nothing in the key told them apart. And when the vendor tool
	// resolved the same PCI address to a real UUID seconds later, that precise
	// signal was discarded as a duplicate of the vague one, so the incident
	// stayed unattributed and — after the ladder had already cordoned and
	// drained the node — was refused a reset and parked for a human with
	// "reset target unattributed".
	//
	// It is therefore matched on, not merely reported: it distinguishes two
	// unattributed devices, and it is the key the store promotes an
	// unattributed incident onto its real UUID by.
	//
	// Always store the normalized form. Sources spell the address differently
	// (see NormalizePCIAddress) and a raw value here would compare unequal to
	// itself across sources.
	PCIAddr string `json:"pci_addr,omitempty"`
}

// IsGPU reports whether the target is a single GPU rather than a whole node.
func (t Target) IsGPU() bool { return t.GPUUUID != "" }

// IsDeviceScoped reports whether the target is about ONE accelerator rather
// than the whole machine.
//
// Distinct from IsGPU, which asks only whether a UUID is known. A target
// carrying a bus address but no UUID names exactly one physical device — the
// card that fell off the bus — and the two questions came apart the day two
// unattributed GPUs on one node stopped collapsing into a single incident.
//
// Ask this wherever the answer decides how much capacity an incident covers.
// Both capacity counters used to ask IsGPU there, so a PCI-only incident was
// charged the node's ENTIRE inventory; with one incident per device, an 8-GPU
// node losing its PCIe switch billed 64 GPU-seconds per second.
func (t Target) IsDeviceScoped() bool { return t.GPUUUID != "" || t.PCIAddr != "" }

// IsUnattributed reports whether the target names a device that has not been
// resolved to a GPU UUID. Such a target can still name a physical device
// through PCIAddr; that is what makes an incident on it promotable rather than
// permanently unfixable.
func (t Target) IsUnattributed() bool { return t.GPUUUID == "" }

// AgentResultGrace is how long AFTER an action's own deadline its result is
// still accepted.
//
// An action's lease used to end exactly at its deadline, which made a
// timing-out action's result unreportable by construction: the executor
// cancels the work at T, POSTs the reason a moment later, and the store's
// completion predicate requires the lease to still be live. So the one result
// worth having — WHY it timed out, which processes still held the device — was
// the one guaranteed to be rejected, and the controller saw a generic timeout
// instead. The action then looked reclaimable and could be dispatched again;
// for the shipped 12h WaitIdle rung that is another twelve hours.
//
// It lives here because three components must agree on it: the store sizes the
// lease with it, the controller waits this much longer than the agent's budget
// for an answer, and the agent reports inside it. Two of those knew the number
// and the third did not.
const AgentResultGrace = 15 * time.Second

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

// Vendor is the accelerator vendor this signal names, or "" when it says
// nothing. Detectors carry it in Evidence because not every source knows one:
// an Alertmanager rule or a bare kernel line may not.
//
// One accessor rather than the map lookup spelled at each site, because the
// answer now decides which remediation ladder runs. An NVIDIA reset ladder
// selected for an AMD card is not a failed step, it is the wrong hammer on
// somebody's hardware.
func (s Signal) Vendor() AcceleratorVendor {
	return AcceleratorVendor(s.Evidence["vendor"])
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

// Halted reports whether automated remediation has ended for the state: the
// incident is lifecycle-terminal (RESOLVED/EXPIRED) or parked for a human
// (NEEDS_HUMAN, which only a manual decision leaves). This is the single
// definition behind the store's claim guard (a queued action whose incident
// is halted must never be handed to an agent), the controller's
// active-incident checks, and the remediation-slot release — the three copies
// previously drifting past each other.
func (s IncidentState) Halted() bool {
	return s.Terminal() || s == StateNeedsHuman
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
	// Vendor is the accelerator vendor the originating fault named, empty
	// when the signal did not say (every XID, every alert, and every row
	// that predates the column). It exists so a preflight can tell an
	// IMPOSSIBLE device action from one whose evidence has not arrived yet:
	// a reset scoped to one vendor can never satisfy an incident about
	// another's device, and treating that as missing evidence holds the
	// incident — after cordon and drain — forever.
	Vendor AcceleratorVendor `json:"vendor,omitempty"`
	// RemediationSlotHeld records durably that this incident holds its
	// target's safety-gate remediation slot: set in the same transaction as
	// its first EXECUTING transition, cleared in the same transaction as the
	// transition that halts it. A new leader rebuilds gate occupancy from
	// this bit; the gate's in-memory refcounts are a projection of it.
	RemediationSlotHeld bool `json:"remediation_slot_held,omitempty"`
	// ApprovalEpoch identifies the incident's current approval round: bumped
	// in the same transaction as each park (and re-park) for approval, which
	// also records the round's "requested" row. See Approval.ParkEpoch.
	ApprovalEpoch int       `json:"approval_epoch,omitempty"`
	OpenedAt      time.Time `json:"opened_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// StateChangedAt is when State last changed. Unlike UpdatedAt it is not
	// bumped by attaching duplicate signals, so timeouts that anchor to a
	// state (approval TTL, verification quiet windows) cannot be postponed
	// by a signal storm.
	StateChangedAt time.Time  `json:"state_changed_at"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	// Version is the optimistic-concurrency counter. The store bumps it on
	// every UpdateIncident and matches on its prior value, so a writer holding
	// a stale snapshot cannot overwrite a row a concurrent writer has since
	// advanced. It is store-owned bookkeeping, not part of the incident's
	// domain state.
	Version int `json:"-"`
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
	// ActionQuiesceAcceleratorHost releases the node-side handles on the GPU
	// that no Kubernetes label can reach: the persistence daemon holds the
	// device even on a fully drained node, and a reset fails while it does.
	ActionQuiesceAcceleratorHost ActionType = "quiesce_accelerator_host"
	// ActionRestoreAcceleratorHost undoes it.
	ActionRestoreAcceleratorHost ActionType = "restore_accelerator_host"
	ActionPowerCycle             ActionType = "power_cycle"
)

// Action is a single unit of work executed on a node. ID is deterministic —
// hash(incident, step, attempt) — so replays after a controller restart are
// idempotent: executors keep a short-lived cache of completed action IDs.
type Action struct {
	ID string `json:"id"`
	// IncidentID is the incident this action was enqueued for. It is carried
	// explicitly (not smuggled through Params) so the actuator can stamp it onto
	// the durable queue entry, which is what CancelPendingActionsForIncident
	// matches when a superseded ladder rung must be tombstoned. Empty for actions
	// not bound to an incident.
	IncidentID string            `json:"incident_id,omitempty"`
	Type       ActionType        `json:"type"`
	Params     map[string]string `json:"params,omitempty"`
	Timeout    time.Duration     `json:"timeout,omitempty"`
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
	// Dead marks work that exhausted its attempt budget. It is a TERMINAL
	// state like the other two, and it is the one that keeps being forgotten:
	// a row in it can never be claimed again, so code that tests only Done and
	// Cancelled reads it as "still in flight" and waits for something that will
	// never happen.
	Dead bool `json:"dead,omitempty"`
}

// Terminal reports whether this action can never make further progress.
//
// It exists because the answer was being spelled out inline, differently, at
// every new site that needed it — and eight review rounds running found the
// same defect wearing a different hat: the store's prune query knew three
// terminal states, the discard knew two, the janitor's re-dispatch probe knew
// two different ones. Each omission wedged a node's GPU monitoring for the
// retention window while consuming a shared budget on every tick.
//
// So there is one predicate, and asking it is the only supported way to know.
func (q QueuedAction) Terminal() bool { return q.Done || q.Dead || q.Cancelled }

// ActionResult is the outcome of executing an Action.
type ActionResult struct {
	ActionID string `json:"action_id"`
	OK       bool   `json:"ok"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
	// Refusal is a machine-readable reason the action declined to act, as
	// distinct from failing to act. Empty means the failure was not a
	// recognised refusal — including on an older agent that predates it,
	// which is why every consumer must treat an empty value as "no evidence
	// of a refusal" rather than guessing from Error's prose. Nothing that
	// chooses between stopping and escalating may depend on it; see
	// idleGuardStopped.
	//
	// `json:"-"` is deliberate: this travels in AgentActionRefusalHeader, not
	// in the strict-decoded result body. The cost is that an agent restart
	// between execution and the result post loses the code, so the protection
	// metric under-counts — the same honest direction it already documents.
	Refusal    string    `json:"-"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Refusal codes carried in ActionResult.Refusal. They are wire values: renaming
// one silently changes what a controller counts as protection.
const (
	// RefusalNotIdle: an idle guard found live processes still holding the
	// device. The guard did its job — this is the protection working, and it
	// is NOT the same event as an idle probe that could not run at all.
	RefusalNotIdle = "not_idle"
)

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
	// AgentArming is the tri-state destructive-arming fact the node's agent
	// reported at its last registration. Empty means unknown: an agent too
	// old to report it, or one talking to this controller through the v1
	// registration protocol.
	AgentArming AgentArming `json:"agent_arming,omitempty"`
}

// AgentArming is the destructive-arming state a node's agent declares at
// registration. Empty is UNKNOWN and must never be treated as a declared
// value in either direction.
type AgentArming string

const (
	AgentArmingUnknown AgentArming = ""
	AgentArmingArmed   AgentArming = "armed"
	AgentArmingUnarmed AgentArming = "unarmed"
)

// AgentRegistration is the inventory data an agent is allowed to own. Fields
// such as platform, labels, actuation addresses, and pause state are managed by
// the controller and must not be writable through the registration endpoint.
type AgentRegistration struct {
	Name   string    `json:"name"`
	GPUs   []GPUInfo `json:"gpus,omitempty"`
	BootID string    `json:"boot_id,omitempty"`
	// DestructiveArmed reports whether this agent process was started with
	// --enable-destructive-actions. Pointer, not bool: it is omitted entirely
	// on the v1 wire (old controllers reject unknown fields with strict JSON
	// decoding), and absent means unknown — an old agent, or a new agent
	// talking to an old controller. The v2 registration protocol REQUIRES it.
	DestructiveArmed *bool `json:"destructive_armed,omitempty"`
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
	EventID  string `json:"event_id,omitempty"`
	Node     string `json:"node"`
	GPUIndex int    `json:"gpu_index"`
	GPUUUID  string `json:"gpu_uuid,omitempty"`
	// PCIAddr is the GPU's PCI address when the source knows it (the kmsg XID
	// line always carries it, even when the device has fallen off the bus and
	// cannot be resolved to an index/UUID). It disambiguates two distinct GPUs
	// that fail with the same XID on one node while both are unattributed.
	PCIAddr string `json:"pci_addr,omitempty"`
	// XID is the NVIDIA-native fault encoding. It stays authoritative for the
	// two paths that carry a GENUINE XID: the kmsg NVRM line and DCGM's real
	// last-XID field (DCGM_FI_DEV_XID_ERRORS). It is NOT a universal fault
	// identity: a non-NVIDIA source, or an NVIDIA source that observes a fault
	// without a real XID, uses Fault instead and leaves XID zero.
	XID int `json:"xid"`
	// Fault is the optional, vendor-neutral fault descriptor. It is the honest
	// landing place for a fault that is not an XID — a future AMD/Intel source,
	// or the nvidia-smi ECC/row-remap counter fallback, which observes a real
	// NVIDIA fault it must not pretend is an XID. The field is additive and
	// optional so the wire stays backward-tolerant. Exactly one identity may
	// be set per event: classification is Fault-first (a set Fault is
	// authoritative and the XID is not consulted), and the controller rejects
	// an event carrying both a nonzero XID and a Fault at ingest.
	Fault     *FaultSignal `json:"fault,omitempty"`
	Raw       string       `json:"raw,omitempty"` // original kmsg line
	Timestamp time.Time    `json:"timestamp"`
}

// FaultSignal is a vendor-neutral fault descriptor carried on an AgentEvent
// beside the NVIDIA-specific XID. It keeps XID as the NVIDIA-native encoding
// rather than the universal one: a source that has a genuine XID sets XID; a
// source that does not (a non-NVIDIA accelerator, or the nvidia-smi ECC/remap
// fallback) describes the fault here. The detector maps (Vendor, Code) to a
// ProblemClass alongside the XID catalog, so a migrated NVIDIA fault classifies
// to exactly the class its former synthesized XID produced.
type FaultSignal struct {
	// Vendor is the accelerator vendor: "nvidia", "amd", "intel", ...
	Vendor string `json:"vendor"`
	// Source is the detection source that observed the fault: "kmsg", "dcgm",
	// "nvidia-smi", ... It is provenance, not classification input.
	Source string `json:"source"`
	// Code is a vendor-native fault code string, e.g. "ecc-dbe" or
	// "row-remap-failure". It is deliberately not an XID: XID stays the
	// NVIDIA-specific encoding on its own field.
	Code string `json:"code"`
	// Attributes carries optional source-specific detail (observed counters,
	// etc.). It never carries classification authority.
	Attributes map[string]string `json:"attributes,omitempty"`
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
	// DeviceHolders are the processes currently holding a GPU device node.
	//
	// They are reported continuously, not only when a reset is attempted, so
	// the controller can refuse a reset playbook before it cordons and drains a
	// node whose device something will never release. An empty slice means
	// nothing holds a device; a nil slice means the agent did not look, and the
	// two must not be confused by a gate.
	//
	// Deliberately WITHOUT omitempty, unlike every other optional field here.
	// omitempty elides a zero-length slice whether or not it is nil, so the
	// wire could not carry the distinction at all: every report from a healthy
	// node whose agent looked and found the device clear arrived as "did not
	// look". The store goes to real trouble to preserve the difference, with
	// round-trip tests on both engines, and none of it could ever observe the
	// empty case. Harmless today — the one gate that reads this treats nil and
	// empty alike — and silently wrong in the permissive direction for the
	// next reader who asks whether the agent actually looked.
	DeviceHolders []AgentDeviceHolder `json:"device_holders"`
}

// AgentDeviceHolder is a process holding an accelerator device node. It is not
// the same thing as a compute application: on a stock GPU Operator node the
// vendor's own monitoring holds the device without ever running a CUDA context,
// and a reset fails for as long as it does.
type AgentDeviceHolder struct {
	PID     int    `json:"pid"`
	Command string `json:"command"`
	Device  string `json:"device"`
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

// ApprovalDecision is a human decision on a pending approval — or, for
// ApprovalRequested, the system's durable record of what was asked.
type ApprovalDecision string

const (
	ApprovalApproved ApprovalDecision = "approved"
	ApprovalRejected ApprovalDecision = "rejected"
	// ApprovalRequested is not a decision: it records, at the moment an
	// incident parks for approval, the identity of the step the human is being
	// asked to approve. A later decision binds to this requested identity — the
	// step the human was shown — so a playbook hot-swap between the request and
	// the click cannot substitute the action being approved.
	ApprovalRequested ApprovalDecision = "requested"
)

// Approval records who decided what, and through which channel.
//
// The decision is bound to the identity of the step it was made for — not just
// the incident. A playbook can be hot-swapped in place (CR/ConfigMap edit) or an
// incident rewound while a decision is pending, either of which can change the
// action sitting at the incident's current step index. The bound identity lets
// the resume path detect that the current step is no longer the one that was
// approved and fail closed (re-park) rather than execute an action the human
// never saw. PlaybookName/StepAction/StepHash are empty for legacy rows recorded
// before this binding existed.
type Approval struct {
	IncidentID string           `json:"incident_id"`
	StepName   string           `json:"step_name"`
	Decision   ApprovalDecision `json:"decision"`
	Actor      string           `json:"actor"`
	Channel    string           `json:"channel"` // "slack" | "cli" | "web"
	At         time.Time        `json:"at"`
	// PlaybookName is the playbook the approved step belonged to at decision time.
	PlaybookName string `json:"playbook_name,omitempty"`
	// StepAction is the wire action of the approved step (e.g. "agent.reboot").
	StepAction string `json:"step_action,omitempty"`
	// StepHash is a content hash over the approved step (playbook, name, action,
	// approval, params), so an edited step at the same index is detected even if
	// its name and action are unchanged.
	StepHash string `json:"step_hash,omitempty"`
	// ParkEpoch is the approval round this row belongs to: each park (and
	// re-park) of an incident bumps Incident.ApprovalEpoch, the park writes a
	// "requested" row carrying the new epoch, and a decision inherits the
	// epoch of the request it answers. Resume honors a decision only when its
	// epoch equals the incident's current one, so a decision can never be
	// carried across a re-park.
	ParkEpoch int `json:"park_epoch,omitempty"`
}
