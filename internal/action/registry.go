// Package action is the single declarative registry of remediation actions.
//
// Every fact about a remediation action used to be smeared across the CRD enum
// (api/v1alpha1), the operator compiler (config_snapshot.go), the playbook
// allow-list (playbook.go), the safety gate (internal/safety), and the
// controller dispatch (reconcile.go). This package holds exactly one record per
// action; those layers derive their behavior from this table instead of each
// maintaining a private copy.
//
// The package sits below every consumer: it imports only the two leaf packages
// api/v1alpha1 (for the closed PlaybookAction enum, the API source of truth) and
// pkg/types (for the runtime ActionType/vendor vocabulary), neither of which
// imports any internal package, so operator, controller, playbook, and safety
// can all consume it without an import cycle.
package action

import (
	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Kind is the executor that runs an action. It is also the namespace prefix of
// the wire action string, which is always "<kind>.<op>".
type Kind string

const (
	KindPlatform Kind = "platform"
	KindAgent    Kind = "agent"
	KindVerify   Kind = "verify"
	KindNotify   Kind = "notify"
)

// Scope is the failure-domain scope a compiled action addresses. It drives the
// compiler's node-scheduling effect inference.
type Scope string

const (
	ScopeGPU  Scope = "gpu"
	ScopeNode Scope = "node"
)

// CapabilityGate names the runtime evidence gate the controller must satisfy
// before an action may execute. Most actions require none; today only the
// NVIDIA physical GPU reset does.
type CapabilityGate string

const (
	CapabilityNone        CapabilityGate = ""
	CapabilityNVIDIAReset CapabilityGate = "nvidia-reset"
)

// CloudPrimitive names the provider-neutral cloud capability a platform action
// depends on, or empty for an action that touches no cloud instance. The
// operator rejects a cloud action at compile time when the configured provider
// does not declare the matching capability; the axis itself is provider-side
// (see internal/cloud.Capabilities), so this only names which one is required.
type CloudPrimitive string

const (
	CloudPrimitiveNone CloudPrimitive = ""
	// CloudPrimitiveReinitializeInPlace is stop/start of the same instance, what
	// RecycleNode needs.
	CloudPrimitiveReinitializeInPlace CloudPrimitive = "reinitialize-in-place"
	// CloudPrimitiveReplace is terminate-for-autoscaler-replacement, what
	// ReplaceNode needs.
	CloudPrimitiveReplace CloudPrimitive = "replace"
)

// Definition is the complete, single-source description of one remediation
// action.
type Definition struct {
	// PlaybookAction is the closed CRD enum value that compiles to this action.
	// It is empty for a runtime-only action that has no CRD spelling and can be
	// reached only from a hand-authored playbook YAML (driver_reload,
	// driver_reinstall, run_script).
	PlaybookAction kubeneuronv1alpha1.PlaybookAction
	// Wire is the namespaced runtime action string carried on a playbook step
	// and the durable agent queue. It is always "<Kind>.<Op>".
	Wire string
	// Kind is the executor that runs the action; Op is the action name within
	// that executor. Dispatch reads both instead of splitting the wire string.
	Kind Kind
	Op   string
	// Scope is the failure-domain scope the compiler uses to decide node
	// scheduling effects. Meaningful only for CRD-compiled actions.
	Scope Scope
	// ForcesApproval marks whole-node actions that restart or destroy the
	// instance: the compiler rejects them unless the step declares approval
	// Required, regardless of execution mode.
	ForcesApproval bool
	// Destructive marks a controller-side platform action that changes or
	// destroys a node and therefore must stay inside the declared
	// spec.safety.destructiveExecution.nodeSelector (blast radius). The
	// controller's confinement check consults this flag instead of a private
	// map. Restorative platform ops (uncordon, restore_accelerator_stack) are
	// deliberately not destructive: undoing a change is always allowed.
	// Agent-side destructive actions (reboot, gpu_reset) are confined by the
	// agent's own execution nodeSelector, not this controller-path flag.
	Destructive bool
	// AgentDestructive marks an action whose execution path reaches the agent
	// executor's destructive boundary: it can only ever succeed on a node
	// whose agent runs --enable-destructive-actions. Distinct from
	// Destructive, which is the controller-path blast-radius axis — an action
	// can be restorative for confinement purposes (restore_accelerator_stack)
	// yet still require an armed agent to execute its host half.
	AgentDestructive bool
	// Compensating marks the one reversal that must remain live after a
	// DryRun safety stop: it restores monitoring only when KubeNeuron's own
	// durable quiesce marker says KubeNeuron previously disabled it. It is
	// intentionally narrower than Destructive=false; for example uncordon
	// still obeys its ownership and manual-handoff protocol.
	Compensating bool
	// IdleGuard marks an action whose ONLY job is to refuse when the device is
	// still in use. Its failure is not a remediation failure: it is the system
	// declining to disrupt a running workload, and the protection metric counts
	// it as such. A flag rather than a wire-string match so a second vendor's
	// idle guard is covered by adding a row here.
	IdleGuard bool
	// CapabilityGate is the runtime evidence gate the controller enforces
	// before dispatch.
	CapabilityGate CapabilityGate
	// CloudPrimitive names the provider capability a cloud action needs, or
	// empty. The operator gates the action against the configured provider's
	// declared capabilities at compile time.
	CloudPrimitive CloudPrimitive
	// Vendor is the accelerator vendor a capability-gated action is scoped to.
	// It is empty for vendor-neutral actions. Declarative for now: the reset
	// gate is still NVIDIA-specific in the controller.
	Vendor types.AcceleratorVendor
}

// GateAction is the safety-gate action class this step consumes. Agent actions
// gate on their bare op, so agent.reboot shares the reboot concurrency cap with
// the actuator's own reboot action; every other executor gates on its full wire
// string. This mirrors the historical gateAction formula exactly.
func (d Definition) GateAction() types.ActionType {
	if d.Kind == KindAgent {
		return types.ActionType(d.Op)
	}
	return types.ActionType(d.Wire)
}

// definitions is the registry. Order is stable and mirrors the CRD enum order,
// with the three runtime-only actions last.
var definitions = []Definition{
	{PlaybookAction: kubeneuronv1alpha1.ActionObserve, Wire: "notify.observe", Kind: KindNotify, Op: "observe", Scope: ScopeGPU},
	{PlaybookAction: kubeneuronv1alpha1.ActionCordon, Wire: "platform.cordon", Kind: KindPlatform, Op: "cordon", Scope: ScopeNode, Destructive: true},
	{PlaybookAction: kubeneuronv1alpha1.ActionDrain, Wire: "platform.drain", Kind: KindPlatform, Op: "drain", Scope: ScopeNode, Destructive: true},
	{PlaybookAction: kubeneuronv1alpha1.ActionUncordon, Wire: "platform.uncordon", Kind: KindPlatform, Op: "uncordon", Scope: ScopeNode},
	{PlaybookAction: kubeneuronv1alpha1.ActionEvictGPUWorkload, Wire: "platform.evict_gpu_workload", Kind: KindPlatform, Op: "evict_gpu_workload", Scope: ScopeGPU, Destructive: true},
	{PlaybookAction: kubeneuronv1alpha1.ActionIdleCheck, Wire: "agent.idle_check", Kind: KindAgent, Op: "idle_check", Scope: ScopeGPU, IdleGuard: true},
	{PlaybookAction: kubeneuronv1alpha1.ActionWaitIdle, Wire: "agent.wait_idle", Kind: KindAgent, Op: "wait_idle", Scope: ScopeGPU, IdleGuard: true},
	// Quiescing stands down DCGM and the device plugin on the node — it degrades
	// the node's monitoring and scheduling exactly like a drain degrades its
	// workloads, so it must stay inside the destructiveExecution blast radius.
	{PlaybookAction: kubeneuronv1alpha1.ActionQuiesceAcceleratorStack, Wire: "platform.quiesce_accelerator_stack", Kind: KindPlatform, Op: "quiesce_accelerator_stack", Scope: ScopeNode, Destructive: true, AgentDestructive: true},
	{PlaybookAction: kubeneuronv1alpha1.ActionRestoreAcceleratorStack, Wire: "platform.restore_accelerator_stack", Kind: KindPlatform, Op: "restore_accelerator_stack", Scope: ScopeNode, Compensating: true},
	{PlaybookAction: kubeneuronv1alpha1.ActionGPUReset, Wire: "agent.gpu_reset", Kind: KindAgent, Op: "gpu_reset", Scope: ScopeGPU, AgentDestructive: true, CapabilityGate: CapabilityNVIDIAReset, Vendor: types.AcceleratorVendorNVIDIA},
	{PlaybookAction: kubeneuronv1alpha1.ActionCollectBundle, Wire: "agent.collect_bundle", Kind: KindAgent, Op: "collect_bundle", Scope: ScopeNode},
	{PlaybookAction: kubeneuronv1alpha1.ActionReboot, Wire: "agent.reboot", Kind: KindAgent, Op: "reboot", Scope: ScopeNode, ForcesApproval: true, AgentDestructive: true},
	{PlaybookAction: kubeneuronv1alpha1.ActionRecycleNode, Wire: "platform.recycle_node", Kind: KindPlatform, Op: "recycle_node", Scope: ScopeNode, ForcesApproval: true, CloudPrimitive: CloudPrimitiveReinitializeInPlace, Destructive: true},
	{PlaybookAction: kubeneuronv1alpha1.ActionReplaceNode, Wire: "platform.replace_node", Kind: KindPlatform, Op: "replace_node", Scope: ScopeNode, ForcesApproval: true, CloudPrimitive: CloudPrimitiveReplace, Destructive: true},
	{PlaybookAction: kubeneuronv1alpha1.ActionRunDiag, Wire: "agent.run_diag", Kind: KindAgent, Op: "run_diag", Scope: ScopeNode},
	{PlaybookAction: kubeneuronv1alpha1.ActionVerifyGPUHealth, Wire: "verify.gpu_health", Kind: KindVerify, Op: "gpu_health", Scope: ScopeGPU},
	{PlaybookAction: kubeneuronv1alpha1.ActionVerifyNodeHealth, Wire: "verify.node_health", Kind: KindVerify, Op: "node_health", Scope: ScopeNode},
	{PlaybookAction: kubeneuronv1alpha1.ActionOpenTicket, Wire: "notify.ticket", Kind: KindNotify, Op: "ticket", Scope: ScopeNode},

	// Runtime-only actions: reachable from a hand-authored playbook YAML but not
	// from the closed CRD enum, so they carry no PlaybookAction spelling. Their
	// Scope is never consulted (they never flow through the compiler) but is set
	// to node for consistency.
	{Wire: "agent.driver_reload", Kind: KindAgent, Op: "driver_reload", Scope: ScopeNode, AgentDestructive: true},
	{Wire: "agent.driver_reinstall", Kind: KindAgent, Op: "driver_reinstall", Scope: ScopeNode, AgentDestructive: true},
	{Wire: "agent.run_script", Kind: KindAgent, Op: "run_script", Scope: ScopeNode, AgentDestructive: true},
}

var (
	byWire           = make(map[string]Definition, len(definitions))
	byPlaybookAction = make(map[kubeneuronv1alpha1.PlaybookAction]Definition)
)

func init() {
	for _, d := range definitions {
		byWire[d.Wire] = d
		if d.PlaybookAction != "" {
			byPlaybookAction[d.PlaybookAction] = d
		}
	}
}

// ByWire returns the definition for a runtime wire action string.
func ByWire(wire string) (Definition, bool) {
	d, ok := byWire[wire]
	return d, ok
}

// ByPlaybookAction returns the definition for a CRD enum action.
func ByPlaybookAction(a kubeneuronv1alpha1.PlaybookAction) (Definition, bool) {
	d, ok := byPlaybookAction[a]
	return d, ok
}

// Supported reports whether wire names a known runtime action. It is the
// playbook allow-list: any action not in the registry is rejected at load.
func Supported(wire string) bool {
	_, ok := byWire[wire]
	return ok
}

// Definitions returns a copy of the full registry in stable order.
func Definitions() []Definition {
	return append([]Definition(nil), definitions...)
}

// rebootClassActions are the safety-gate action classes that count against the
// reboot concurrency cap. power_cycle has no playbook wire spelling but stays
// reboot-class for any actuator that emits it directly.
var rebootClassActions = map[types.ActionType]bool{
	types.ActionReboot:          true,
	types.ActionPowerCycle:      true,
	types.ActionDriverReinstall: true,
}

// IsRebootClass reports whether a safety-gate action class counts against the
// reboot concurrency cap.
func IsRebootClass(a types.ActionType) bool {
	return rebootClassActions[a]
}
