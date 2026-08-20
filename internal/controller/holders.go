package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// quiesceableComponents maps the process name a node reports to the GPU
// Operator component that owns it.
//
// The mapping exists so the controller can tell, before touching a node,
// whether it has any way to release a given holder. Linux truncates process
// names to 15 characters, which is why these are prefixes rather than exact
// names: `nvidia-device-plugin` arrives as `nvidia-device-p`.
var quiesceableComponents = map[string]string{
	"nv-hostengine":   "dcgm",
	"dcgm-exporter":   "dcgm-exporter",
	"nvidia-device-p": "device-plugin",
}

// hostQuiescedCommands are released by the agent on the node itself, so no
// Kubernetes label has to exist for them.
var hostQuiescedCommands = map[string]bool{
	"nvidia-persiste": true, // nvidia-persistenced, truncated by the kernel
}

// ResetObstruction explains why a reset cannot succeed on a node.
type ResetObstruction struct {
	// Holder is the process, as the node reported it.
	Holder types.AgentDeviceHolder
	// Reason is operator-facing and says what to do about it.
	Reason string
}

func (o ResetObstruction) String() string {
	return fmt.Sprintf("%s(%d) holds %s: %s", o.Holder.Command, o.Holder.PID, o.Holder.Device, o.Reason)
}

// resetObstructions reports the holders that would defeat a reset on this node
// and that KubeNeuron cannot release.
//
// The point is timing. Every one of these would be caught later by the node-side
// preflight, but by then a playbook has cordoned the node, evicted its
// workloads, and stood the vendor's monitoring down — all for a reset that was
// never going to run. Answering from the accelerator report instead lets the
// refusal happen before the first disruptive step.
//
// A nil holder list means the agent did not look, which is not evidence of a
// clear device; it produces no obstruction here and the node-side preflight
// remains the backstop. An empty list means the node looked and found nothing.
func resetObstructions(node *types.Node, holders []types.AgentDeviceHolder, forbidden []string) []ResetObstruction {
	forbid := map[string]bool{}
	for _, name := range forbidden {
		if name = strings.TrimSpace(name); name != "" {
			forbid[name] = true
		}
	}
	var out []ResetObstruction
	for _, holder := range holders {
		command := strings.TrimSpace(holder.Command)
		switch {
		case forbid[command]:
			out = append(out, ResetObstruction{Holder: holder,
				Reason: "declared in spec.safety.quiesce.forbidResetWhenPresent, so this node is not reset per-device"})
		case hostQuiescedCommands[command]:
			// The agent stops these itself; nothing has to be true of the node.
		default:
			component, quiesceable := quiesceableComponents[command]
			if !quiesceable {
				out = append(out, ResetObstruction{Holder: holder, Reason: strings.Join([]string{
					"KubeNeuron has no way to release this process",
					"stand it down before the reset, or declare it in spec.safety.quiesce.forbidResetWhenPresent",
				}, "; ")})
				continue
			}
			if !componentIsOperatorManaged(node, component) {
				// The label is how the GPU Operator is asked to remove the
				// component. Its absence means this instance came from
				// somewhere else — on EKS, the machine image — and flipping
				// labels will do nothing to it.
				out = append(out, ResetObstruction{Holder: holder, Reason: fmt.Sprintf(
					"not managed by the GPU Operator on this node (no %s%s label), so the quiesce cannot stand it down; "+
						"install the operator's own %s and remove the one from the machine image",
					acceleratorDeployLabelPrefix, component, component)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Holder.Command != out[j].Holder.Command {
			return out[i].Holder.Command < out[j].Holder.Command
		}
		return out[i].Holder.PID < out[j].Holder.PID
	})
	return dedupeObstructionsByCommand(out)
}

// dedupeObstructionsByCommand keeps one entry per process. A holder of three
// device nodes is one thing to fix, not three.
func dedupeObstructionsByCommand(in []ResetObstruction) []ResetObstruction {
	seen := map[string]bool{}
	var out []ResetObstruction
	for _, o := range in {
		key := fmt.Sprintf("%s/%d", o.Holder.Command, o.Holder.PID)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, o)
	}
	return out
}

// acceleratorDeployLabelPrefix is the GPU Operator's per-component switch. It is
// duplicated from the Kubernetes platform on purpose: the controller must be
// able to reason about the label without depending on a platform that may not
// be Kubernetes at all.
const acceleratorDeployLabelPrefix = "nvidia.com/gpu.deploy."

func componentIsOperatorManaged(node *types.Node, component string) bool {
	if node == nil || node.Labels == nil {
		return false
	}
	_, present := node.Labels[acceleratorDeployLabelPrefix+component]
	return present
}

// refuseInfeasibleReset reports why a playbook containing a GPU reset cannot
// work on this node, or nil when nothing is known to stand in the way.
//
// It runs once, before the playbook's first step, and it is deliberately
// different in kind from the capability gate. The gate holds when evidence is
// missing, because evidence can arrive. This refuses when the node reports a
// fact that will not change on its own — a process holding the GPU that nothing
// in KubeNeuron can release — so waiting would only postpone the same failure
// until after the node had been cordoned and drained.
//
// Escalating here, while idleGuardStopped quarantines on what looks like the
// same fact, is not an inconsistency — the two see it at different points with
// different information, and the difference is what makes each right:
//
//   - Here, nothing has been disrupted yet, and escalation climbs to a rung
//     that begins with cordon and drain and requires a human approval before
//     the reboot itself. A workload holding the device is moved gracefully by
//     that drain, not killed. The alternative — parking — leaves a degraded
//     GPU in service because somebody was using it.
//   - The idle guard runs as a step INSIDE the ladder, after that cordon and
//     drain have already happened. A holder still present there is one the
//     graceful path could not move, so the next rung's drain would not help
//     either, and escalating really would be trading the workload for the
//     device. Quarantine is the honest answer at that point.
//
// Written down because reading either site alone makes the other look wrong.
func (c *Controller) refuseInfeasibleReset(ctx context.Context, inc *types.Incident, book *playbook.Playbook) error {
	if book == nil || !playbookResetsAGPU(book) {
		return nil
	}
	// The STRUCTURAL refusals below run for a dry-run incident too. They read
	// permanent reported facts — no device UUID, a reset scoped to a vendor
	// this incident is not about — and refusing costs nothing, because a
	// dry-run ladder disrupts nothing either way.
	//
	// Skipping them made the simulation model a strictly MORE capable system
	// than the live one: on AMD silicon, ecc-dbe walked its whole ladder in
	// dry-run and landed in "would have recovered", while the same incident
	// live is refused before the first disruptive step and parked for a human.
	// The pilot checklist's entire premise is to sit in dry-run and read what
	// this would have got you, and that number was inflated worst on exactly
	// the fleets where the honest answer is "nothing".
	//
	// The holder check further down stays waived for dry-run: which processes
	// hold a device is live state that changes between now and any real run,
	// so refusing on it would report a permanent verdict about a temporary
	// fact.
	if inc.Target.GPUUUID == "" {
		// An unattributed incident (kmsg PCI->GPU resolution failed while
		// nvidia-smi was wedged) carries no device to reset, and no later report
		// can supply one. That is a reported fact, not missing evidence, so refuse
		// it here — before the first cordon/drain — and let the ladder escalate to
		// the reboot rung, which works without a UUID. Without this the incident
		// would drain the node and then re-deny the reset on every reconcile tick,
		// silently, forever.
		return fmt.Errorf("playbook %q resets a GPU but incident %s is unattributed (no GPU UUID); a per-device reset is impossible: %w",
			book.Name, inc.ID, errResetTargetUnattributed)
	}
	if vendor, mismatched := resetVendorMismatch(inc, book); mismatched {
		return fmt.Errorf("playbook %q resets a %s GPU but incident %s is about a %s device; that reset can never run: %w",
			book.Name, vendor, inc.ID, inc.Vendor, errResetVendorMismatch)
	}
	if vendor, mismatched := c.resetVendorAbsentFromNode(ctx, inc, book); mismatched {
		return fmt.Errorf("playbook %q resets a %s GPU but node %s reports no %s runtime; that reset can never run: %w",
			book.Name, vendor, inc.Target.Node, vendor, errResetVendorMismatch)
	}
	if inc.DryRun {
		return nil // the holder check below reads live state; see above
	}
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return nil
	}
	// Read the report for the incident's OWN vendor. Reading NVIDIA's on an AMD
	// incident asked a question about the wrong runtime: it found nothing,
	// returned nil, and the obstruction check silently no-opped — conservative
	// in direction, but it meant this preflight protected nothing outside
	// NVIDIA while appearing to run.
	vendor := inc.Vendor
	if !vendor.Valid() {
		vendor = types.AcceleratorVendorNVIDIA
	}
	report, err := reports.GetAcceleratorReport(ctx, inc.Target.Node, vendor)
	if err != nil || report == nil || report.DeviceHolders == nil {
		// No report, or an agent that did not look. The node-side preflight
		// stays the backstop; refusing on absent evidence would block resets on
		// every fleet running an older agent.
		return nil
	}
	node, _ := c.acceleratorNode(ctx, inc.Target.Node)
	obstructions := resetObstructions(node, report.DeviceHolders, c.forbiddenHolders(ctx))
	if len(obstructions) == 0 {
		return nil
	}
	return fmt.Errorf("playbook %q resets a GPU, which cannot succeed on %s: %s",
		book.Name, inc.Target.Node, FormatObstructions(obstructions))
}

// playbookResetsAGPU reports whether any step performs a capability-gated
// physical reset — a registry fact, so a second gated reset action (another
// vendor's) is covered without editing this function.
func playbookResetsAGPU(book *playbook.Playbook) bool {
	for _, step := range book.Steps {
		if def, ok := action.ByWire(step.Action); ok && def.CapabilityGate == action.CapabilityNVIDIAReset {
			return true
		}
	}
	return false
}

// resetVendorAbsentFromNode catches the same impossibility as
// resetVendorMismatch, from the other side: the NODE, not the incident.
//
// INERT IN THIS BUILD, and deliberately kept. internal/accelerator carries one
// adapter, so every report a node can produce says nvidia and the only action
// with a declared vendor needs nvidia — the lookup below can never miss. The
// failure described next is therefore one this build cannot reach; it becomes
// reachable the day a second adapter lands, which is when a check written
// afterwards would be written under pressure. TestAttestedVendorsMatchThe
// Adapters is what will notice that day arriving.
//
// An incident that never learned its vendor — a manual operator trigger, a
// metric alert — bound to a playbook whose reset targets a vendor the machine
// does not run would otherwise pass every check here, cordon and drain the
// node, and only then fail at the reset step on missing evidence: a hold with
// no deadline, re-denying on every tick, with the node left drained and
// nobody paged. That is exactly the failure errResetVendorMismatch exists to
// prevent, reached through a different door.
//
// It requires POSITIVE evidence of the mismatch: at least one report from this
// node, and none from the vendor the reset needs. A node that has simply not
// reported yet — a fresh install, a restarted agent — reports nothing at all
// and is left alone, because "no evidence yet" and "evidence of a different
// runtime" are not the same claim.
func (c *Controller) resetVendorAbsentFromNode(ctx context.Context, inc *types.Incident, book *playbook.Playbook) (types.AcceleratorVendor, bool) {
	if inc.Vendor.Valid() {
		return "", false // resetVendorMismatch already answered this
	}
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return "", false
	}
	present, err := reports.ListAcceleratorReports(ctx, inc.Target.Node)
	if err != nil || len(present) == 0 {
		return "", false
	}
	has := make(map[types.AcceleratorVendor]bool, len(present))
	for _, r := range present {
		has[r.Vendor] = true
	}
	for i := range book.Steps {
		def, ok := action.ByWire(book.Steps[i].Action)
		if !ok || def.Vendor == "" || has[def.Vendor] {
			continue
		}
		return def.Vendor, true
	}
	return "", false
}

// resetVendorMismatch reports the vendor a playbook's reset is scoped to when
// the incident is about a DIFFERENT vendor's device. An incident with no
// vendor (an XID, an alert, or a row predating the column) constrains nothing
// and never mismatches, so this cannot refuse anything that worked before the
// field existed.
func resetVendorMismatch(inc *types.Incident, book *playbook.Playbook) (types.AcceleratorVendor, bool) {
	if !inc.Vendor.Valid() {
		return "", false
	}
	for _, step := range book.Steps {
		def, ok := action.ByWire(step.Action)
		if !ok || def.Vendor == "" || def.Vendor == inc.Vendor {
			continue
		}
		return def.Vendor, true
	}
	return "", false
}

// FormatObstructions renders obstructions for one operator-facing message.
func FormatObstructions(obstructions []ResetObstruction) string {
	parts := make([]string, 0, len(obstructions))
	for _, o := range obstructions {
		parts = append(parts, o.String())
	}
	return strings.Join(parts, "; ")
}
