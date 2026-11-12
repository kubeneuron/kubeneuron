// SPDX-License-Identifier: LicenseRef-KubeNeuron

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/operations"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/topology"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// This file is the live adapter for the pure device topology classifier
// (internal/topology). It captures the controller-owned facts — the node's
// advertised resources and labels from the platform, the retained NVIDIA
// accelerator report from the store, the opt-in DRA observation — into one
// immutable snapshot and hands it to topology.Classify. Two callers share
// it: the read-only readiness/topology API, and the NVIDIA reset gate, which
// uses the result ONLY to add a refusal after the legacy evidence test has
// already passed (admission.go).

// topologySnapshot captures the classifier inputs for one node. The report
// is supplied by the caller: the reset gate hands over the exact report the
// legacy gate validated (possibly the one pinned by a quiesce step), and the
// readiness path hands over the retained live report. A nil report is the
// "missing" fact, which the classifier refuses.
func (c *Controller) topologySnapshot(ctx context.Context, node *types.Node, labels map[string]string, report *types.AgentAcceleratorReport, now time.Time) topology.Snapshot {
	rc := c.runtimeConfig(ctx)
	snapshot := topology.Snapshot{
		Node:        node.Name,
		NodeUID:     node.UID,
		EvaluatedAt: now,
		Labels:      labels,
		Report:      topology.ReportFacts(report),
		Resources:   c.topologyResources(ctx, node.Name),
		DRA:         topology.DRAObservation{State: topology.DRADisabled},
	}
	// Freshness is judged by the SAME bound the reset gate uses when a profile
	// selects the node; without one the classifier's default applies. The
	// classifier can only shorten what the profile allows, never extend it.
	if profile, err := (config.Config{AcceleratorProfiles: rc.AcceleratorProfiles}).
		ResolveAcceleratorRuntimeProfile(labels, types.AcceleratorVendorNVIDIA); err == nil && profile != nil {
		snapshot.MaxReportAge = profile.MaxReportAge.Std()
	}
	if rc.Topology.DRADiscovery {
		snapshot.DRA = c.observeDRA(ctx, node.Name)
	}
	return snapshot
}

// topologyResources reads the node's advertised resources. Fail closed: a
// platform that does not implement the observer (nil, a bare platform, a
// test double) is recorded as unobserved, which the classifier refuses. The
// absence of an interface is not evidence that the platform has no
// device-plugin advertisement; only an observer that explicitly answers
// topology.ResourceSourceNotApplicable says so, and then the agent's
// attestation is the whole of the evidence. A Kubernetes platform always
// implements the observer (compile-time pinned) and always answers with the
// node's own advertisement, so on Kubernetes an unreadable or absent node is
// unobserved, and unobserved refuses. Node-scoped actions never reach here.
func (c *Controller) topologyResources(ctx context.Context, node string) topology.Resources {
	if c.platform == nil {
		return topology.Resources{}
	}
	observer, ok := c.platform.(platform.NodeResourceObserver)
	if !ok {
		return topology.Resources{}
	}
	resources, found, err := observer.NodeResources(ctx, node)
	if err != nil {
		c.log.Debug("topology: node resources unavailable", "node", node, "err", err)
		return topology.Resources{}
	}
	if !found {
		return topology.Resources{}
	}
	switch resources.Source {
	case topology.ResourceSourceNode, topology.ResourceSourceNotApplicable:
		return resources
	default:
		// An observer that answers without saying where from, or from a
		// source this version does not know, has not observed.
		return topology.Resources{}
	}
}

// observeDRA takes the opt-in read (the node's ResourceSlices and the claim
// references of its Pods). It is reached only when the runtime configuration
// enabled discovery; a platform that cannot observe reports not-observed,
// which the classifier refuses, and an observed answer that did not inspect
// Pods is rejected by the classifier as invalid.
func (c *Controller) observeDRA(ctx context.Context, node string) topology.DRAObservation {
	observer, ok := c.platform.(platform.DRAObserver)
	if !ok || c.platform == nil {
		return topology.DRAObservation{State: topology.DRANotObserved, Reason: "platform-cannot-observe"}
	}
	observation := observer.ObserveDRA(ctx, node)
	if observation.State == "" {
		observation.State = topology.DRANotObserved
		if observation.Reason == "" {
			observation.Reason = "unspecified"
		}
	}
	return observation
}

// classifyTopology runs the pure classifier and records the closed-label
// counter. It is the one metrics seam for the classifier so the pure package
// stays deterministic.
func classifyTopology(snapshot topology.Snapshot) topology.Classification {
	classification := topology.Classify(snapshot)
	metrics.TopologyClassifications.WithLabelValues(string(classification.Topology), string(classification.DeviceActions)).Inc()
	return classification
}

// BuildTopologyClassification is the read-only adapter behind the node
// topology API and the topology field of node readiness. It never writes and
// never changes what the controller admits; it explains the evidence.
//
// A node unknown to the inventory is store.ErrNotFound (404). A missing NVIDIA
// report (a report store answering store.ErrNotFound) is ordinary evidence
// (ReportMissing). A store that cannot retain reports at all, or a failed
// store read, is operations.ErrUnavailable so it can never be mistaken for an
// assessed node: "no retained report" and "this store cannot retain reports"
// are different facts, and only the first is a classification.
func (c *Controller) BuildTopologyClassification(ctx context.Context, nodeName string) (topology.Classification, error) {
	if c.store == nil {
		return topology.Classification{}, fmt.Errorf("%w: topology: workflow store is unavailable", operations.ErrUnavailable)
	}
	if strings.TrimSpace(nodeName) == "" {
		return topology.Classification{}, fmt.Errorf("topology: node is required")
	}
	// Capability is checked before any read, as in BuildRuntimeContractCoverage,
	// so an unsupported store answers the same way for a known and an unknown
	// node and the failure cannot be mistaken for a per-node condition.
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return topology.Classification{}, fmt.Errorf("%w: topology: accelerator report store is not configured", operations.ErrUnavailable)
	}
	node, err := c.store.GetNode(ctx, nodeName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return topology.Classification{}, err
		}
		return topology.Classification{}, fmt.Errorf("%w: topology: load node %q: %w", operations.ErrUnavailable, nodeName, err)
	}
	labels, err := c.inventoryLabels(ctx, node)
	if err != nil {
		return topology.Classification{}, err
	}
	report, err := reports.GetAcceleratorReport(ctx, node.Name, types.AcceleratorVendorNVIDIA)
	switch {
	case errors.Is(err, store.ErrNotFound):
		report = nil
	case err != nil:
		return topology.Classification{}, fmt.Errorf("%w: topology: load nvidia report for node %q: %w", operations.ErrUnavailable, nodeName, err)
	}
	return classifyTopology(c.topologySnapshot(ctx, node, labels, report, time.Now().UTC())), nil
}

// errTopologyRefused wraps a classifier refusal on the reset path so callers
// and tests can tell it from a legacy evidence failure.
var errTopologyRefused = errors.New("device topology refuses a device-scoped reset")

// refuseUnqualifiedResetTopology resolves the target node the same way the
// legacy reset authority does (the live platform inventory first, the store
// only without a platform) and applies the narrowing step to it.
func (c *Controller) refuseUnqualifiedResetTopology(ctx context.Context, target types.Target, report *types.AgentAcceleratorReport) error {
	node, ok := c.acceleratorNode(ctx, target.Node)
	if !ok || node == nil {
		return fmt.Errorf("%w: node %q is not in the inventory", errTopologyRefused, target.Node)
	}
	return c.refuseUnqualifiedTopology(ctx, node, node.Labels, report)
}

// refuseUnqualifiedTopology is the reset gate's narrowing step. It is called
// only after the legacy evidence test passed, with the very report that
// passed it, and it can only turn that pass into a refusal. The returned
// error carries the stable reason codes and the inputs digest so the
// incident's record explains the refusal without exposing raw evidence.
func (c *Controller) refuseUnqualifiedTopology(ctx context.Context, node *types.Node, labels map[string]string, report *types.AgentAcceleratorReport) error {
	classification := classifyTopology(c.topologySnapshot(ctx, node, labels, report, time.Now().UTC()))
	if classification.Eligible() {
		return nil
	}
	metrics.DestructiveStepsDeferred.WithLabelValues(metrics.DeferTopologyUnqualified).Inc()
	codes := make([]string, 0, len(classification.Reasons))
	for _, code := range classification.Reasons {
		codes = append(codes, string(code))
	}
	return fmt.Errorf("%w: topology=%s reasons=%s inputs_digest=%s",
		errTopologyRefused, classification.Topology, strings.Join(codes, ","), classification.InputsDigest)
}
