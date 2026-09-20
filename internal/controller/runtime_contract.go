package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/operations"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// inventoryLabels returns the labels a node carries right now, which is what
// profile selection must be judged against.
//
// The store's node row is written by agent registration (RegisterNode via
// UpsertAgentRegistration), which carries identity, heartbeat, and arming but
// never labels, and nothing else in the controller persists labels. Reading
// the row alone therefore made every selection Uncovered on a live Kubernetes
// cluster — no profile could ever be Exact and no qualification could ever be
// created — while every unit test, which seeds labels straight into the store,
// stayed green. The kind integration harness is what exposed it.
//
// Labels are platform-owned facts, so they are read from the Kubernetes Node
// object itself through the watch-maintained cache, exactly as the
// blast-radius confinement check does (see nodeLabelsForConfinement): never
// from the GPU-filtered inventory, which a node with a restarting device plugin
// or a CPU-only test node is absent from. A platform without that capability
// (bare metal, tests) keeps the stored labels. A resolved absence is an empty
// label set: a deleted Node object selects no profile. A platform error is an
// error, never "no labels": answering Uncovered on a blip would let a
// qualification observation record permanent drift on evidence nobody read.
func (c *Controller) inventoryLabels(ctx context.Context, node *types.Node) (map[string]string, error) {
	labeler, ok := c.platform.(platform.NodeLabeler)
	if !ok {
		return node.Labels, nil
	}
	labels, found, err := labeler.NodeLabels(ctx, node.Name)
	if err != nil {
		return nil, fmt.Errorf("%w: runtime contract coverage: load labels for node %q: %w", operations.ErrUnavailable, node.Name, err)
	}
	if !found {
		return map[string]string{}, nil
	}
	return labels, nil
}

// BuildRuntimeContractCoverage is the live, read-only adapter behind the
// runtime contract coverage API. It loads one current node, the exact
// (node, vendor) report retained by the accelerator report store, and the
// runtime configuration's profiles and digest, then hands them to the pure
// assessment in the config package. Nothing here writes, and the result never
// feeds an admission, incident, or autonomy decision.
//
// A missing report row is ordinary evidence: the assessment reports a Missing
// attestation. A store that cannot retain accelerator reports, or a store
// read that fails, is an error instead, because "no evidence available" must
// never be presented as an assessed node.
func (c *Controller) BuildRuntimeContractCoverage(ctx context.Context, nodeName string, vendor types.AcceleratorVendor) (config.RuntimeContractCoverage, error) {
	if c.store == nil {
		return config.RuntimeContractCoverage{}, fmt.Errorf("%w: runtime contract coverage: workflow store is unavailable", operations.ErrUnavailable)
	}
	if strings.TrimSpace(nodeName) == "" {
		return config.RuntimeContractCoverage{}, fmt.Errorf("runtime contract coverage: node is required")
	}
	if !vendor.Valid() {
		return config.RuntimeContractCoverage{}, fmt.Errorf("runtime contract coverage: unsupported vendor %q", vendor)
	}
	// Capability is checked before any read so an unsupported store answers
	// the same way for a known and an unknown node.
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return config.RuntimeContractCoverage{}, fmt.Errorf("%w: runtime contract coverage: accelerator report store is not configured", operations.ErrUnavailable)
	}
	node, err := c.store.GetNode(ctx, nodeName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return config.RuntimeContractCoverage{}, err
		}
		return config.RuntimeContractCoverage{}, fmt.Errorf("%w: runtime contract coverage: load node %q: %w", operations.ErrUnavailable, nodeName, err)
	}
	labels, err := c.inventoryLabels(ctx, node)
	if err != nil {
		return config.RuntimeContractCoverage{}, err
	}
	report, err := reports.GetAcceleratorReport(ctx, node.Name, vendor)
	switch {
	case errors.Is(err, store.ErrNotFound):
		report = nil
	case err != nil:
		return config.RuntimeContractCoverage{}, fmt.Errorf("%w: runtime contract coverage: load %s report for node %q: %w", operations.ErrUnavailable, vendor, nodeName, err)
	}
	rc := c.runtimeConfig(ctx)
	// ConfigDigest identifies the compiled configuration the profiles came
	// from. It is deliberately not substituted with a profile digest when the
	// deployment has no operator-compiled snapshot: the selected profile's own
	// digest already travels in the result under its own name.
	return config.AssessRuntimeContractCoverage(config.RuntimeContractCoverageInput{
		Now:           time.Now().UTC(),
		ConfigDigest:  rc.SourceDigest,
		NodeName:      node.Name,
		NodeUID:       node.UID,
		NodeLabels:    labels,
		Vendor:        vendor,
		AgentLastSeen: node.AgentLastSeen,
		AgentMaxAge:   verifyEvidenceMaxAge,
		Report:        report,
		Profiles:      rc.AcceleratorProfiles,
	}), nil
}
