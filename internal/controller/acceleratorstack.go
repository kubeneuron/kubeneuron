package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// pinnedAcceleratorEvidence is the runtime attestation captured at the moment
// the vendor stack was switched off.
//
// The capability gate normally reads the node's latest accelerator report. That
// breaks down for exactly one step: quiescing stops the DCGM host engine, the
// agent's very next report therefore carries no attested runtime version, and
// the gate would deny the reset the quiesce was performed for. The attestation
// was real when it was taken, so the controller keeps it for the short life of
// the playbook and validates against it instead.
//
// The pin lives in memory only, and deliberately so. Persisting it would make a
// snapshot taken before a crash durable, and evidence that outlives the process
// which validated it is exactly what a fail-closed gate must not accept.
// Recovery does not need it: the platform records which nodes are quiesced, so a
// restarted controller restores them and sends any incident still running there
// back to its quiesce step, which re-attests from scratch.
type pinnedAcceleratorEvidence struct {
	node     string
	nodeUID  string
	profile  *config.AcceleratorRuntimeProfile
	report   types.AgentAcceleratorReport
	pinnedAt time.Time
}

// pinAcceleratorEvidence records the evidence for an incident.
func (c *Controller) pinAcceleratorEvidence(incidentID string, pin pinnedAcceleratorEvidence) {
	c.pinnedEvidenceMu.Lock()
	defer c.pinnedEvidenceMu.Unlock()
	if c.pinnedEvidence == nil {
		c.pinnedEvidence = map[string]pinnedAcceleratorEvidence{}
	}
	c.pinnedEvidence[incidentID] = pin
}

// takePinnedAcceleratorEvidence returns the pin for an incident if it is still
// within the evidence age bound. An expired pin is dropped rather than reused:
// evidence does not become truer by being held onto.
func (c *Controller) takePinnedAcceleratorEvidence(incidentID string, now time.Time) (pinnedAcceleratorEvidence, bool) {
	c.pinnedEvidenceMu.Lock()
	defer c.pinnedEvidenceMu.Unlock()
	pin, ok := c.pinnedEvidence[incidentID]
	if !ok {
		return pinnedAcceleratorEvidence{}, false
	}
	if now.Sub(pin.report.ObservedAt) > verifyEvidenceMaxAge {
		delete(c.pinnedEvidence, incidentID)
		return pinnedAcceleratorEvidence{}, false
	}
	return pin, true
}

func (c *Controller) forgetPinnedAcceleratorEvidence(incidentID string) {
	c.pinnedEvidenceMu.Lock()
	defer c.pinnedEvidenceMu.Unlock()
	delete(c.pinnedEvidence, incidentID)
}

// pinnedAcceleratorIncidentsByNode maps each pinned node to the incident that
// pinned it. Keyed by node because recovery starts from what the platform says
// is quiesced, not from what this process happens to remember.
func (c *Controller) pinnedAcceleratorIncidentsByNode() map[string]string {
	c.pinnedEvidenceMu.Lock()
	defer c.pinnedEvidenceMu.Unlock()
	out := make(map[string]string, len(c.pinnedEvidence))
	for incidentID, pin := range c.pinnedEvidence {
		out[pin.node] = incidentID
	}
	return out
}

// rewindIncidentsToQuiesce sends any incident still running on this node back to
// its quiesce step.
//
// A controller that restarted has no pinned evidence, so the node was restored
// above and its attestation is valid again — but the incident may have advanced
// past the quiesce and would otherwise run its reset against a node whose stack
// is back up. Re-running the quiesce re-attests, re-pins, and stands the vendor
// components down again, which is the only sequence that can work.
// It reports whether every needed rewind committed: a false return means at
// least one rewind failed or was skipped, and the caller must NOT restore the
// vendor stack yet — restoring would delete the durable quiesce marker that
// makes this retryable, leaving an un-rewound incident to run its reset
// against a restored stack.
func (c *Controller) rewindIncidentsToQuiesce(ctx context.Context, node string) bool {
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{
		States: []types.IncidentState{
			types.StateEvaluating, types.StateExecuting, types.StateAwaitingApproval,
		},
	})
	if err != nil {
		c.log.Warn("listing incidents for quiesce rewind failed", "node", node, "err", err)
		return false
	}
	complete := true
	for _, inc := range incidents {
		if inc.Target.Node != node {
			continue
		}
		if c.isInFlight(inc.ID) {
			// A step goroutine still owns this incident. Rewinding its StepIndex
			// here would be silently clobbered by that goroutine's post-step
			// transition: transition takes the fresh row's Version and writes back
			// this snapshot's StepIndex/Attempt, un-rewinding the incident. Skip
			// it, exactly as resolveIncidentsOnVanishedNodes does; the next
			// janitor pass rewinds it if it is still past the quiesce step — and
			// the marker survives because we report incomplete below.
			complete = false
			continue
		}
		book, ok := c.runtimeConfig(ctx).Engine.Playbook(inc.Playbook)
		if !ok {
			continue
		}
		index, found := quiesceStepIndex(book)
		if !found || inc.StepIndex <= index {
			continue
		}
		c.log.Info("rewinding incident to its quiesce step after recovering the node",
			"incident", inc.ID, "node", node, "from_step", inc.StepIndex, "to_step", index)
		// Bumping StateChangedAt here is the WRITE-FENCE that makes this
		// field-only rewrite safe against the concurrently-running walk:
		// transition() and parkForApproval() conflict when the fresh row's
		// StateChangedAt differs from their caller's snapshot, so a walk
		// advance that listed this incident BEFORE the rewind cannot silently
		// write its stale StepIndex back over it. The bump also keeps the
		// approval-TTL anchor honest, and requestMismatch re-parks any
		// pending decision against the rewound step.
		inc.StateChangedAt = time.Now()
		inc.StepIndex = index
		// Bump Attempt rather than zeroing it. Action IDs are hash(incident,
		// stepIndex, attempt); a rewound quiesce that reused attempt 0 would
		// recompute the *same* ID as the original, already-'done' action, so the
		// executor would replay the old success without actually standing the
		// vendor stack down again — and the reset would then run against a node
		// whose stack the janitor just brought back up. A monotonic bump gives
		// the rewound step a fresh identity so it truly re-executes, while a
		// genuine same-attempt retry after failover (StepIndex still at or before
		// the quiesce step, handled elsewhere) keeps its ID and still dedupes.
		inc.Attempt++
		if err := c.store.UpdateIncident(ctx, inc); err != nil {
			c.log.Warn("rewinding incident failed", "incident", inc.ID, "err", err)
			complete = false
		}
	}
	return complete
}

// quiesceStepIndex finds the step that stands the vendor stack down. The wire
// spelling comes from the registry record, not a literal, so a renamed or
// second quiesce-class action cannot silently miss the rewind.
func quiesceStepIndex(book *playbook.Playbook) (int, bool) {
	def, ok := action.ByPlaybookAction(kubeneuronv1alpha1.ActionQuiesceAcceleratorStack)
	if !ok {
		return 0, false
	}
	for i, step := range book.Steps {
		if step.Action == def.Wire {
			return i, true
		}
	}
	return 0, false
}

// quiesceAcceleratorStack captures the runtime attestation, switches off the
// vendor components that hold the GPU device nodes, and waits for them to go.
//
// Capturing first is the whole point of the ordering: once DCGM is down there
// is no attestation left to capture, and a reset with no attestation must not
// happen. If the evidence is not good enough for a reset right now, nothing is
// switched off at all.
func (c *Controller) quiesceAcceleratorStack(ctx context.Context, inc *types.Incident, step *playbook.Step) (*types.ActionResult, error) {
	controller, ok := c.platform.(platform.AcceleratorStackController)
	if !ok {
		return nil, fmt.Errorf("platform.quiesce_accelerator_stack: platform %q cannot control the accelerator stack", c.platform.Name())
	}
	node := inc.Target.Node
	if inc.Target.GPUUUID != "" {
		if err := c.pinAcceleratorEvidenceForReset(ctx, inc); err != nil {
			return nil, fmt.Errorf("platform.quiesce_accelerator_stack: refusing to stop monitoring without reset-grade evidence: %w", err)
		}
	}
	quiesced, err := controller.QuiesceAcceleratorStack(ctx, node)
	if err != nil {
		return nil, err
	}
	// The node decides when the device is actually free. Inferring it from pod
	// labels was wrong on a real cluster: a device plugin that came from the
	// machine image rather than the GPU Operator carried different labels, so
	// the controller reported a settled stack while the plugin still held
	// /dev/nvidia0 — and the reset that followed was doomed before it started.
	// This step also releases the node-side holders no label can reach, chiefly
	// the persistence daemon.
	settle, err := c.executeAgentStep(ctx, inc, string(types.ActionQuiesceAcceleratorHost), step)
	if err != nil {
		return nil, fmt.Errorf("platform.quiesce_accelerator_stack: %w", err)
	}
	detail := strings.Join(quiesced, ", ")
	if detail == "" {
		detail = "nothing"
	}
	return okResult(inc, fmt.Sprintf("stopped %s on %s; %s", detail, node, settle.Output)), nil
}

// pinAcceleratorEvidenceForReset validates the live evidence exactly as the
// reset gate would, and pins it.
func (c *Controller) pinAcceleratorEvidenceForReset(ctx context.Context, inc *types.Incident) error {
	report, nodeUID, profile, err := c.acceleratorEvidenceForReset(ctx, inc.Target)
	if err != nil {
		return err
	}
	c.pinAcceleratorEvidence(inc.ID, pinnedAcceleratorEvidence{
		node:     inc.Target.Node,
		nodeUID:  nodeUID,
		profile:  profile,
		report:   *report,
		pinnedAt: time.Now(),
	})
	return nil
}

// restoreAcceleratorStackStep puts both halves back: the node's own holders
// through the agent, and the vendor components through the platform.
func (c *Controller) restoreAcceleratorStackStep(ctx context.Context, inc *types.Incident, step *playbook.Step) (*types.ActionResult, error) {
	node := inc.Target.Node
	hostResult, err := c.executeAgentStep(ctx, inc, string(types.ActionRestoreAcceleratorHost), step)
	if err != nil {
		return nil, fmt.Errorf("platform.restore_accelerator_stack: %w", err)
	}
	// Keep the platform's durable quiesce marker until the node's own state is
	// restored. If this action fails or the controller dies, recovery still finds
	// the marker and retries instead of silently leaving the host changed.
	restored, err := c.restoreAcceleratorStack(ctx, node)
	if err != nil {
		return nil, err
	}
	c.forgetPinnedAcceleratorEvidence(inc.ID)
	detail := strings.Join(restored, ", ")
	if detail == "" {
		detail = "nothing"
	}
	return okResult(inc, fmt.Sprintf("restored %s on %s; %s", detail, node, hostResult.Output)), nil
}

// restoreAcceleratorStack puts the vendor components back and drops the pin.
func (c *Controller) restoreAcceleratorStack(ctx context.Context, node string) ([]string, error) {
	controller, ok := c.platform.(platform.AcceleratorStackController)
	if !ok {
		return nil, fmt.Errorf("platform %q cannot control the accelerator stack", c.platform.Name())
	}
	return controller.RestoreAcceleratorStack(ctx, node)
}

// restoreAbandonedAcceleratorStacks puts monitoring back on any node whose
// incident is no longer running.
//
// A playbook that quiesces and then fails — a refused reset, a lost approval, a
// controller restart mid-run — must not leave the cluster's GPU monitoring
// switched off. This runs on the reconcile tick, so recovery does not depend on
// the failing playbook reaching a cleanup step.
func (c *Controller) restoreAbandonedAcceleratorStacks(ctx context.Context) {
	controller, ok := c.platform.(platform.AcceleratorStackController)
	if !ok {
		return
	}
	// Ask the platform which nodes are quiesced rather than trusting this
	// process's memory.
	//
	// Persisting the pinned evidence instead was the obvious alternative, and
	// it is the wrong one: it would make a snapshot taken before a crash
	// durable, and evidence that survived the process which validated it is
	// exactly what a fail-closed gate should not accept. Recovering the node
	// and re-running the quiesce re-attests from scratch, which is both safer
	// and simpler.
	nodes, err := controller.QuiescedNodes(ctx)
	if err != nil {
		c.log.Warn("listing quiesced nodes failed, will retry", "err", err)
		return
	}
	// ONE bounded wait budget for the whole pass, not per node: the janitor
	// runs on its own goroutine (round 8), but K quiesced nodes with dead
	// agents must still cost one budget, not K of them — an unbounded pass
	// starves the OTHER janitors sharing this goroutine and stretches
	// recovery latency for every node behind the stuck one. Healthy restores
	// are fast and share the budget; nodes the budget ran out on retry next
	// pass, where their already-enqueued actions re-attach.
	ctx, cancel := context.WithTimeout(ctx, acceleratorHostRestoreWait)
	defer cancel()
	owners := c.pinnedAcceleratorIncidentsByNode()
	for _, node := range nodes {
		incidentID, held := owners[node]
		if held {
			inc, err := c.store.GetIncident(ctx, incidentID)
			if err == nil && inc != nil && isActiveIncidentState(inc.State) {
				continue // a running playbook owns this quiesce
			}
		}
		if !held {
			// Quiesced with no pin behind it: this controller did not perform
			// it, or it restarted since. Either way nothing is driving it, and
			// any incident still on this node must start its quiesce again
			// rather than reset a node whose attestation is gone. Restoring is
			// only safe once every rewind has COMMITTED: the restore deletes
			// the durable marker, so a failed rewind must keep it and retry
			// next pass instead of leaving an un-rewound incident to reset a
			// restored stack.
			if !c.rewindIncidentsToQuiesce(ctx, node) {
				c.log.Warn("holding accelerator stack restore until every incident is rewound", "node", node)
				continue
			}
		}
		// The node's own state is the other half. Restoring only the
		// Kubernetes side would leave the persistence daemon stopped and
		// persistence mode off — a node quietly left in a state nobody chose,
		// with nothing left running to notice.
		if err := c.restoreAcceleratorHost(ctx, incidentID, node); err != nil {
			metrics.StackRestoreFailures.Inc()
			c.log.Warn("restoring node accelerator state failed, will retry",
				"node", node, "incident", incidentID, "err", err)
			c.reportStuckRestore(ctx, node, err)
			continue
		}
		restored, err := c.restoreAcceleratorStack(ctx, node)
		if err != nil {
			metrics.StackRestoreFailures.Inc()
			c.log.Warn("restoring accelerator stack failed, will retry",
				"node", node, "incident", incidentID, "err", err)
			c.reportStuckRestore(ctx, node, err)
			continue
		}
		c.forgetStuckRestore(node)
		if held {
			c.forgetPinnedAcceleratorEvidence(incidentID)
		}
		if len(restored) != 0 {
			c.log.Info("restored accelerator stack on a node no playbook is driving",
				"node", node, "incident", incidentID, "components", strings.Join(restored, ","))
		}
	}
}

// reportStuckRestore tells an operator ONCE per stuck node that its GPU
// monitoring is still down because the janitor's restore keeps failing —
// symmetric with reportStuckCordon. Repeating it every tick would train
// people to ignore it; a controller restart repeats it, which is the right
// way round. forgetStuckRestore clears the mark once a restore succeeds so a
// later relapse is reported again.
func (c *Controller) reportStuckRestore(ctx context.Context, node string, cause error) {
	if c.notifier == nil || !c.markCordonReported("stack-restore/"+node, "") {
		return
	}
	if err := c.notifier.Notify(ctx, notify.NotifyEvent{
		Kind: notify.EventNeedsHuman,
		// A synthetic incident carrier: notifiers render Incident fields, and
		// this condition is node-scoped rather than owned by any live incident.
		Incident: &types.Incident{
			ID:     "janitor/stack-restore/" + node,
			Target: types.Target{Node: node},
			State:  types.StateNeedsHuman,
		},
		Message: fmt.Sprintf(
			"node %s has its GPU monitoring stack quiesced and the automatic restore keeps failing (%v). "+
				"Monitoring stays DOWN on this node until the restore succeeds; check the node's agent.",
			node, cause),
	}); err != nil {
		c.log.Warn("stuck-restore notification failed", "node", node, "err", err)
	}
}

func (c *Controller) forgetStuckRestore(node string) {
	c.cordonReported.mu.Lock()
	defer c.cordonReported.mu.Unlock()
	delete(c.cordonReported.seen, "stack-restore/"+node+"/")
}

// restoreAcceleratorHost puts the node's own accelerator state back: the
// persistence daemon and persistence mode that the quiesce turned off.
//
// The action ID is derived from the node alone, not from a playbook step,
// because this runs when no step is executing (and, for an unowned quiesce,
// with no incident either). It stays deterministic so a retry re-attaches to
// the same queued action instead of stacking new ones — which is right WITHIN
// one restore attempt and wrong across them, so any finished row is discarded
// first. See the DiscardCompletedAction call below.
//
// Action.IncidentID is DELIBERATELY empty. The janitor acts precisely when no
// active incident owns the quiesce — the owner is halted or gone — and the
// terminal-incident claim guard refuses to hand an agent any action stamped
// with a halted incident's ID. Stamping the halted owner (a previous round's
// "provenance" fix) therefore made the restore permanently unclaimable: the
// node's monitoring stayed down and every reconcile tick burned the full
// bounded wait polling an action no agent would ever receive. Provenance
// belongs in params, where the claim guard does not read it.
//
// The wait bound comes from the CALLER's context: the janitor grants one
// shared acceleratorHostRestoreWait budget per pass across all nodes, so a
// dead agent cannot starve the other janitors on this goroutine or the other
// nodes behind it. agentrpc's enqueue is idempotent on the action ID, so the
// agent keeps executing after the janitor stops waiting and a later pass
// re-attaches to the same action and collects its stored result.
func (c *Controller) restoreAcceleratorHost(ctx context.Context, orphanedIncidentID, nodeName string) error {
	if c.actuator == nil {
		return nil
	}
	node, err := c.nodeFor(ctx, nodeName)
	if err != nil {
		return err
	}
	params := map[string]string{"host_state_key": nodeName}
	if orphanedIncidentID != "" {
		params["orphaned_incident"] = orphanedIncidentID
	}
	h := sha256.Sum256(fmt.Appendf(nil, "%s|restore-accelerator-host", nodeName))
	actionID := hex.EncodeToString(h[:8])

	// Discard a finished row under this ID before dispatching.
	//
	// The janitor only reaches this function while the node is STILL recorded
	// as quiesced, so a terminal row under this ID is stale or redundant, and
	// either way re-dispatching is correct because the restore is idempotent.
	//
	// Not "stale by construction", which an earlier version of this comment
	// claimed: the host restore does not clear the quiesce record.
	// RestoreAcceleratorStack does, it runs afterwards, and it can fail — so a
	// pass where the host half succeeded and the platform half did not leaves a
	// completed row on a still-quiesced node. Worth stating precisely, because
	// a later round will reason from it.
	//
	// Leaving the row in place meant the queue answered from history instead of
	// dispatching — enqueue is idempotent on the ID and terminal rows live for
	// the retention window, 90 days by default.
	//
	// Both directions were harmful. A restore that failed once returned that
	// stored failure to every later pass in zero milliseconds with no agent
	// involved, so a single transient hiccup left the node's persistence
	// daemon stopped permanently — and the stuck-restore report fires once per
	// node, so nobody was told twice. A restore that SUCCEEDED was worse: the
	// next time that node was quiesced and abandoned, the janitor replayed the
	// old success, cleared the durable marker on the strength of it, and left
	// the node's GPU monitoring off with nothing left to retry.
	// Called directly, not through a type assertion. store.Store declares the
	// method, so the assertion could never fail for a real store — but it was
	// silent by construction, and the controller's own fakes embed a nil
	// store.Store, which SATISFIES the assertion and panics rather than
	// skipping. It provided no protection while looking like it did; the
	// compiler provides the real thing.
	if err := c.store.DiscardCompletedAction(ctx, actionID); err != nil {
		return fmt.Errorf("clearing the previous restore attempt for %s: %w", nodeName, err)
	}

	result, err := c.actuator.Execute(ctx, node, types.Action{
		ID:      actionID,
		Type:    types.ActionRestoreAcceleratorHost,
		Params:  params,
		Timeout: acceleratorHostRestoreTimeout,
	})
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("restore_accelerator_host: %s", firstNonBlank(result.Error, "no detail"))
	}
	return nil
}

// acceleratorHostRestoreTimeout bounds the agent-side execution of one
// automatic host restore attempt. It is retried on the next reconcile tick, so
// a short bound is safe.
const acceleratorHostRestoreTimeout = 2 * time.Minute

// acceleratorHostRestoreWait bounds how long ONE janitor pass may spend
// waiting for restore results, across every quiesced node together. It is
// deliberately much shorter than the actions' execution timeout: the wait
// blocks the janitor goroutine (never the walk, since round 8), while the
// actions themselves keep running on their agents and are collected on later
// passes. A var only so tests can prove the bound without waiting it out.
var acceleratorHostRestoreWait = 30 * time.Second

// isActiveIncidentState reports whether an incident is still being driven.
// The inverse set is defined once, at the type level (IncidentState.Halted),
// shared with the store's claim guard.
func isActiveIncidentState(state types.IncidentState) bool {
	return !state.Halted()
}
