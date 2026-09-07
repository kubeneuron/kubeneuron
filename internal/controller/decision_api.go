package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/operations"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// BuildDecisionSnapshot captures the controller-owned facts for a single
// decision.  It is deliberately the only live adapter used by the v0.4.0
// operations service: preview and simulation receive the resulting immutable
// value, never a pointer back into controller state.
func (c *Controller) BuildDecisionSnapshot(ctx context.Context, nodeName string, request decision.Request) (decision.Snapshot, error) {
	if c.store == nil {
		return decision.Snapshot{}, fmt.Errorf("decision snapshot: workflow store is unavailable")
	}
	node, err := c.store.GetNode(ctx, nodeName)
	if err != nil {
		return decision.Snapshot{}, err
	}
	now := time.Now().UTC()
	rc := c.runtimeConfig(ctx)
	snapshot := decision.Snapshot{
		Version:         decision.EvaluatorVersion,
		EvaluatedAt:     now,
		ConfigDigest:    rc.SourceDigest,
		Node:            cloneNode(node),
		Request:         request,
		AgentMaxAge:     verifyEvidenceMaxAge,
		GlobalPaused:    c.gate != nil && c.gate.Paused(),
		EmergencyStop:   c.gate != nil && c.gate.Paused(),
		ChangeFreeze:    node.Paused,
		EvidenceSources: []string{"controller"},
		EvidenceRefs: []decision.EvidenceRef{{
			Source:     "controller",
			ID:         "runtime-config/" + firstNonEmpty(rc.SourceDigest, "unknown"),
			ObservedAt: now,
			Digest:     evidenceReferenceDigest("controller", rc.SourceDigest, now.UTC().Format(time.RFC3339Nano)),
		}},
	}
	if !node.AgentLastSeen.IsZero() {
		snapshot.EvidenceSources = append(snapshot.EvidenceSources, "agent")
		snapshot.EvidenceRefs = append(snapshot.EvidenceRefs, decision.EvidenceRef{
			Source:     "agent",
			ID:         "heartbeat/" + firstNonEmpty(node.UID, node.Name),
			ObservedAt: node.AgentLastSeen.UTC(),
			Digest:     evidenceReferenceDigest("agent", node.Name, node.UID, node.AgentLastSeen.UTC().Format(time.RFC3339Nano), string(node.AgentArming)),
		})
	}
	if _, active := c.activeMaintenanceWindow(ctx, nodeName); active {
		snapshot.MaintenanceActive = true
	}
	if request.Class == decision.ActionRemediate || request.Class == decision.ActionAutonomous {
		selector := rc.DestructiveSelector
		if len(selector) > 0 && !labelsMatchSelector(selector, node.Labels) {
			snapshot.SelectorExcluded = true
		}
	}
	if request.Class != decision.ActionObserve {
		incidents, listErr := c.store.ListIncidents(ctx, store.IncidentFilter{Node: nodeName})
		if listErr != nil {
			return decision.Snapshot{}, fmt.Errorf("decision snapshot: list node incidents: %w", listErr)
		}
		for _, incident := range incidents {
			if incident == nil || incident.State.Terminal() {
				continue
			}
			snapshot.IncidentConflict = true
			if incident.RemediationSlotHeld {
				snapshot.OwnershipConflict = true
			}
		}
		// An active queue lease is a second, independent ownership signal: the
		// incident slot can be clear while an agent is still running a bounded
		// diagnostic or finishing a previous action. Stores that cannot inspect
		// live leases fail closed for an effect-capable request rather than
		// allowing a new owner to race an unknown executor.
		leases, ok := c.store.(store.ActiveActionLeaseInspector)
		if !ok {
			snapshot.OwnershipConflict = true
		} else {
			active, leaseErr := leases.HasActiveActionLease(ctx, nodeName)
			if leaseErr != nil {
				return decision.Snapshot{}, fmt.Errorf("decision snapshot: inspect node action lease: %w", leaseErr)
			}
			snapshot.OwnershipConflict = snapshot.OwnershipConflict || active
		}
	}

	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return snapshot, nil // evaluator turns unavailable evidence into Unknown.
	}
	allReports, err := reports.ListAcceleratorReports(ctx, nodeName)
	if err != nil {
		return decision.Snapshot{}, fmt.Errorf("decision snapshot: list accelerator reports: %w", err)
	}
	if len(allReports) == 0 {
		return snapshot, nil
	}
	sort.Slice(allReports, func(i, j int) bool { return allReports[i].Vendor < allReports[j].Vendor })
	selected := allReports[0]
	// A device reset currently has a defined NVIDIA runtime contract.  Prefer
	// the NVIDIA report when both vendors are present instead of basing the
	// answer on database order.
	if request.AcceleratorAction == types.AcceleratorActionResetDevice {
		for _, report := range allReports {
			if report.Vendor == types.AcceleratorVendorNVIDIA {
				selected = report
				break
			}
		}
	}
	snapshot.Report = selected
	if selected == nil || !selected.Vendor.Valid() {
		return snapshot, nil
	}
	if strings.HasPrefix(selected.RuntimeVersion, "dcgm-") {
		// The report's runtime version is accepted only after the agent's
		// bounded local DCGM probe, so this label means a current DCGM-backed
		// capability fact is actually present rather than merely configured.
		snapshot.EvidenceSources = append(snapshot.EvidenceSources, "dcgm")
		snapshot.EvidenceRefs = append(snapshot.EvidenceRefs, decision.EvidenceRef{
			Source:     "dcgm",
			ID:         "accelerator-report/" + nodeName + "/" + string(selected.Vendor),
			ObservedAt: selected.ObservedAt.UTC(),
			Digest:     evidenceReferenceDigest("dcgm", nodeName, string(selected.Vendor), selected.RuntimeVersion, selected.ObservedAt.UTC().Format(time.RFC3339Nano)),
		})
	}
	profile, err := (config.Config{AcceleratorProfiles: rc.AcceleratorProfiles}).ResolveAcceleratorRuntimeProfile(node.Labels, selected.Vendor)
	if err == nil {
		snapshot.Profile = profile
		if snapshot.ConfigDigest == "" {
			snapshot.ConfigDigest = decision.ConfigDigest(profile)
		}
		return snapshot, nil
	}
	if errors.Is(err, config.ErrNoAcceleratorRuntimeProfile) || errors.Is(err, config.ErrAmbiguousAcceleratorRuntimeProfile) {
		// A missing/ambiguous selection is represented to the evaluator as an
		// observation-only profile mismatch; it never selects arbitrarily.
		return snapshot, nil
	}
	return decision.Snapshot{}, fmt.Errorf("decision snapshot: select runtime profile: %w", err)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// evidenceReferenceDigest avoids putting mutable, raw evidence in an API
// response while still giving an operator a stable integrity handle for each
// source fact captured into a DecisionSnapshot.
func evidenceReferenceDigest(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(hash[:])
}

// LiveAdmissionDecision is the compatibility seam between the established
// remediation state machine and v0.4.0's shared evaluator. Every live
// accelerator admission captures the same immutable input shape used by
// preview/simulation/autonomy. The existing fine-grained step gates remain
// authoritative during the v0.4 compatibility window; their outcome is
// compared and logged rather than silently changing a running fleet's
// historical admission behavior.
func (c *Controller) LiveAdmissionDecision(ctx context.Context, nodeName string, request decision.Request) (decision.Result, error) {
	snapshot, err := c.BuildDecisionSnapshot(ctx, nodeName, request)
	if err != nil {
		return decision.Result{}, err
	}
	started := time.Now()
	result := decision.Evaluate(snapshot)
	metrics.DecisionEvaluationSeconds.WithLabelValues("live-admission").Observe(time.Since(started).Seconds())
	metrics.DecisionEvaluations.WithLabelValues("live-admission", string(result.State)).Inc()
	for _, reason := range result.ReasonCodes {
		switch reason {
		case decision.ReasonEvidenceStale, decision.ReasonNoHealthyAgent, decision.ReasonEvidenceSourceMissing:
			metrics.DecisionEvidenceStale.WithLabelValues("live-admission", string(reason)).Inc()
		}
	}
	return result, nil
}

// cloneNode avoids aliases to mutable inventory maps/slices in a persisted
// snapshot.
func cloneNode(node *types.Node) types.Node {
	if node == nil {
		return types.Node{}
	}
	out := *node
	if node.Labels != nil {
		out.Labels = make(map[string]string, len(node.Labels))
		for key, value := range node.Labels {
			out.Labels[key] = value
		}
	}
	out.GPUs = append([]types.GPUInfo(nil), node.GPUs...)
	return out
}

// Operations returns the durable v0.4.0 service when the configured store
// supports it.  The HTTP adapter uses this narrow accessor to fail closed on
// unsupported deployments.
func (c *Controller) Operations() *operations.Manager { return c.operations }

// CreateIncidentFromSimulation is the explicit bridge from a frozen
// simulation to the existing incident state machine.  It persists through the
// same transaction/audit path as a manual signal, then returns the durable
// incident it opened or attached to.
func (c *Controller) CreateIncidentFromSimulation(ctx context.Context, signal types.Signal) (*types.Incident, error) {
	if err := c.IngestSignal(ctx, signal); err != nil {
		return nil, err
	}
	incident, err := c.store.GetOpenIncident(ctx, signal.Target, signal.Class)
	if err != nil {
		return nil, err
	}
	return incident, nil
}
