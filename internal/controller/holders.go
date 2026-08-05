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
func (c *Controller) refuseInfeasibleReset(ctx context.Context, inc *types.Incident, book *playbook.Playbook) error {
	if inc.DryRun || book == nil || !playbookResetsAGPU(book) {
		return nil
	}
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
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return nil
	}
	report, err := reports.GetAcceleratorReport(ctx, inc.Target.Node, types.AcceleratorVendorNVIDIA)
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

// FormatObstructions renders obstructions for one operator-facing message.
func FormatObstructions(obstructions []ResetObstruction) string {
	parts := make([]string, 0, len(obstructions))
	for _, o := range obstructions {
		parts = append(parts, o.String())
	}
	return strings.Join(parts, "; ")
}
