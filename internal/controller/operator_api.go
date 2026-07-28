package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/kubeneuron/kubeneuron/internal/approval"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// This file implements httpapi.OperatorBackend: the read and decision
// surface behind the authenticated operator API.

// ListIncidents returns incidents filtered by state names and node.
func (c *Controller) ListIncidents(ctx context.Context, states []string, node string, limit int) ([]*types.Incident, error) {
	f := store.IncidentFilter{Node: node, Limit: limit}
	for _, s := range states {
		f.States = append(f.States, types.IncidentState(s))
	}
	return c.store.ListIncidents(ctx, f)
}

// IncidentDetail returns one incident with its audit trail.
func (c *Controller) IncidentDetail(ctx context.Context, id string) (*types.Incident, []*types.AuditEntry, error) {
	inc, err := c.store.GetIncident(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	trail, err := c.store.AuditTrail(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return inc, trail, nil
}

// DecideApproval records an authenticated human decision; the reconcile walk
// picks it up on its next pass.
func (c *Controller) DecideApproval(ctx context.Context, id, actor, channel string, decision types.ApprovalDecision) error {
	return approval.New(c.store, c.approvalTTL).Decide(ctx, id, "", actor, channel, decision)
}

// ResolveIncident manually resolves an incident (typically from
// NEEDS_HUMAN after out-of-band repair). The transition validator rejects
// states that must not be short-circuited, such as EXECUTING.
func (c *Controller) ResolveIncident(ctx context.Context, id, actor, reason string) error {
	inc, err := c.store.GetIncident(ctx, id)
	if err != nil {
		return err
	}
	if err := c.transition(ctx, inc, types.StateResolved, actor, "manual-resolve",
		firstNonBlank(reason, "manually resolved"), nil); err != nil {
		return err
	}
	return c.notifier.Notify(ctx, notify.NotifyEvent{
		Kind: notify.EventResolved, Incident: inc,
		Message: fmt.Sprintf("manually resolved by %s: %s", actor, firstNonBlank(reason, "no reason given")),
	})
}

// Nodes lists the persisted inventory.
func (c *Controller) Nodes(ctx context.Context) ([]*types.Node, error) {
	return c.store.ListNodes(ctx)
}

// Node returns one inventory record.
func (c *Controller) Node(ctx context.Context, name string) (*types.Node, error) {
	return c.store.GetNode(ctx, name)
}

// AcceleratorReports returns the latest retained report for each accelerator
// vendor on one known node. It is read-only operational evidence: exposing a
// ready report never changes execution mode or grants an action capability.
func (c *Controller) AcceleratorReports(ctx context.Context, node string) ([]*types.AgentAcceleratorReport, error) {
	if _, err := c.store.GetNode(ctx, node); err != nil {
		return nil, err
	}
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return nil, fmt.Errorf("accelerator report store is not configured")
	}
	return reports.ListAcceleratorReports(ctx, node)
}

// AcceleratorObservationProfile returns only the immutable digest that the
// configured runtime profile selects for an authenticated node and vendor.
// It is an observation-binding API, not an action authorization API: a nil
// result is the normal default-deny outcome when no profile selects the node.
// Ambiguous selectors, invalid configuration, and unavailable node labels are
// errors so the HTTP layer can fail closed rather than choose an arbitrary
// profile.
func (c *Controller) AcceleratorObservationProfile(ctx context.Context, node string, vendor types.AcceleratorVendor) (*types.AgentAcceleratorObservationProfile, error) {
	if node == "" || !vendor.Valid() {
		return nil, fmt.Errorf("accelerator observation profile requires node and supported vendor")
	}
	labels := c.nodeLabels(ctx, node)
	if labels == nil {
		return nil, fmt.Errorf("node labels are unavailable for runtime profile selection")
	}
	c.acceleratorProfilesMu.RLock()
	profiles := append([]config.AcceleratorRuntimeProfile(nil), c.acceleratorProfiles...)
	c.acceleratorProfilesMu.RUnlock()
	profile, err := (config.Config{AcceleratorProfiles: profiles}).ResolveAcceleratorRuntimeProfile(labels, vendor)
	if errors.Is(err, config.ErrNoAcceleratorRuntimeProfile) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &types.AgentAcceleratorObservationProfile{
		Vendor:            profile.Vendor,
		ProfileDigest:     profile.ProfileDigest,
		ProfileUID:        profile.ProfileUID,
		ProfileGeneration: profile.ProfileGeneration,
		RuntimeVersion:    profile.RuntimeVersion,
	}, nil
}

// SetPaused flips the global automation pause (the big red button).
func (c *Controller) SetPaused(paused bool, actor string) {
	if paused {
		c.gate.Pause()
	} else {
		c.gate.Resume()
	}
	c.log.Warn("automation pause changed", "paused", paused, "actor", actor)
}

// Paused reports the global pause state.
func (c *Controller) Paused() bool { return c.gate.Paused() }
