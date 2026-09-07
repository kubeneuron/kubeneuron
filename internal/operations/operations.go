// Package operations implements the durable product workflows introduced by
// v0.4.0: explainable readiness, candidate policy impact previews, bounded
// diagnostics, and no-side-effect remediation simulations.
//
// The package does not know how to read Kubernetes or execute an action.  Its
// caller supplies captured decision snapshots and the existing durable action
// queue.  That separation keeps parsing/preview code away from cluster write
// credentials and makes every workflow testable from fixtures.
package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	// CompilerVersion belongs in every candidate and preview, because a
	// compiler change can alter how an otherwise identical YAML bundle is
	// normalized or validated.
	CompilerVersion              = "v1"
	maxCandidateBytes            = 1 << 20
	defaultCandidateTTL          = 24 * time.Hour
	maxQuickDiagnosticTimeout    = 10 * time.Minute
	maxExtendedDiagnosticTimeout = 30 * time.Minute
	maxFleetActiveDiagnostics    = 10
	maxNodeActiveDiagnostics     = 1
)

var (
	ErrUnavailable         = errors.New("operations: durable workflow store is unavailable")
	ErrIncompleteInventory = errors.New("operations: inventory snapshot is incomplete or stale")
	ErrIdempotencyConflict = errors.New("operations: idempotency key was reused for a different request")
	ErrInvalidState        = errors.New("operations: invalid lifecycle transition")
)

// SnapshotBuilder captures all mutable evidence required for one decision.
// It must return a fully self-contained Snapshot; operations never falls back
// to a live read after it receives one.
type SnapshotBuilder func(context.Context, string, decision.Request) (decision.Snapshot, error)

// NodeLister provides the currently managed fleet.  The manager sorts its
// answer itself so preview output does not depend on informer/map order.
type NodeLister func(context.Context) ([]*types.Node, error)

// IncidentCreator turns an approved, frozen simulation into the existing
// durable remediation incident workflow.  It is deliberately an injected
// callback: simulations cannot reach a controller by accident.
type IncidentCreator func(context.Context, types.Signal) (*types.Incident, error)

// IncidentReader lets an idempotent simulation-to-incident request return the
// original incident without re-ingesting a signal on replay.
type IncidentReader func(context.Context, string) (*types.Incident, error)

// AutonomyMaintenanceWindowResolver answers whether one of a plan's named
// maintenance windows currently covers a node. It is injected because window
// names and selector evaluation belong to the controller's live configuration,
// not to a persisted autonomy plan.
type AutonomyMaintenanceWindowResolver func(context.Context, string, []string) (bool, error)

// AutonomyEffectExecutor is the explicit hardware-qualified seam for a
// GPUAutonomyPlan. The manager invokes it only after persisting a fresh,
// evaluator-approved effect record. Implementations MUST treat EffectID as an
// idempotency key: after a controller restart the same admitted effect can be
// presented again, but it must never turn into a second device action.
//
// The stock controller intentionally supplies no executor. That keeps new
// installations in observable simulation-only rollout mode until a deployment
// wires a qualified adapter for its hardware and compensation model.
type AutonomyEffectExecutor func(context.Context, AutonomyEffectRequest) (AutonomyEffectResult, error)

// Manager owns only durable product resources.  The workflow store is also
// the existing controller action queue, which lets quick/extended diagnostics
// retain lease and emergency-stop semantics rather than inventing a second
// dispatcher.
type Manager struct {
	resources                       store.OperationalStore
	workflow                        store.Store
	buildSnapshot                   SnapshotBuilder
	listNodes                       NodeLister
	createIncident                  IncidentCreator
	getIncident                     IncidentReader
	autonomyExecute                 AutonomyEffectExecutor
	autonomyMaintenanceWindowActive AutonomyMaintenanceWindowResolver
	// diagnosticMu serializes the check-and-admit portion of a diagnostic
	// request in the elected controller. The durable queue remains the source
	// of truth across restart; this narrow mutex closes the otherwise ordinary
	// same-process race where two requests both see the final fleet slot free.
	diagnosticMu sync.Mutex
	now          func() time.Time
}

// Options wires a Manager.  No nil dependency is silently replaced with an
// in-memory fake: writes fail closed when durable backing is unavailable.
type Options struct {
	Resources                       store.OperationalStore
	Workflow                        store.Store
	BuildSnapshot                   SnapshotBuilder
	ListNodes                       NodeLister
	CreateIncident                  IncidentCreator
	GetIncident                     IncidentReader
	AutonomyEffectExecutor          AutonomyEffectExecutor
	AutonomyMaintenanceWindowActive AutonomyMaintenanceWindowResolver
	Now                             func() time.Time
}

func New(options Options) *Manager {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{
		resources:                       options.Resources,
		workflow:                        options.Workflow,
		buildSnapshot:                   options.BuildSnapshot,
		listNodes:                       options.ListNodes,
		createIncident:                  options.CreateIncident,
		getIncident:                     options.GetIncident,
		autonomyExecute:                 options.AutonomyEffectExecutor,
		autonomyMaintenanceWindowActive: options.AutonomyMaintenanceWindowActive,
		now:                             now,
	}
}

func (m *Manager) requireResources() error {
	if m == nil || m.resources == nil {
		return ErrUnavailable
	}
	return nil
}

func (m *Manager) requireSnapshots() error {
	if err := m.requireResources(); err != nil {
		return err
	}
	if m.buildSnapshot == nil {
		return fmt.Errorf("%w: decision snapshot builder is unavailable", ErrUnavailable)
	}
	return nil
}

// Readiness is the read-only explanation returned to fleet/node callers.  A
// state-changing workflow captures the same structure into a DecisionSnapshot
// resource; GET readiness never creates surprising writes.
type Readiness struct {
	Node     string            `json:"node"`
	Snapshot decision.Snapshot `json:"snapshot"`
	Decision decision.Result   `json:"decision"`
}

// ReadinessFilter is the fleet explorer's stable, read-only filter. Tenant
// and cluster are read from conventional node labels
// (kubeneuron.io/tenant and kubeneuron.io/cluster) because the historical
// node inventory predates multi-tenant envelope fields; a deployment that
// does not attach those labels simply has no matching scoped nodes.
type ReadinessFilter struct {
	State     decision.State
	Vendor    types.AcceleratorVendor
	Profile   string
	Tenant    string
	Cluster   string
	StaleOnly bool
	Limit     int
	AfterNode string
}

// OperationalListOptions is the common list/query contract for durable
// v0.4.0 resources. The (created_at,id) cursor is deliberately represented as
// a pair internally; HTTP turns it into an opaque value, so callers cannot
// accidentally depend on a database timestamp format.
type OperationalListOptions struct {
	State          string
	Tenant         string
	Cluster        string
	Limit          int
	CreatedSince   time.Time
	CreatedUntil   time.Time
	AfterCreatedAt time.Time
	AfterID        string
	IncludeExpired bool
}

func (m *Manager) listOperationalResources(ctx context.Context, kind types.OperationalResourceKind, options OperationalListOptions) ([]*types.OperationalResource, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	if options.Limit <= 0 {
		options.Limit = 100
	}
	return m.resources.ListOperationalResources(ctx, types.OperationalResourceFilter{
		Kind: kind, State: options.State, Tenant: options.Tenant, Cluster: options.Cluster,
		Limit: options.Limit, CreatedSince: options.CreatedSince, CreatedUntil: options.CreatedUntil,
		AfterCreatedAt: options.AfterCreatedAt, AfterID: options.AfterID, IncludeExpired: options.IncludeExpired,
	})
}

// NodeReadiness evaluates one current snapshot without persisting it.
func (m *Manager) NodeReadiness(ctx context.Context, node string) (*Readiness, error) {
	if err := m.requireSnapshots(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(node) == "" {
		return nil, fmt.Errorf("node is required")
	}
	snapshot, err := m.buildSnapshot(ctx, node, decision.Request{Class: decision.ActionObserve})
	if err != nil {
		return nil, err
	}
	return &Readiness{Node: node, Snapshot: snapshot, Decision: m.evaluateDecision("readiness", snapshot)}, nil
}

// evaluateDecision is the sole metrics seam around the pure evaluator. It
// does not feed metrics back into the result, so all adapters remain
// deterministic for the same frozen Snapshot.
func (m *Manager) evaluateDecision(adapter string, snapshot decision.Snapshot) decision.Result {
	started := time.Now()
	result := decision.Evaluate(snapshot)
	metrics.DecisionEvaluationSeconds.WithLabelValues(adapter).Observe(time.Since(started).Seconds())
	metrics.DecisionEvaluations.WithLabelValues(adapter, string(result.State)).Inc()
	for _, reason := range result.ReasonCodes {
		switch reason {
		case decision.ReasonEvidenceStale, decision.ReasonNoHealthyAgent, decision.ReasonEvidenceSourceMissing:
			metrics.DecisionEvidenceStale.WithLabelValues(adapter, string(reason)).Inc()
		}
	}
	return result
}

// FleetReadiness evaluates each supplied node in stable name order.  A node
// whose snapshot cannot be captured is represented as Unknown only when the
// builder returned a usable snapshot; a hard capture error remains visible to
// the caller instead of becoming a misleading healthy fleet count.
func (m *Manager) FleetReadiness(ctx context.Context) ([]Readiness, error) {
	// Keep the legacy all-fleet helper genuinely all-fleet.  The HTTP path is
	// deliberately page-bounded, but callers such as an internal aggregate
	// must not silently lose node 501 just because the public page cap exists.
	// Reusing the same page routine keeps its sorting and evaluator semantics
	// identical to REST while avoiding a second fleet walk implementation.
	var out []Readiness
	after := ""
	for {
		items, next, err := m.FleetReadinessPage(ctx, ReadinessFilter{Limit: 500, AfterNode: after})
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" {
			return out, nil
		}
		if next <= after {
			return nil, fmt.Errorf("%w: readiness cursor did not advance", ErrIncompleteInventory)
		}
		after = next
	}
}

// FleetReadinessPage evaluates a stable name-ordered fleet page. It computes
// one snapshot per included node and returns the final node name as an
// internal cursor; the HTTP layer makes that cursor opaque. An incomplete
// capture remains an error rather than being silently counted as healthy.
func (m *Manager) FleetReadinessPage(ctx context.Context, filter ReadinessFilter) ([]Readiness, string, error) {
	if err := m.requireSnapshots(); err != nil {
		return nil, "", err
	}
	if m.listNodes == nil {
		return nil, "", fmt.Errorf("%w: node inventory lister is unavailable", ErrUnavailable)
	}
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	if filter.Limit > 500 {
		return nil, "", fmt.Errorf("readiness limit exceeds 500")
	}
	nodes, err := m.listNodes(ctx)
	if err != nil {
		return nil, "", err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	out := make([]Readiness, 0, min(filter.Limit, len(nodes)))
	for _, node := range nodes {
		if node == nil || strings.TrimSpace(node.Name) == "" {
			return nil, "", fmt.Errorf("%w: inventory contains a node without a name", ErrIncompleteInventory)
		}
		if filter.AfterNode != "" && node.Name <= filter.AfterNode {
			continue
		}
		if !readinessScopeMatches(node, filter) {
			continue
		}
		item, err := m.NodeReadiness(ctx, node.Name)
		if err != nil {
			return nil, "", fmt.Errorf("readiness for node %q: %w", node.Name, err)
		}
		if filter.State != "" && item.Decision.State != filter.State {
			continue
		}
		if filter.Vendor != "" && (item.Snapshot.Report == nil || item.Snapshot.Report.Vendor != filter.Vendor) {
			continue
		}
		if filter.Profile != "" && (item.Snapshot.Profile == nil || item.Snapshot.Profile.Name != filter.Profile) {
			continue
		}
		if filter.StaleOnly && !decisionIsStale(item.Decision) {
			continue
		}
		out = append(out, *item)
		if len(out) == filter.Limit+1 {
			// The final item merely proves there is another page. Do not return
			// it; the last returned name is the resume cursor.
			out = out[:filter.Limit]
			return out, out[len(out)-1].Node, nil
		}
	}
	return out, "", nil
}

func readinessScopeMatches(node *types.Node, filter ReadinessFilter) bool {
	return operationalNodeScopeMatches(node, filter.Tenant, filter.Cluster)
}

// operationalNodeScopeMatches is the common tenant/cluster boundary for
// durable v0.4.0 resources. Scope comes from controller-owned node labels,
// never from a client-supplied resource alone: otherwise a request could label
// itself as tenant A while evaluating a tenant B node.
func operationalNodeScopeMatches(node *types.Node, tenant, cluster string) bool {
	if node == nil {
		return false
	}
	tenant, cluster = strings.TrimSpace(tenant), strings.TrimSpace(cluster)
	if tenant != "" && node.Labels["kubeneuron.io/tenant"] != tenant {
		return false
	}
	if cluster != "" && node.Labels["kubeneuron.io/cluster"] != cluster {
		return false
	}
	return true
}

func requireSnapshotScope(snapshot decision.Snapshot, tenant, cluster string) error {
	if !operationalNodeScopeMatches(&snapshot.Node, tenant, cluster) {
		return fmt.Errorf("%w: node %q is outside requested tenant/cluster scope", ErrInvalidState, snapshot.Node.Name)
	}
	return nil
}

// snapshotNodeScope copies controller-owned tenant/cluster labels into a
// durable evidence envelope. Captures use labels rather than request fields so
// a caller cannot relabel a cross-tenant node in an otherwise valid payload.
func snapshotNodeScope(snapshot decision.Snapshot) (string, string) {
	return strings.TrimSpace(snapshot.Node.Labels["kubeneuron.io/tenant"]), strings.TrimSpace(snapshot.Node.Labels["kubeneuron.io/cluster"])
}

func decisionIsStale(result decision.Result) bool {
	for _, reason := range result.ReasonCodes {
		if reason == decision.ReasonEvidenceStale || reason == decision.ReasonNoHealthyAgent || reason == decision.ReasonEvidenceSourceMissing {
			return true
		}
	}
	return false
}

// DecisionSnapshot is the immutable persisted form used by preview,
// simulation, incident creation, and autonomy.  The payload includes both
// inputs and the evaluator answer so historical replay does not read current
// policy or telemetry.
type DecisionSnapshot struct {
	ID        string            `json:"id"`
	Tenant    string            `json:"tenant,omitempty"`
	Cluster   string            `json:"cluster,omitempty"`
	Snapshot  decision.Snapshot `json:"snapshot"`
	Decision  decision.Result   `json:"decision"`
	CreatedAt time.Time         `json:"created_at"`
}

func (m *Manager) captureDecision(ctx context.Context, actor string, snapshot decision.Snapshot, requestID string) (*DecisionSnapshot, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	now := m.now().UTC()
	if snapshot.EvaluatedAt.IsZero() {
		snapshot.EvaluatedAt = now
	}
	tenant, cluster := snapshotNodeScope(snapshot)
	captured := &DecisionSnapshot{
		ID:        newID("decision"),
		Tenant:    tenant,
		Cluster:   cluster,
		Snapshot:  snapshot,
		Decision:  m.evaluateDecision("snapshot", snapshot),
		CreatedAt: now,
	}
	payload, err := marshalPayload(captured)
	if err != nil {
		return nil, err
	}
	expires := captured.Decision.ExpiresAt.UTC()
	if expires.IsZero() {
		// A malformed/unknown snapshot is never authority, but it still needs
		// a retention anchor so a stream of failed captures cannot outlive the
		// configured operational-data window forever.
		expires = now
	}
	if err := m.resources.CreateOperationalResource(ctx, &types.OperationalResource{
		Kind: types.ResourceDecisionSnapshot, ID: captured.ID, State: "captured", Actor: actor,
		Tenant: tenant, Cluster: cluster, ConfigDigest: captured.Decision.ConfigDigest,
		Payload: payload, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires,
	}); err != nil {
		return nil, err
	}
	if err := m.audit(ctx, types.ResourceDecisionSnapshot, captured.ID, actor, "capture", requestID, captured.ID, nil, captured.Decision.HumanSummary); err != nil {
		return nil, err
	}
	return captured, nil
}

// CandidateUpload is the API input for a candidate configuration.  Content is
// YAML or JSON (JSON is valid YAML); callers must supply an idempotency key for
// all write APIs.
type CandidateUpload struct {
	Content        []byte    `json:"-"`
	Tenant         string    `json:"tenant,omitempty"`
	Cluster        string    `json:"cluster,omitempty"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
	IdempotencyKey string    `json:"-"`
}

// CandidateConfiguration is an immutable validated compiler input.  Original
// bytes are not returned by default; Normalized is strict canonical JSON that
// can be replayed without YAML aliases/comments changing semantics.
type CandidateConfiguration struct {
	ID               string                             `json:"id"`
	ResourceVersion  int                                `json:"resource_version"`
	State            string                             `json:"state"`
	APIVersion       string                             `json:"api_version"`
	Kind             string                             `json:"kind"`
	OriginalDigest   string                             `json:"original_digest"`
	NormalizedDigest string                             `json:"normalized_digest"`
	Normalized       json.RawMessage                    `json:"normalized"`
	CompilerVersion  string                             `json:"compiler_version"`
	Actor            string                             `json:"actor"`
	Tenant           string                             `json:"tenant,omitempty"`
	Cluster          string                             `json:"cluster,omitempty"`
	CreatedAt        time.Time                          `json:"created_at"`
	ExpiresAt        time.Time                          `json:"expires_at"`
	RevokedAt        *time.Time                         `json:"revoked_at,omitempty"`
	RevokedBy        string                             `json:"revoked_by,omitempty"`
	RevocationReason string                             `json:"revocation_reason,omitempty"`
	Profiles         []config.AcceleratorRuntimeProfile `json:"profiles,omitempty"`
	Policies         []config.Policy                    `json:"policies,omitempty"`
}

// candidateBundle is deliberately a small, strict configuration envelope.  A
// candidate may carry a profile-only change, a policy-only change, or both;
// it never contains executable scripts or arbitrary controller flags.
type candidateBundle struct {
	APIVersion          string                             `yaml:"apiVersion" json:"api_version"`
	Kind                string                             `yaml:"kind" json:"kind"`
	AcceleratorProfiles []config.AcceleratorRuntimeProfile `yaml:"accelerator_profiles" json:"accelerator_profiles,omitempty"`
	Policies            []config.Policy                    `yaml:"policies" json:"policies,omitempty"`
}

// candidateMetadata and the CR-shaped types below intentionally model only
// fields that can influence a preview.  They are not Kubernetes API objects:
// parsing a candidate must be possible in an isolated process that has no
// cluster credentials.  Keeping them local also means an unrecognised future
// CR field is rejected instead of being silently omitted from the candidate
// digest.
type candidateMetadata struct {
	Name       string `yaml:"name"`
	UID        string `yaml:"uid,omitempty"`
	Generation int64  `yaml:"generation,omitempty"`
}

type candidateLabelSelector struct {
	MatchLabels      map[string]string `yaml:"matchLabels"`
	MatchExpressions []any             `yaml:"matchExpressions,omitempty"`
}

type candidateRuntimeActionPolicy struct {
	Action                               types.AcceleratorAction        `yaml:"action"`
	Scopes                               []types.AcceleratorTargetScope `yaml:"scopes"`
	RequireVerifiedUnpartitionedTopology bool                           `yaml:"requireVerifiedUnpartitionedTopology,omitempty"`
}

type candidateRuntimeProfileSpec struct {
	KubeNeuronRef  string                         `yaml:"kubeNeuronRef,omitempty"`
	NodeSelector   candidateLabelSelector         `yaml:"nodeSelector"`
	Vendor         types.AcceleratorVendor        `yaml:"vendor"`
	ProfileDigest  string                         `yaml:"profileDigest"`
	DriverVersion  string                         `yaml:"driverVersion"`
	RuntimeVersion string                         `yaml:"runtimeVersion"`
	MaxReportAge   string                         `yaml:"maxReportAge"`
	AllowedActions []candidateRuntimeActionPolicy `yaml:"allowedActions,omitempty"`
}

type candidateRuntimeProfileCR struct {
	APIVersion string                      `yaml:"apiVersion"`
	Kind       string                      `yaml:"kind"`
	Metadata   candidateMetadata           `yaml:"metadata"`
	Spec       candidateRuntimeProfileSpec `yaml:"spec"`
}

type candidateSignalMatch struct {
	Class    types.ProblemClass      `yaml:"class"`
	Vendor   types.AcceleratorVendor `yaml:"vendor,omitempty"`
	Source   string                  `yaml:"source,omitempty"`
	Severity string                  `yaml:"severity,omitempty"`
}

type candidateRemediationPolicySpec struct {
	KubeNeuronRef string               `yaml:"kubeNeuronRef,omitempty"`
	Priority      int32                `yaml:"priority,omitempty"`
	Match         candidateSignalMatch `yaml:"match"`
	PlaybookRef   string               `yaml:"playbookRef"`
	Parameters    map[string]string    `yaml:"parameters,omitempty"`
}

type candidateRemediationPolicyCR struct {
	APIVersion string                         `yaml:"apiVersion"`
	Kind       string                         `yaml:"kind"`
	Metadata   candidateMetadata              `yaml:"metadata"`
	Spec       candidateRemediationPolicySpec `yaml:"spec"`
}

// CreateCandidate validates, normalizes, and durably stores a candidate
// without applying it to Kubernetes.  Parsing has no cluster client and no
// write credential; callers can run it in a separately sandboxed process in
// deployments that need a stronger isolation boundary.
func (m *Manager) CreateCandidate(ctx context.Context, actor string, upload CandidateUpload) (*CandidateConfiguration, bool, error) {
	if err := m.requireResources(); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(actor) == "" {
		return nil, false, fmt.Errorf("actor is required")
	}
	if strings.TrimSpace(upload.IdempotencyKey) == "" {
		return nil, false, fmt.Errorf("idempotency key is required")
	}
	if len(upload.Content) == 0 || len(upload.Content) > maxCandidateBytes {
		return nil, false, fmt.Errorf("candidate content must be between 1 and %d bytes", maxCandidateBytes)
	}
	// Parse and validate before reserving the retry key. A malformed upload is
	// not an operation, so it must not poison a key that an operator retries
	// after fixing YAML or an expiry typo.
	bundle, normalized, err := compileCandidate(upload.Content)
	if err != nil {
		return nil, false, err
	}
	now := m.now().UTC()
	requestedExpiry := upload.ExpiresAt.UTC()
	expires := requestedExpiry
	if expires.IsZero() {
		expires = now.Add(defaultCandidateTTL)
	}
	if !expires.After(now) {
		return nil, false, fmt.Errorf("candidate expiry must be in the future")
	}
	digest := digestBytes(upload.Content)
	requestDigest := digestJSON(struct {
		ContentDigest string    `json:"content_digest"`
		Tenant        string    `json:"tenant"`
		Cluster       string    `json:"cluster"`
		ExpiresAt     time.Time `json:"expires_at,omitempty"`
	}{ContentDigest: digest, Tenant: strings.TrimSpace(upload.Tenant), Cluster: strings.TrimSpace(upload.Cluster), ExpiresAt: requestedExpiry})
	reservation := &types.OperationalIdempotencyRecord{
		Kind: types.ResourceCandidateConfiguration, Actor: actor, Key: scopedIdempotencyKey("upload", upload.IdempotencyKey),
		RequestDigest: requestDigest, ResourceID: newID("candidate"),
	}
	claimed, created, err := m.resources.PutOperationalIdempotency(ctx, reservation)
	if err != nil {
		return nil, false, err
	}
	if !created {
		if claimed.RequestDigest != requestDigest {
			return nil, false, ErrIdempotencyConflict
		}
		candidate, err := m.getCandidate(ctx, claimed.ResourceID)
		return candidate, true, err
	}

	candidate := &CandidateConfiguration{
		ID: reservation.ResourceID, ResourceVersion: 1, State: "uploaded",
		APIVersion:       bundle.APIVersion,
		Kind:             bundle.Kind,
		OriginalDigest:   digest,
		NormalizedDigest: digestBytes(normalized),
		Normalized:       normalized,
		CompilerVersion:  CompilerVersion,
		Actor:            actor,
		Tenant:           strings.TrimSpace(upload.Tenant),
		Cluster:          strings.TrimSpace(upload.Cluster),
		CreatedAt:        now,
		ExpiresAt:        expires,
		Profiles:         bundle.AcceleratorProfiles,
		Policies:         bundle.Policies,
	}
	payload, err := marshalPayload(candidate)
	if err != nil {
		return nil, false, err
	}
	if err := m.resources.CreateOperationalResource(ctx, &types.OperationalResource{
		Kind: types.ResourceCandidateConfiguration, ID: candidate.ID, State: "uploaded", Actor: actor,
		Tenant: candidate.Tenant, Cluster: candidate.Cluster, ConfigDigest: candidate.NormalizedDigest,
		Payload: payload, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, Version: candidate.ResourceVersion,
	}); err != nil {
		return nil, false, err
	}
	if err := m.audit(ctx, types.ResourceCandidateConfiguration, candidate.ID, actor, "upload", upload.IdempotencyKey, "", map[string]string{"original_digest": digest, "normalized_digest": candidate.NormalizedDigest}, "validated"); err != nil {
		return nil, false, err
	}
	return candidate, false, nil
}

func compileCandidate(content []byte) (candidateBundle, json.RawMessage, error) {
	// Decode a minimal header first, then decode exactly the selected schema
	// with KnownFields.  This accepts both the purpose-built review bundle and
	// the native CR-shaped objects operators already keep in Git, while still
	// rejecting a field that a newer schema could otherwise smuggle past an old
	// compiler.
	var header struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := decodeSingleCandidateDocument(content, &header, false); err != nil {
		return candidateBundle{}, nil, err
	}
	if header.APIVersion != "kubeneuron.io/v1alpha1" {
		return candidateBundle{}, nil, fmt.Errorf("candidate schema: unsupported apiVersion %q", header.APIVersion)
	}

	var bundle candidateBundle
	switch header.Kind {
	case "CandidateConfiguration":
		if err := decodeSingleCandidateDocument(content, &bundle, true); err != nil {
			return candidateBundle{}, nil, err
		}
	case "AcceleratorRuntimeProfile":
		var profileCR candidateRuntimeProfileCR
		if err := decodeSingleCandidateDocument(content, &profileCR, true); err != nil {
			return candidateBundle{}, nil, err
		}
		profile, err := compileCandidateRuntimeProfile(profileCR)
		if err != nil {
			return candidateBundle{}, nil, err
		}
		bundle = candidateBundle{APIVersion: header.APIVersion, Kind: "CandidateConfiguration", AcceleratorProfiles: []config.AcceleratorRuntimeProfile{profile}}
	case "GPURemediationPolicy":
		var policyCR candidateRemediationPolicyCR
		if err := decodeSingleCandidateDocument(content, &policyCR, true); err != nil {
			return candidateBundle{}, nil, err
		}
		policy, err := compileCandidateRemediationPolicy(policyCR)
		if err != nil {
			return candidateBundle{}, nil, err
		}
		bundle = candidateBundle{APIVersion: header.APIVersion, Kind: "CandidateConfiguration", Policies: []config.Policy{policy}}
	default:
		return candidateBundle{}, nil, fmt.Errorf("candidate schema: unsupported kind %q", header.Kind)
	}
	if len(bundle.AcceleratorProfiles) == 0 && len(bundle.Policies) == 0 {
		return candidateBundle{}, nil, fmt.Errorf("candidate schema: at least one profile or policy is required")
	}
	seenProfiles := make(map[string]struct{}, len(bundle.AcceleratorProfiles))
	for _, profile := range bundle.AcceleratorProfiles {
		if err := profile.Validate(); err != nil {
			return candidateBundle{}, nil, fmt.Errorf("candidate profile %q: %w", profile.Name, err)
		}
		if _, exists := seenProfiles[profile.Name]; exists {
			return candidateBundle{}, nil, fmt.Errorf("candidate schema: duplicate profile %q", profile.Name)
		}
		seenProfiles[profile.Name] = struct{}{}
	}
	for index, policy := range bundle.Policies {
		if strings.TrimSpace(string(policy.Match.Class)) == "" || strings.TrimSpace(policy.Playbook) == "" {
			return candidateBundle{}, nil, fmt.Errorf("candidate policy %d requires match.class and playbook", index)
		}
	}
	normalized, err := json.Marshal(bundle)
	if err != nil {
		return candidateBundle{}, nil, fmt.Errorf("normalize candidate: %w", err)
	}
	return bundle, normalized, nil
}

func decodeSingleCandidateDocument(content []byte, destination any, knownFields bool) error {
	decoder := yaml.NewDecoder(strings.NewReader(string(content)))
	decoder.KnownFields(knownFields)
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("candidate schema: %w", err)
	}
	// A second YAML document is a configuration-smuggling attempt. A review is
	// always for exactly one object, so an uploader cannot hide a different CR
	// behind a valid first document.
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("candidate schema: exactly one document is required")
		}
		return fmt.Errorf("candidate schema: %w", err)
	}
	return nil
}

func compileCandidateRuntimeProfile(in candidateRuntimeProfileCR) (config.AcceleratorRuntimeProfile, error) {
	if in.Metadata.Name == "" {
		return config.AcceleratorRuntimeProfile{}, fmt.Errorf("candidate AcceleratorRuntimeProfile metadata.name is required")
	}
	if len(in.Spec.NodeSelector.MatchExpressions) > 0 {
		return config.AcceleratorRuntimeProfile{}, fmt.Errorf("candidate AcceleratorRuntimeProfile nodeSelector.matchExpressions is not supported; use matchLabels")
	}
	maxAge, err := time.ParseDuration(in.Spec.MaxReportAge)
	if err != nil {
		return config.AcceleratorRuntimeProfile{}, fmt.Errorf("candidate AcceleratorRuntimeProfile spec.maxReportAge: %w", err)
	}
	profileUID := strings.TrimSpace(in.Metadata.UID)
	if profileUID == "" {
		// A candidate is intentionally not applied to Kubernetes, so it has no
		// API-server UID yet. Derive a visibly candidate-only identity that is
		// bound to its normalized fields rather than pretending it is a live CR.
		profileUID = "candidate-" + strings.TrimPrefix(digestJSON(struct {
			Name string                      `json:"name"`
			Spec candidateRuntimeProfileSpec `json:"spec"`
		}{in.Metadata.Name, in.Spec}), "sha256:")[:16]
	}
	generation := in.Metadata.Generation
	if generation <= 0 {
		generation = 1
	}
	profile := config.AcceleratorRuntimeProfile{
		Name: in.Metadata.Name, NodeSelector: copySelector(in.Spec.NodeSelector.MatchLabels), Vendor: in.Spec.Vendor,
		ProfileDigest: in.Spec.ProfileDigest, DriverVersion: in.Spec.DriverVersion, RuntimeVersion: in.Spec.RuntimeVersion,
		ProfileUID: profileUID, ProfileGeneration: generation, MaxReportAge: config.Duration(maxAge),
		AllowedActions: make([]config.AcceleratorActionPolicy, 0, len(in.Spec.AllowedActions)),
	}
	for _, action := range in.Spec.AllowedActions {
		profile.AllowedActions = append(profile.AllowedActions, config.AcceleratorActionPolicy{
			Action: action.Action, Scopes: append([]types.AcceleratorTargetScope(nil), action.Scopes...),
			RequireVerifiedUnpartitionedTopology: action.RequireVerifiedUnpartitionedTopology,
		})
	}
	if err := profile.Validate(); err != nil {
		return config.AcceleratorRuntimeProfile{}, fmt.Errorf("candidate AcceleratorRuntimeProfile %q: %w", profile.Name, err)
	}
	return profile, nil
}

func compileCandidateRemediationPolicy(in candidateRemediationPolicyCR) (config.Policy, error) {
	if strings.TrimSpace(in.Metadata.Name) == "" {
		return config.Policy{}, fmt.Errorf("candidate GPURemediationPolicy metadata.name is required")
	}
	if strings.TrimSpace(string(in.Spec.Match.Class)) == "" || strings.TrimSpace(in.Spec.PlaybookRef) == "" {
		return config.Policy{}, fmt.Errorf("candidate GPURemediationPolicy %q requires spec.match.class and spec.playbookRef", in.Metadata.Name)
	}
	if in.Spec.Match.Source != "" || in.Spec.Match.Severity != "" {
		return config.Policy{}, fmt.Errorf("candidate GPURemediationPolicy %q uses unsupported match fields source or severity", in.Metadata.Name)
	}
	if in.Spec.Match.Vendor != "" && !in.Spec.Match.Vendor.Valid() {
		return config.Policy{}, fmt.Errorf("candidate GPURemediationPolicy %q has unsupported vendor %q", in.Metadata.Name, in.Spec.Match.Vendor)
	}
	return config.Policy{
		Match:    config.Match{Class: in.Spec.Match.Class, Vendor: in.Spec.Match.Vendor},
		Playbook: in.Spec.PlaybookRef, Params: copySelector(in.Spec.Parameters),
	}, nil
}

func (m *Manager) getCandidate(ctx context.Context, id string) (*CandidateConfiguration, error) {
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourceCandidateConfiguration, id)
	if err != nil {
		return nil, err
	}
	var candidate CandidateConfiguration
	if err := json.Unmarshal(resource.Payload, &candidate); err != nil {
		return nil, fmt.Errorf("candidate %q is corrupt: %w", id, err)
	}
	candidate.ResourceVersion = resource.Version
	candidate.State = resource.State
	return &candidate, nil
}

// GetCandidate returns one immutable candidate.  It remains available after
// expiry for authorized audit readers; Preview independently refuses to use an
// expired candidate as an active input.
func (m *Manager) GetCandidate(ctx context.Context, id string) (*CandidateConfiguration, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	return m.getCandidate(ctx, id)
}

// ListCandidates returns a stable, scope-filtered page of immutable uploaded
// candidates. Expired candidates remain listable when IncludeExpired is set,
// because expiry revokes use as an active input but must not erase the review
// and audit record that explains a prior preview.
func (m *Manager) ListCandidates(ctx context.Context, options OperationalListOptions) ([]*CandidateConfiguration, error) {
	resources, err := m.listOperationalResources(ctx, types.ResourceCandidateConfiguration, options)
	if err != nil {
		return nil, err
	}
	out := make([]*CandidateConfiguration, 0, len(resources))
	for _, resource := range resources {
		var candidate CandidateConfiguration
		if err := json.Unmarshal(resource.Payload, &candidate); err != nil {
			return nil, fmt.Errorf("candidate %q is corrupt: %w", resource.ID, err)
		}
		candidate.ResourceVersion = resource.Version
		candidate.State = resource.State
		out = append(out, &candidate)
	}
	return out, nil
}

// RevokeCandidate is a logical deletion: it makes a candidate unusable for
// new previews immediately while retaining normalized bytes and its immutable
// audit history for incident/review replay. This is safer than a physical
// DELETE, which would turn a formerly reviewable policy change into a hole in
// the audit trail.
func (m *Manager) RevokeCandidate(ctx context.Context, actor, id, reason, idempotencyKey string, expectedVersion int) (*CandidateConfiguration, bool, error) {
	if err := m.requireResources(); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(id) == "" || strings.TrimSpace(reason) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return nil, false, fmt.Errorf("actor, candidate ID, revocation reason, and idempotency key are required")
	}
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourceCandidateConfiguration, id)
	if err != nil {
		return nil, false, err
	}
	requestDigest := digestJSON(struct {
		ID              string `json:"id"`
		Reason          string `json:"reason"`
		ExpectedVersion int    `json:"expected_version"`
	}{ID: id, Reason: reason, ExpectedVersion: expectedVersion})
	// Check a prior successful retry before looking at the current lifecycle
	// state: a replay naturally sees the candidate as revoked.
	storageKey := scopedIdempotencyKey("revoke", idempotencyKey)
	if existing, lookupErr := m.resources.GetOperationalIdempotency(ctx, types.ResourceCandidateConfiguration, actor, storageKey); lookupErr == nil {
		if existing.RequestDigest != requestDigest || existing.ResourceID != id {
			return nil, false, ErrIdempotencyConflict
		}
		candidate, getErr := m.getCandidate(ctx, id)
		return candidate, true, getErr
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return nil, false, lookupErr
	}
	if expectedVersion > 0 && expectedVersion != resource.Version {
		return nil, false, store.ErrOperationalConflict
	}
	if resource.State != "uploaded" {
		return nil, false, fmt.Errorf("%w: candidate %q is %s", ErrInvalidState, id, resource.State)
	}
	record, created, err := m.resources.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{
		Kind: types.ResourceCandidateConfiguration, Actor: actor, Key: storageKey,
		RequestDigest: requestDigest, ResourceID: id,
	})
	if err != nil {
		return nil, false, err
	}
	if !created {
		if record.RequestDigest != requestDigest || record.ResourceID != id {
			return nil, false, ErrIdempotencyConflict
		}
		candidate, err := m.getCandidate(ctx, id)
		return candidate, true, err
	}
	var candidate CandidateConfiguration
	if err := json.Unmarshal(resource.Payload, &candidate); err != nil {
		return nil, false, fmt.Errorf("candidate %q is corrupt: %w", id, err)
	}
	now := m.now().UTC()
	candidate.State, candidate.RevokedAt, candidate.RevokedBy, candidate.RevocationReason = "revoked", &now, actor, reason
	candidate.ResourceVersion = resource.Version + 1
	payload, err := marshalPayload(candidate)
	if err != nil {
		return nil, false, err
	}
	resource.State, resource.Actor, resource.Payload = candidate.State, actor, payload
	resource.ExpiresAt = &now
	if err := m.resources.UpdateOperationalResource(ctx, resource, resource.Version); err != nil {
		return nil, false, err
	}
	candidate.ResourceVersion = resource.Version
	if err := m.audit(ctx, types.ResourceCandidateConfiguration, id, actor, "revoke", idempotencyKey, "", map[string]string{"reason": reason}, "revoked"); err != nil {
		return nil, false, err
	}
	return &candidate, false, nil
}

// NodeDecisionDelta explains the before/after answer for one node in a policy
// impact preview.  Both results include their own config digest and evidence
// refs, so the diff can be exported/replayed without live controller logs.
type NodeDecisionDelta struct {
	Node    string          `json:"node"`
	Before  decision.Result `json:"before"`
	After   decision.Result `json:"after"`
	Changed bool            `json:"changed"`
}

type PolicyImpactPreview struct {
	ID                  string              `json:"id"`
	ResourceVersion     int                 `json:"resource_version"`
	CandidateID         string              `json:"candidate_id"`
	CandidateDigest     string              `json:"candidate_digest"`
	Tenant              string              `json:"tenant,omitempty"`
	Cluster             string              `json:"cluster,omitempty"`
	InventorySnapshotID string              `json:"inventory_snapshot_id"`
	EvaluatorVersion    string              `json:"evaluator_version"`
	CreatedAt           time.Time           `json:"created_at"`
	NewlyEligible       []NodeDecisionDelta `json:"newly_eligible"`
	NewlyBlocked        []NodeDecisionDelta `json:"newly_blocked"`
	ChangedObservedOnly []NodeDecisionDelta `json:"changed_observed_only"`
	Unchanged           []NodeDecisionDelta `json:"unchanged"`
}

type fleetSnapshot struct {
	ID          string         `json:"id"`
	CandidateID string         `json:"candidate_id"`
	Tenant      string         `json:"tenant,omitempty"`
	Cluster     string         `json:"cluster,omitempty"`
	CapturedAt  time.Time      `json:"captured_at"`
	Decisions   []capturedNode `json:"decisions"`
}

type capturedNode struct {
	Node         string            `json:"node"`
	Before       decision.Snapshot `json:"before"`
	After        decision.Snapshot `json:"after"`
	BeforeResult decision.Result   `json:"before_result"`
	AfterResult  decision.Result   `json:"after_result"`
}

// CreatePreview evaluates a candidate against one frozen fleet input.  Any
// incomplete/stale snapshot rejects the preview rather than producing a
// plausible diff from mixed-time telemetry.
func (m *Manager) CreatePreview(ctx context.Context, actor, candidateID, idempotencyKey string) (*PolicyImpactPreview, bool, error) {
	if err := m.requireSnapshots(); err != nil {
		return nil, false, err
	}
	if m.listNodes == nil {
		return nil, false, fmt.Errorf("%w: node inventory lister is unavailable", ErrUnavailable)
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(candidateID) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return nil, false, fmt.Errorf("actor, candidate ID, and idempotency key are required")
	}
	candidate, err := m.getCandidate(ctx, candidateID)
	if err != nil {
		return nil, false, err
	}
	if candidate.State != "uploaded" {
		return nil, false, fmt.Errorf("%w: candidate %q is %s", ErrInvalidState, candidate.ID, candidate.State)
	}
	if !candidate.ExpiresAt.After(m.now()) {
		return nil, false, fmt.Errorf("candidate %q is expired", candidate.ID)
	}
	requestDigest := digestJSON(struct {
		CandidateID string `json:"candidate_id"`
		Digest      string `json:"digest"`
	}{candidateID, candidate.NormalizedDigest})

	nodes, err := m.listNodes(ctx)
	if err != nil {
		return nil, false, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	captured := make([]capturedNode, 0, len(nodes))
	preview := &PolicyImpactPreview{
		ID: newID("preview"), ResourceVersion: 1, CandidateID: candidate.ID, CandidateDigest: candidate.NormalizedDigest,
		Tenant: candidate.Tenant, Cluster: candidate.Cluster,
		InventorySnapshotID: newID("inventory"), EvaluatorVersion: decision.EvaluatorVersion,
		CreatedAt: m.now().UTC(),
	}
	for _, node := range nodes {
		if node == nil || strings.TrimSpace(node.Name) == "" {
			return nil, false, fmt.Errorf("%w: inventory includes an unnamed node", ErrIncompleteInventory)
		}
		if !operationalNodeScopeMatches(node, candidate.Tenant, candidate.Cluster) {
			continue
		}
		before, err := m.buildSnapshot(ctx, node.Name, decision.Request{Class: decision.ActionObserve})
		if err != nil {
			return nil, false, fmt.Errorf("%w: capture %s: %v", ErrIncompleteInventory, node.Name, err)
		}
		if !operationalNodeScopeMatches(&before.Node, candidate.Tenant, candidate.Cluster) {
			return nil, false, fmt.Errorf("%w: node %s moved outside candidate tenant/cluster scope during capture", ErrIncompleteInventory, node.Name)
		}
		beforeResult := m.evaluateDecision("preview", before)
		if incomplete(beforeResult) {
			return nil, false, fmt.Errorf("%w: node %s: %s", ErrIncompleteInventory, node.Name, beforeResult.HumanSummary)
		}
		after, err := snapshotWithCandidate(before, candidate)
		if err != nil {
			return nil, false, fmt.Errorf("candidate decision for node %s: %w", node.Name, err)
		}
		afterResult := m.evaluateDecision("preview", after)
		if incomplete(afterResult) {
			return nil, false, fmt.Errorf("%w: candidate node %s: %s", ErrIncompleteInventory, node.Name, afterResult.HumanSummary)
		}
		delta := NodeDecisionDelta{Node: node.Name, Before: beforeResult, After: afterResult, Changed: !resultsEqual(beforeResult, afterResult)}
		switch {
		case beforeResult.State != decision.StateEligible && afterResult.State == decision.StateEligible:
			preview.NewlyEligible = append(preview.NewlyEligible, delta)
		case beforeResult.State == decision.StateEligible && afterResult.State != decision.StateEligible:
			preview.NewlyBlocked = append(preview.NewlyBlocked, delta)
		case beforeResult.State == decision.StateObservedOnly || afterResult.State == decision.StateObservedOnly:
			if delta.Changed {
				preview.ChangedObservedOnly = append(preview.ChangedObservedOnly, delta)
			} else {
				preview.Unchanged = append(preview.Unchanged, delta)
			}
		default:
			if delta.Changed {
				preview.NewlyBlocked = append(preview.NewlyBlocked, delta)
			} else {
				preview.Unchanged = append(preview.Unchanged, delta)
			}
		}
		captured = append(captured, capturedNode{Node: node.Name, Before: before, After: after, BeforeResult: beforeResult, AfterResult: afterResult})
	}

	inventory := fleetSnapshot{ID: preview.InventorySnapshotID, CandidateID: candidate.ID, Tenant: candidate.Tenant, Cluster: candidate.Cluster, CapturedAt: preview.CreatedAt, Decisions: captured}
	inventoryPayload, err := marshalPayload(inventory)
	if err != nil {
		return nil, false, err
	}
	payload, err := marshalPayload(preview)
	if err != nil {
		return nil, false, err
	}
	// Capture/compile validation is intentionally complete before claiming the
	// retry key. An incomplete inventory is a normal, recoverable condition;
	// reserving first would leave a key pointing at no preview and make the
	// operator unable to retry after fresh evidence arrives.
	currentCandidate, err := m.getCandidate(ctx, candidate.ID)
	if err != nil {
		return nil, false, err
	}
	if currentCandidate.ResourceVersion != candidate.ResourceVersion || currentCandidate.State != "uploaded" || currentCandidate.NormalizedDigest != candidate.NormalizedDigest {
		return nil, false, fmt.Errorf("%w: candidate %q changed while its preview was captured", ErrInvalidState, candidate.ID)
	}
	if !currentCandidate.ExpiresAt.After(m.now()) {
		return nil, false, fmt.Errorf("candidate %q is expired", candidate.ID)
	}
	reservation := &types.OperationalIdempotencyRecord{
		Kind: types.ResourcePolicyImpactPreview, Actor: actor, Key: scopedIdempotencyKey("preview", idempotencyKey),
		RequestDigest: requestDigest, ResourceID: preview.ID,
	}
	claimed, created, err := m.resources.PutOperationalIdempotency(ctx, reservation)
	if err != nil {
		return nil, false, err
	}
	if !created {
		if claimed.RequestDigest != requestDigest {
			return nil, false, ErrIdempotencyConflict
		}
		previous, getErr := m.getPreview(ctx, claimed.ResourceID)
		return previous, true, getErr
	}
	if err := m.resources.CreateOperationalResource(ctx, &types.OperationalResource{
		Kind: types.ResourceDecisionSnapshot, ID: inventory.ID, State: "captured", Actor: actor,
		Tenant: candidate.Tenant, Cluster: candidate.Cluster, ConfigDigest: candidate.NormalizedDigest,
		Payload: inventoryPayload, CreatedAt: preview.CreatedAt, UpdatedAt: preview.CreatedAt, ExpiresAt: &candidate.ExpiresAt,
	}); err != nil {
		return nil, false, err
	}
	if err := m.resources.CreateOperationalResource(ctx, &types.OperationalResource{
		Kind: types.ResourcePolicyImpactPreview, ID: preview.ID, State: "complete", Actor: actor,
		Tenant: candidate.Tenant, Cluster: candidate.Cluster, ConfigDigest: candidate.NormalizedDigest,
		Payload: payload, CreatedAt: preview.CreatedAt, UpdatedAt: preview.CreatedAt, ExpiresAt: &candidate.ExpiresAt, Version: preview.ResourceVersion,
	}); err != nil {
		return nil, false, err
	}
	if err := m.audit(ctx, types.ResourceDecisionSnapshot, inventory.ID, actor, "capture-fleet", idempotencyKey, "", map[string]string{"candidate_id": candidate.ID}, "captured"); err != nil {
		return nil, false, err
	}
	if err := m.audit(ctx, types.ResourcePolicyImpactPreview, preview.ID, actor, "preview", idempotencyKey, inventory.ID, map[string]string{"candidate_id": candidate.ID}, "complete"); err != nil {
		return nil, false, err
	}
	return preview, false, nil
}

func (m *Manager) getPreview(ctx context.Context, id string) (*PolicyImpactPreview, error) {
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourcePolicyImpactPreview, id)
	if err != nil {
		return nil, err
	}
	var preview PolicyImpactPreview
	if err := json.Unmarshal(resource.Payload, &preview); err != nil {
		return nil, fmt.Errorf("preview %q is corrupt: %w", id, err)
	}
	preview.ResourceVersion = resource.Version
	return &preview, nil
}

func (m *Manager) GetPreview(ctx context.Context, id string) (*PolicyImpactPreview, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	return m.getPreview(ctx, id)
}

// ListPreviews provides the audit/console view of immutable previews. The
// candidate-centric endpoint remains the convenient way to retrieve the
// latest result for one candidate; this list is intentionally stable for
// export and review queues.
func (m *Manager) ListPreviews(ctx context.Context, options OperationalListOptions) ([]*PolicyImpactPreview, error) {
	resources, err := m.listOperationalResources(ctx, types.ResourcePolicyImpactPreview, options)
	if err != nil {
		return nil, err
	}
	out := make([]*PolicyImpactPreview, 0, len(resources))
	for _, resource := range resources {
		var preview PolicyImpactPreview
		if err := json.Unmarshal(resource.Payload, &preview); err != nil {
			return nil, fmt.Errorf("preview %q is corrupt: %w", resource.ID, err)
		}
		preview.ResourceVersion = resource.Version
		out = append(out, &preview)
	}
	return out, nil
}

// LatestPreviewForCandidate returns the most recently created retained preview
// for a candidate.  Preview IDs remain immutable and are exposed inside the
// response; this convenience read powers the candidate-centric REST route.
func (m *Manager) LatestPreviewForCandidate(ctx context.Context, candidateID string) (*PolicyImpactPreview, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	var latest *PolicyImpactPreview
	options := OperationalListOptions{Limit: 500, IncludeExpired: true}
	for {
		resources, err := m.listOperationalResources(ctx, types.ResourcePolicyImpactPreview, options)
		if err != nil {
			return nil, err
		}
		for _, resource := range resources {
			var preview PolicyImpactPreview
			if err := json.Unmarshal(resource.Payload, &preview); err != nil {
				return nil, fmt.Errorf("preview %q is corrupt: %w", resource.ID, err)
			}
			preview.ResourceVersion = resource.Version
			if preview.CandidateID != candidateID {
				continue
			}
			if latest == nil || preview.CreatedAt.After(latest.CreatedAt) || (preview.CreatedAt.Equal(latest.CreatedAt) && preview.ID > latest.ID) {
				copy := preview
				latest = &copy
			}
		}
		if len(resources) < options.Limit {
			break
		}
		last := resources[len(resources)-1]
		if !options.AfterCreatedAt.IsZero() && (last.CreatedAt.Before(options.AfterCreatedAt) || (last.CreatedAt.Equal(options.AfterCreatedAt) && last.ID <= options.AfterID)) {
			return nil, fmt.Errorf("%w: preview cursor did not advance", ErrIncompleteInventory)
		}
		options.AfterCreatedAt, options.AfterID = last.CreatedAt, last.ID
	}
	if latest == nil {
		return nil, store.ErrNotFound
	}
	return latest, nil
}

func snapshotWithCandidate(before decision.Snapshot, candidate *CandidateConfiguration) (decision.Snapshot, error) {
	after, err := cloneSnapshot(before)
	if err != nil {
		return decision.Snapshot{}, err
	}
	after.ConfigDigest = candidate.NormalizedDigest
	if after.Report == nil || !after.Report.Vendor.Valid() {
		after.Profile = nil
		return after, nil
	}
	profile, err := (config.Config{AcceleratorProfiles: candidate.Profiles}).ResolveAcceleratorRuntimeProfile(after.Node.Labels, after.Report.Vendor)
	if errors.Is(err, config.ErrNoAcceleratorRuntimeProfile) {
		after.Profile = nil
		return after, nil
	}
	if err != nil {
		return decision.Snapshot{}, err
	}
	after.Profile = profile
	return after, nil
}

func cloneSnapshot(in decision.Snapshot) (decision.Snapshot, error) {
	blob, err := json.Marshal(in)
	if err != nil {
		return decision.Snapshot{}, err
	}
	var out decision.Snapshot
	if err := json.Unmarshal(blob, &out); err != nil {
		return decision.Snapshot{}, err
	}
	return out, nil
}

func incomplete(result decision.Result) bool {
	return result.State == decision.StateUnknown
}

func resultsEqual(a, b decision.Result) bool {
	// ConfigDigest necessarily changes across an accepted candidate and
	// EvaluatedAt may differ by nanoseconds.  The preview's "changed" contract
	// is the outcome/gates/limits an operator would act on.
	if a.State != b.State || len(a.ReasonCodes) != len(b.ReasonCodes) || len(a.AllowedActions) != len(b.AllowedActions) || a.Limits != b.Limits {
		return false
	}
	for i := range a.ReasonCodes {
		if a.ReasonCodes[i] != b.ReasonCodes[i] {
			return false
		}
	}
	for i := range a.AllowedActions {
		if a.AllowedActions[i] != b.AllowedActions[i] {
			return false
		}
	}
	return true
}

// HealthCheckProfile is an explicitly bounded diagnostic capability.
type HealthCheckProfile string

const (
	HealthCheckPassive  HealthCheckProfile = "Passive"
	HealthCheckQuick    HealthCheckProfile = "Quick"
	HealthCheckExtended HealthCheckProfile = "Extended"
)

type HealthCheckRequest struct {
	Node     string             `json:"node"`
	DeviceID string             `json:"device_id,omitempty"`
	Profile  HealthCheckProfile `json:"profile"`
	Reason   string             `json:"reason"`
	Deadline time.Time          `json:"deadline,omitempty"`
	Tenant   string             `json:"tenant,omitempty"`
	Cluster  string             `json:"cluster,omitempty"`
	// These two grants are supplied by the authorization adapter after role
	// and change-budget checks. Extended diagnostics refuse a caller that only
	// claims a profile name; an API layer must make both explicit.
	ElevatedAuthorizationGranted bool   `json:"elevated_authorization_granted,omitempty"`
	DisruptionBudgetApproved     bool   `json:"disruption_budget_approved,omitempty"`
	IdempotencyKey               string `json:"-"`
}

type HealthCheckRun struct {
	ID                 string             `json:"id"`
	ResourceVersion    int                `json:"resource_version"`
	Node               string             `json:"node"`
	DeviceID           string             `json:"device_id,omitempty"`
	Profile            HealthCheckProfile `json:"profile"`
	State              string             `json:"state"`
	Reason             string             `json:"reason"`
	Actor              string             `json:"actor"`
	Tenant             string             `json:"tenant,omitempty"`
	Cluster            string             `json:"cluster,omitempty"`
	DecisionSnapshotID string             `json:"decision_snapshot_id"`
	Decision           decision.Result    `json:"decision"`
	ActionID           string             `json:"action_id,omitempty"`
	Deadline           time.Time          `json:"deadline"`
	EvidenceDigest     string             `json:"evidence_digest,omitempty"`
	Outcome            string             `json:"outcome,omitempty"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
	CompletedAt        *time.Time         `json:"completed_at,omitempty"`
}

// CreateHealthCheck creates a durable run. Passive runs capture existing
// evidence only; Quick and Extended runs use the existing leased action queue
// with explicit timeout/diag level and cannot bypass the global stop.
func (m *Manager) CreateHealthCheck(ctx context.Context, actor string, request HealthCheckRequest) (*HealthCheckRun, bool, error) {
	if err := m.requireSnapshots(); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(request.Node) == "" || strings.TrimSpace(request.Reason) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return nil, false, fmt.Errorf("actor, node, reason, and idempotency key are required")
	}
	if request.Profile != HealthCheckPassive && request.Profile != HealthCheckQuick && request.Profile != HealthCheckExtended {
		return nil, false, fmt.Errorf("unsupported health-check profile %q", request.Profile)
	}
	request.Tenant, request.Cluster = strings.TrimSpace(request.Tenant), strings.TrimSpace(request.Cluster)
	now := m.now().UTC()
	requestedDeadline := request.Deadline.UTC()
	deadline, err := diagnosticDeadline(now, request.Profile, requestedDeadline)
	if err != nil {
		return nil, false, err
	}
	requestDigest := digestJSON(struct {
		Node, DeviceID, Profile, Reason string
		Tenant, Cluster                 string
		Deadline                        time.Time
		ElevatedAuthorizationGranted    bool
		DisruptionBudgetApproved        bool
	}{request.Node, request.DeviceID, string(request.Profile), request.Reason, strings.TrimSpace(request.Tenant), strings.TrimSpace(request.Cluster), requestedDeadline,
		request.ElevatedAuthorizationGranted, request.DisruptionBudgetApproved})
	reservation := &types.OperationalIdempotencyRecord{
		Kind: types.ResourceHealthCheckRun, Actor: actor, Key: scopedIdempotencyKey("request", request.IdempotencyKey),
		RequestDigest: requestDigest, ResourceID: newID("health"),
	}
	if existing, lookupErr := m.resources.GetOperationalIdempotency(ctx, types.ResourceHealthCheckRun, actor, scopedIdempotencyKey("request", request.IdempotencyKey)); lookupErr == nil {
		if existing.RequestDigest != requestDigest {
			return nil, false, ErrIdempotencyConflict
		}
		run, getErr := m.getHealthCheck(ctx, existing.ResourceID)
		return run, true, getErr
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return nil, false, lookupErr
	}
	decisionRequest := decision.Request{Class: decision.ActionDiagnostic, TargetDeviceID: request.DeviceID}
	if request.Profile == HealthCheckExtended {
		decisionRequest.MaintenanceRequired = true
		decisionRequest.AllowDuringMaintenance = true
		decisionRequest.ApprovalRequired = true
		decisionRequest.ApprovalGranted = request.ElevatedAuthorizationGranted && request.DisruptionBudgetApproved
		decisionRequest.ElevatedAuthorizationGranted = request.ElevatedAuthorizationGranted
		decisionRequest.DisruptionBudgetApproved = request.DisruptionBudgetApproved
	}
	snapshot, err := m.buildSnapshot(ctx, request.Node, decisionRequest)
	if err != nil {
		return nil, false, err
	}
	if err := requireSnapshotScope(snapshot, request.Tenant, request.Cluster); err != nil {
		return nil, false, err
	}
	if request.Profile != HealthCheckPassive && m.workflow == nil {
		return nil, false, fmt.Errorf("%w: diagnostic action queue is unavailable", ErrUnavailable)
	}
	// A quick/extended request must atomically observe the existing queued
	// runs and reserve its own slot before it can enqueue work. The public
	// mutation path is leader-fenced, and this mutex makes that check-and-write
	// sequence deterministic inside the elected controller process.
	m.diagnosticMu.Lock()
	defer m.diagnosticMu.Unlock()
	// A same-key retry may have waited behind the original request. Recheck
	// before testing capacity: the original queued run is its result, not a
	// reason to reject the retry as a second diagnostic.
	if existing, lookupErr := m.resources.GetOperationalIdempotency(ctx, types.ResourceHealthCheckRun, actor, scopedIdempotencyKey("request", request.IdempotencyKey)); lookupErr == nil {
		if existing.RequestDigest != requestDigest {
			return nil, false, ErrIdempotencyConflict
		}
		run, getErr := m.getHealthCheck(ctx, existing.ResourceID)
		return run, true, getErr
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return nil, false, lookupErr
	}
	if request.Profile != HealthCheckPassive {
		if err := m.checkDiagnosticConcurrency(ctx, request.Node); err != nil {
			return nil, false, err
		}
	}
	claimed, created, err := m.resources.PutOperationalIdempotency(ctx, reservation)
	if err != nil {
		return nil, false, err
	}
	if !created {
		if claimed.RequestDigest != requestDigest {
			return nil, false, ErrIdempotencyConflict
		}
		run, err := m.getHealthCheck(ctx, claimed.ResourceID)
		return run, true, err
	}

	snapshot.Limits.Deadline = deadline
	captured, err := m.captureDecision(ctx, actor, snapshot, request.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	run := &HealthCheckRun{
		ID: reservation.ResourceID, ResourceVersion: 1, Node: request.Node, DeviceID: request.DeviceID, Profile: request.Profile,
		Reason: request.Reason, Actor: actor, Tenant: request.Tenant, Cluster: request.Cluster,
		DecisionSnapshotID: captured.ID, Decision: captured.Decision,
		Deadline: deadline, CreatedAt: now, UpdatedAt: now,
	}
	var queuedAction *types.Action
	if request.Profile == HealthCheckPassive {
		run.State = "completed"
		run.Outcome = "captured existing evidence without device work"
		run.EvidenceDigest = evidenceDigest(captured.Snapshot)
		completed := now
		run.CompletedAt = &completed
	} else {
		if !captured.Decision.Permitted() {
			run.State = "blocked"
			run.Outcome = captured.Decision.HumanSummary
			completed := now
			run.CompletedAt = &completed
		} else {
			run.State = "queued"
			run.ActionID = "healthcheck/" + run.ID
			diagLevel := "1"
			if request.Profile == HealthCheckExtended {
				diagLevel = "2"
			}
			queuedAction = &types.Action{
				ID: run.ActionID, Type: types.ActionRunDiag, Timeout: deadline.Sub(now),
				Params: map[string]string{"diag_level": diagLevel, "health_check_id": run.ID, "device_id": request.DeviceID},
			}
		}
	}
	payload, err := marshalPayload(run)
	if err != nil {
		return nil, false, err
	}
	resource := &types.OperationalResource{
		Kind: types.ResourceHealthCheckRun, ID: run.ID, State: run.State, Actor: actor,
		Tenant: request.Tenant, Cluster: request.Cluster, ConfigDigest: run.Decision.ConfigDigest,
		Payload: payload, CreatedAt: now, UpdatedAt: now, ExpiresAt: &deadline, Version: run.ResourceVersion,
	}
	if err := m.resources.CreateOperationalResource(ctx, resource); err != nil {
		return nil, false, err
	}
	if err := m.audit(ctx, types.ResourceHealthCheckRun, run.ID, actor, "request", request.IdempotencyKey, captured.ID, map[string]string{"profile": string(run.Profile), "node": run.Node}, run.State); err != nil {
		return nil, false, err
	}
	if queuedAction != nil {
		// Persist the product record and its audit intent before an agent can
		// claim work. The earlier order enqueued first, which meant a storage
		// failure could leave an untraceable diagnostic action running on a
		// device. A queue failure now becomes a durable failed run instead.
		if err := m.workflow.EnqueueAction(ctx, request.Node, *queuedAction); err != nil {
			failedAt := m.now().UTC()
			run.State = "failed"
			run.Outcome = "diagnostic action could not be queued before dispatch"
			run.UpdatedAt, run.CompletedAt = failedAt, &failedAt
			run.ResourceVersion = resource.Version + 1
			failedPayload, marshalErr := marshalPayload(run)
			if marshalErr != nil {
				return nil, false, marshalErr
			}
			resource.State, resource.Payload = run.State, failedPayload
			if updateErr := m.resources.UpdateOperationalResource(ctx, resource, resource.Version); updateErr != nil {
				return nil, false, fmt.Errorf("queue health check: %w (and record failure: %v)", err, updateErr)
			}
			run.ResourceVersion = resource.Version
			if auditErr := m.audit(ctx, types.ResourceHealthCheckRun, run.ID, "system", "queue-failed", request.IdempotencyKey, captured.ID, nil, run.Outcome); auditErr != nil {
				return nil, false, fmt.Errorf("queue health check: %w (and audit failure: %v)", err, auditErr)
			}
			return run, false, fmt.Errorf("queue health check: %w", err)
		}
	}
	return run, false, nil
}

func (m *Manager) checkDiagnosticConcurrency(ctx context.Context, node string) error {
	resources, err := m.resources.ListOperationalResources(ctx, types.OperationalResourceFilter{
		Kind: types.ResourceHealthCheckRun, State: "queued", Limit: 500, IncludeExpired: true,
	})
	if err != nil {
		return err
	}
	fleet, nodeActive := 0, 0
	for _, resource := range resources {
		var run HealthCheckRun
		if err := json.Unmarshal(resource.Payload, &run); err != nil {
			return fmt.Errorf("health check %q is corrupt: %w", resource.ID, err)
		}
		fleet++
		if run.Node == node {
			nodeActive++
		}
	}
	if nodeActive >= maxNodeActiveDiagnostics {
		return fmt.Errorf("diagnostic concurrency limit reached for node %q", node)
	}
	if fleet >= maxFleetActiveDiagnostics {
		return fmt.Errorf("fleet diagnostic concurrency limit reached (%d)", maxFleetActiveDiagnostics)
	}
	return nil
}

func diagnosticDeadline(now time.Time, profile HealthCheckProfile, requested time.Time) (time.Time, error) {
	max := maxQuickDiagnosticTimeout
	if profile == HealthCheckExtended {
		max = maxExtendedDiagnosticTimeout
	}
	if requested.IsZero() {
		return now.Add(max), nil
	}
	requested = requested.UTC()
	if !requested.After(now) || requested.After(now.Add(max)) {
		return time.Time{}, fmt.Errorf("%s diagnostic deadline must be in the next %s", profile, max)
	}
	return requested, nil
}

func (m *Manager) getHealthCheck(ctx context.Context, id string) (*HealthCheckRun, error) {
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourceHealthCheckRun, id)
	if err != nil {
		return nil, err
	}
	var run HealthCheckRun
	if err := json.Unmarshal(resource.Payload, &run); err != nil {
		return nil, fmt.Errorf("health check %q is corrupt: %w", id, err)
	}
	run.ResourceVersion = resource.Version
	return &run, nil
}

func (m *Manager) GetHealthCheck(ctx context.Context, id string) (*HealthCheckRun, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	return m.getHealthCheck(ctx, id)
}

// ListNodeHealthChecks is retained for callers that only need the first page.
// New REST callers use ListNodeHealthChecksPage so an active node's oldest
// records cannot silently disappear after the shared store's 500-row scan cap.
func (m *Manager) ListNodeHealthChecks(ctx context.Context, node string, limit int) ([]*HealthCheckRun, error) {
	runs, _, _, err := m.ListNodeHealthChecksPage(ctx, node, OperationalListOptions{Limit: limit, IncludeExpired: true})
	return runs, err
}

// ListNodeHealthChecksPage filters a stable operational-resource stream by
// node while retaining the underlying (created_at,id) cursor. The cursor is
// the last returned match, rather than the last scanned non-match, so a resume
// never skips a health check interleaved with another node's history.
func (m *Manager) ListNodeHealthChecksPage(ctx context.Context, node string, options OperationalListOptions) ([]*HealthCheckRun, time.Time, string, error) {
	if err := m.requireResources(); err != nil {
		return nil, time.Time{}, "", err
	}
	if strings.TrimSpace(node) == "" {
		return nil, time.Time{}, "", fmt.Errorf("node is required")
	}
	if options.Limit <= 0 {
		options.Limit = 100
	}
	if options.Limit > 500 {
		return nil, time.Time{}, "", fmt.Errorf("health-check limit exceeds 500")
	}
	// Node is inside the normalized payload rather than an envelope column, so
	// scan in store-sized pages. This keeps the SQL shared by SQLite/PostgreSQL
	// portable while still providing a complete cursor-paged node history.
	scan := options
	scan.Limit = 500
	out := make([]*HealthCheckRun, 0)
	for {
		resources, err := m.listOperationalResources(ctx, types.ResourceHealthCheckRun, scan)
		if err != nil {
			return nil, time.Time{}, "", err
		}
		for _, resource := range resources {
			var run HealthCheckRun
			if err := json.Unmarshal(resource.Payload, &run); err != nil {
				return nil, time.Time{}, "", fmt.Errorf("health check %q is corrupt: %w", resource.ID, err)
			}
			run.ResourceVersion = resource.Version
			if run.Node != node {
				continue
			}
			if len(out) == options.Limit {
				last := out[len(out)-1]
				return out, last.CreatedAt, last.ID, nil
			}
			out = append(out, &run)
		}
		if len(resources) < scan.Limit {
			return out, time.Time{}, "", nil
		}
		last := resources[len(resources)-1]
		if !scan.AfterCreatedAt.IsZero() && (last.CreatedAt.Before(scan.AfterCreatedAt) || (last.CreatedAt.Equal(scan.AfterCreatedAt) && last.ID <= scan.AfterID)) {
			return nil, time.Time{}, "", fmt.Errorf("%w: health-check cursor did not advance", ErrIncompleteInventory)
		}
		scan.AfterCreatedAt, scan.AfterID = last.CreatedAt, last.ID
	}
}

// ListHealthChecks returns durable checks across a tenant/cluster page. Use
// ListNodeHealthChecks for the node drill-down route; this method is the
// console queue/export backing path.
func (m *Manager) ListHealthChecks(ctx context.Context, options OperationalListOptions) ([]*HealthCheckRun, error) {
	resources, err := m.listOperationalResources(ctx, types.ResourceHealthCheckRun, options)
	if err != nil {
		return nil, err
	}
	out := make([]*HealthCheckRun, 0, len(resources))
	for _, resource := range resources {
		var run HealthCheckRun
		if err := json.Unmarshal(resource.Payload, &run); err != nil {
			return nil, fmt.Errorf("health check %q is corrupt: %w", resource.ID, err)
		}
		run.ResourceVersion = resource.Version
		out = append(out, &run)
	}
	return out, nil
}

// CancelHealthCheck transitions only a queued run.  A leased action may have
// started on an agent already; that case remains running until its bounded
// outcome is observed instead of falsely reporting a cancellation.
func (m *Manager) CancelHealthCheck(ctx context.Context, actor, id string, expectedVersion int) (*HealthCheckRun, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourceHealthCheckRun, id)
	if err != nil {
		return nil, err
	}
	if expectedVersion > 0 && expectedVersion != resource.Version {
		return nil, store.ErrOperationalConflict
	}
	var run HealthCheckRun
	if err := json.Unmarshal(resource.Payload, &run); err != nil {
		return nil, err
	}
	if run.State != "queued" {
		return nil, fmt.Errorf("%w: health check %q is %s", ErrInvalidState, id, run.State)
	}
	canceller, ok := m.workflow.(store.PendingActionCanceller)
	if !ok {
		return nil, fmt.Errorf("%w: queue does not support pending-action cancellation", ErrUnavailable)
	}
	cancelled, err := canceller.CancelPendingAction(ctx, run.ActionID)
	if err != nil {
		return nil, err
	}
	if !cancelled {
		return nil, fmt.Errorf("%w: health check action is no longer pending", ErrInvalidState)
	}
	now := m.now().UTC()
	run.State, run.Outcome, run.UpdatedAt = "cancelled", "cancelled by operator before agent claim", now
	run.CompletedAt = &now
	run.ResourceVersion = resource.Version + 1
	payload, err := marshalPayload(run)
	if err != nil {
		return nil, err
	}
	resource.State, resource.Actor, resource.Payload = run.State, actor, payload
	if err := m.resources.UpdateOperationalResource(ctx, resource, resource.Version); err != nil {
		return nil, err
	}
	run.ResourceVersion = resource.Version
	if err := m.audit(ctx, types.ResourceHealthCheckRun, id, actor, "cancel", "", run.DecisionSnapshotID, nil, run.Outcome); err != nil {
		return nil, err
	}
	return &run, nil
}

// CancelHealthCheckRequest is the public idempotent cancellation operation.
// A retry after a successful cancellation returns the durable terminal run;
// it never tries to tombstone a leased agent action a second time.
func (m *Manager) CancelHealthCheckRequest(ctx context.Context, actor, id, idempotencyKey string, expectedVersion int) (*HealthCheckRun, bool, error) {
	if err := m.requireResources(); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(id) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return nil, false, fmt.Errorf("actor, health-check ID, and idempotency key are required")
	}
	digest := digestJSON(struct {
		ID              string `json:"id"`
		ExpectedVersion int    `json:"expected_version"`
		Action          string `json:"action"`
	}{ID: id, ExpectedVersion: expectedVersion, Action: "cancel"})
	storageKey := scopedIdempotencyKey("cancel", idempotencyKey)
	if existing, lookupErr := m.resources.GetOperationalIdempotency(ctx, types.ResourceHealthCheckRun, actor, storageKey); lookupErr == nil {
		if existing.RequestDigest != digest || existing.ResourceID != id {
			return nil, false, ErrIdempotencyConflict
		}
		run, getErr := m.getHealthCheck(ctx, id)
		if getErr != nil {
			return nil, false, getErr
		}
		if run.State != "cancelled" {
			return nil, false, fmt.Errorf("%w: prior health-check cancellation did not complete", ErrInvalidState)
		}
		return run, true, nil
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return nil, false, lookupErr
	}
	// Reject stale/terminal requests before reserving their key. A client can
	// then reread a run that was claimed by an agent and make an appropriate
	// next decision instead of being trapped behind a failed cancellation key.
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourceHealthCheckRun, id)
	if err != nil {
		return nil, false, err
	}
	if expectedVersion > 0 && expectedVersion != resource.Version {
		return nil, false, store.ErrOperationalConflict
	}
	var current HealthCheckRun
	if err := json.Unmarshal(resource.Payload, &current); err != nil {
		return nil, false, fmt.Errorf("health check %q is corrupt: %w", id, err)
	}
	if current.State != "queued" {
		return nil, false, fmt.Errorf("%w: health check %q is %s", ErrInvalidState, id, current.State)
	}
	if _, ok := m.workflow.(store.PendingActionCanceller); !ok {
		return nil, false, fmt.Errorf("%w: queue does not support pending-action cancellation", ErrUnavailable)
	}
	record, created, err := m.resources.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{
		Kind: types.ResourceHealthCheckRun, Actor: actor, Key: storageKey, RequestDigest: digest, ResourceID: id,
	})
	if err != nil {
		return nil, false, err
	}
	if !created {
		if record.RequestDigest != digest || record.ResourceID != id {
			return nil, false, ErrIdempotencyConflict
		}
		run, getErr := m.getHealthCheck(ctx, id)
		if getErr != nil {
			return nil, false, getErr
		}
		if run.State != "cancelled" {
			return nil, false, fmt.Errorf("%w: prior health-check cancellation did not complete", ErrInvalidState)
		}
		return run, true, nil
	}
	run, err := m.CancelHealthCheck(ctx, actor, id, expectedVersion)
	return run, false, err
}

// ReconcileHealthChecks observes action completion, cancellation and timeout.
// It performs no new device work; queue delivery remains owned by the agent
// protocol and global emergency-stop path.
func (m *Manager) ReconcileHealthChecks(ctx context.Context) error {
	if err := m.requireResources(); err != nil {
		return err
	}
	if m.workflow == nil {
		return nil
	}
	filter := types.OperationalResourceFilter{Kind: types.ResourceHealthCheckRun, State: "queued", Limit: 500, IncludeExpired: true}
	for {
		resources, err := m.resources.ListOperationalResources(ctx, filter)
		if err != nil {
			return err
		}
		for _, resource := range resources {
			var run HealthCheckRun
			if err := json.Unmarshal(resource.Payload, &run); err != nil {
				return fmt.Errorf("health check %q is corrupt: %w", resource.ID, err)
			}
			queued, err := m.workflow.GetAction(ctx, run.ActionID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			now := m.now().UTC()
			changed := false
			switch {
			case errors.Is(err, store.ErrNotFound):
				run.State, run.Outcome, changed = "failed", "diagnostic action record disappeared", true
			case queued.Cancelled:
				run.State, run.Outcome, changed = "cancelled", "cancelled by safety stop or operator", true
			case queued.Done:
				run.State, run.Outcome, run.EvidenceDigest = diagnosticActionResultSummary(queued.Result)
				changed = true
			case queued.Dead:
				run.State, run.Outcome, changed = "failed", "diagnostic action exhausted its lease retry budget", true
			case now.After(run.Deadline):
				if canceller, ok := m.workflow.(store.PendingActionCanceller); ok {
					_, _ = canceller.CancelPendingAction(ctx, run.ActionID)
				}
				run.State, run.Outcome, changed = "timed_out", "diagnostic deadline elapsed", true
			}
			if !changed {
				continue
			}
			run.UpdatedAt, run.CompletedAt = now, &now
			run.ResourceVersion = resource.Version + 1
			payload, err := marshalPayload(run)
			if err != nil {
				return err
			}
			resource.State, resource.Payload = run.State, payload
			if err := m.resources.UpdateOperationalResource(ctx, resource, resource.Version); err != nil {
				if errors.Is(err, store.ErrOperationalConflict) {
					continue
				}
				return err
			}
			run.ResourceVersion = resource.Version
			if err := m.audit(ctx, types.ResourceHealthCheckRun, run.ID, "system", "complete", "", run.DecisionSnapshotID, nil, run.Outcome); err != nil {
				return err
			}
		}
		if len(resources) < filter.Limit {
			return nil
		}
		last := resources[len(resources)-1]
		if !filter.AfterCreatedAt.IsZero() && (last.CreatedAt.Before(filter.AfterCreatedAt) || (last.CreatedAt.Equal(filter.AfterCreatedAt) && last.ID <= filter.AfterID)) {
			return fmt.Errorf("%w: health-check reconciliation cursor did not advance", ErrIncompleteInventory)
		}
		filter.AfterCreatedAt, filter.AfterID = last.CreatedAt, last.ID
	}
}

// diagnosticActionResultSummary intentionally does not copy command output or
// error prose into a generally readable HealthCheckRun. Agent diagnostics can
// contain workload names, process arguments, paths, and occasionally secret
// material. The queue remains the restricted raw-evidence store; this product
// summary carries a content digest and a concise, safe outcome instead.
func diagnosticActionResultSummary(result *types.ActionResult) (state, outcome, evidence string) {
	if result == nil {
		return "failed", "diagnostic action completed without a result", ""
	}
	blob, err := json.Marshal(result)
	if err == nil {
		evidence = digestBytes(blob)
	}
	if result.OK {
		return "completed", "diagnostic action completed; raw result retained as restricted evidence", evidence
	}
	combined := strings.ToLower(result.Error + "\n" + result.Output)
	if strings.Contains(combined, "unsupported") || strings.Contains(combined, "not supported") {
		return "failed", "diagnostic capability is unsupported by the reporting agent", evidence
	}
	return "failed", "diagnostic action failed; inspect restricted agent evidence by action ID", evidence
}

type SimulationRequest struct {
	Node           string                       `json:"node"`
	DeviceID       string                       `json:"device_id,omitempty"`
	Action         types.AcceleratorAction      `json:"action,omitempty"`
	Scope          types.AcceleratorTargetScope `json:"scope,omitempty"`
	Class          types.ProblemClass           `json:"class"`
	Severity       types.Severity               `json:"severity,omitempty"`
	Rationale      string                       `json:"rationale"`
	Tenant         string                       `json:"tenant,omitempty"`
	Cluster        string                       `json:"cluster,omitempty"`
	IdempotencyKey string                       `json:"-"`
}

type SimulationStep struct {
	ID               string   `json:"id"`
	Action           string   `json:"action"`
	DependsOn        []string `json:"depends_on,omitempty"`
	Lock             string   `json:"lock,omitempty"`
	ExpectedEvidence []string `json:"expected_evidence,omitempty"`
	WouldRun         bool     `json:"would_run"`
	Reason           string   `json:"reason,omitempty"`
}

type RemediationSimulation struct {
	ID              string `json:"id"`
	ResourceVersion int    `json:"resource_version"`
	Node            string `json:"node"`
	DeviceID        string `json:"device_id,omitempty"`
	// Action and Scope bind a later autonomy envelope to the exact simulated
	// hardware effect.  Older snapshots without these fields intentionally
	// cannot qualify a v0.4 autonomy plan; treating an observe-only simulation
	// as authority for a reset would violate the frozen-plan contract.
	Action             types.AcceleratorAction      `json:"action,omitempty"`
	Scope              types.AcceleratorTargetScope `json:"scope,omitempty"`
	Class              types.ProblemClass           `json:"class"`
	Severity           types.Severity               `json:"severity"`
	Rationale          string                       `json:"rationale"`
	Actor              string                       `json:"actor"`
	Tenant             string                       `json:"tenant,omitempty"`
	Cluster            string                       `json:"cluster,omitempty"`
	DecisionSnapshotID string                       `json:"decision_snapshot_id"`
	Decision           decision.Result              `json:"decision"`
	Steps              []SimulationStep             `json:"steps"`
	CreatedAt          time.Time                    `json:"created_at"`
	IncidentID         string                       `json:"incident_id,omitempty"`
}

func (m *Manager) CreateSimulation(ctx context.Context, actor string, request SimulationRequest) (*RemediationSimulation, bool, error) {
	if err := m.requireSnapshots(); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(request.Node) == "" || strings.TrimSpace(request.Rationale) == "" || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(string(request.Class)) == "" {
		return nil, false, fmt.Errorf("actor, node, class, rationale, and idempotency key are required")
	}
	if request.Severity == "" {
		request.Severity = types.SeverityWarning
	}
	request.Tenant, request.Cluster = strings.TrimSpace(request.Tenant), strings.TrimSpace(request.Cluster)
	digest := digestJSON(struct {
		Node, DeviceID, Action, Scope, Class, Severity, Rationale string
		Tenant, Cluster                                           string
	}{request.Node, request.DeviceID, string(request.Action), string(request.Scope), string(request.Class), string(request.Severity), request.Rationale, strings.TrimSpace(request.Tenant), strings.TrimSpace(request.Cluster)})
	reservation := &types.OperationalIdempotencyRecord{
		Kind: types.ResourceRemediationSimulation, Actor: actor, Key: scopedIdempotencyKey("simulate", request.IdempotencyKey),
		RequestDigest: digest, ResourceID: newID("simulation"),
	}
	if existing, lookupErr := m.resources.GetOperationalIdempotency(ctx, types.ResourceRemediationSimulation, actor, scopedIdempotencyKey("simulate", request.IdempotencyKey)); lookupErr == nil {
		if existing.RequestDigest != digest {
			return nil, false, ErrIdempotencyConflict
		}
		simulation, getErr := m.getSimulation(ctx, existing.ResourceID)
		return simulation, true, getErr
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return nil, false, lookupErr
	}
	snapshot, err := m.buildSnapshot(ctx, request.Node, decision.Request{
		Class: decision.ActionSimulate, AcceleratorAction: request.Action, Scope: request.Scope, TargetDeviceID: request.DeviceID,
	})
	if err != nil {
		return nil, false, err
	}
	if err := requireSnapshotScope(snapshot, request.Tenant, request.Cluster); err != nil {
		return nil, false, err
	}
	claimed, created, err := m.resources.PutOperationalIdempotency(ctx, reservation)
	if err != nil {
		return nil, false, err
	}
	if !created {
		if claimed.RequestDigest != digest {
			return nil, false, ErrIdempotencyConflict
		}
		simulation, err := m.getSimulation(ctx, claimed.ResourceID)
		return simulation, true, err
	}
	captured, err := m.captureDecision(ctx, actor, snapshot, request.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	simulation := &RemediationSimulation{
		ID: reservation.ResourceID, ResourceVersion: 1, Node: request.Node, DeviceID: request.DeviceID,
		Action: request.Action, Scope: request.Scope, Class: request.Class,
		Severity: request.Severity, Rationale: request.Rationale, Actor: actor,
		Tenant: strings.TrimSpace(request.Tenant), Cluster: strings.TrimSpace(request.Cluster), DecisionSnapshotID: captured.ID,
		Decision: captured.Decision, Steps: simulationGraph(captured.Decision, request), CreatedAt: m.now().UTC(),
	}
	payload, err := marshalPayload(simulation)
	if err != nil {
		return nil, false, err
	}
	state := "permitted"
	if !simulation.Decision.Permitted() {
		state = "blocked"
	}
	if err := m.resources.CreateOperationalResource(ctx, &types.OperationalResource{
		Kind: types.ResourceRemediationSimulation, ID: simulation.ID, State: state, Actor: actor,
		Tenant: request.Tenant, Cluster: request.Cluster, ConfigDigest: simulation.Decision.ConfigDigest,
		Payload: payload, CreatedAt: simulation.CreatedAt, UpdatedAt: simulation.CreatedAt, ExpiresAt: &captured.Decision.ExpiresAt, Version: simulation.ResourceVersion,
	}); err != nil {
		return nil, false, err
	}
	if err := m.audit(ctx, types.ResourceRemediationSimulation, simulation.ID, actor, "simulate", request.IdempotencyKey, captured.ID, map[string]string{"node": request.Node}, state); err != nil {
		return nil, false, err
	}
	return simulation, false, nil
}

func simulationGraph(result decision.Result, request SimulationRequest) []SimulationStep {
	wouldRun := result.Permitted()
	steps := []SimulationStep{
		{ID: "capture-decision", Action: "capture immutable evidence", ExpectedEvidence: []string{"node identity", "runtime report", "effective config digest"}, WouldRun: true},
		{ID: "evaluate", Action: "evaluate safety and capability gates", DependsOn: []string{"capture-decision"}, Lock: "decision:" + request.Node, WouldRun: true, Reason: result.HumanSummary},
	}
	if request.Action != "" {
		steps = append(steps,
			SimulationStep{ID: "acquire-ownership", Action: "acquire node/device ownership lease", DependsOn: []string{"evaluate"}, Lock: "target:" + request.Node + "/" + request.DeviceID, WouldRun: wouldRun, Reason: result.HumanSummary},
			SimulationStep{ID: "dispatch", Action: string(request.Action), DependsOn: []string{"acquire-ownership"}, Lock: "action:" + string(request.Action), ExpectedEvidence: []string{"fresh evidence", "approval", "scope"}, WouldRun: wouldRun, Reason: result.HumanSummary},
			SimulationStep{ID: "verify", Action: "verify post-action evidence", DependsOn: []string{"dispatch"}, ExpectedEvidence: []string{"fresh agent report"}, WouldRun: wouldRun},
		)
	}
	return steps
}

func (m *Manager) getSimulation(ctx context.Context, id string) (*RemediationSimulation, error) {
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourceRemediationSimulation, id)
	if err != nil {
		return nil, err
	}
	var simulation RemediationSimulation
	if err := json.Unmarshal(resource.Payload, &simulation); err != nil {
		return nil, fmt.Errorf("simulation %q is corrupt: %w", id, err)
	}
	simulation.ResourceVersion = resource.Version
	return &simulation, nil
}

func (m *Manager) GetSimulation(ctx context.Context, id string) (*RemediationSimulation, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	return m.getSimulation(ctx, id)
}

// ListSimulations returns a stable review/export page. A simulation remains
// immutable even after it has spawned an incident; IncidentID is therefore a
// link, never a mutable rewrite of its frozen decision evidence.
func (m *Manager) ListSimulations(ctx context.Context, options OperationalListOptions) ([]*RemediationSimulation, error) {
	resources, err := m.listOperationalResources(ctx, types.ResourceRemediationSimulation, options)
	if err != nil {
		return nil, err
	}
	out := make([]*RemediationSimulation, 0, len(resources))
	for _, resource := range resources {
		var simulation RemediationSimulation
		if err := json.Unmarshal(resource.Payload, &simulation); err != nil {
			return nil, fmt.Errorf("simulation %q is corrupt: %w", resource.ID, err)
		}
		simulation.ResourceVersion = resource.Version
		out = append(out, &simulation)
	}
	return out, nil
}

// CreateIncidentFromSimulation is the sole bridge from a simulation to the
// live incident workflow.  It records the originating simulation and frozen
// decision in immutable operational audit data before returning the incident.
func (m *Manager) CreateIncidentFromSimulation(ctx context.Context, actor, simulationID, rationale string, expectedVersion int) (*types.Incident, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	if m.createIncident == nil {
		return nil, fmt.Errorf("%w: incident creator is unavailable", ErrUnavailable)
	}
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourceRemediationSimulation, simulationID)
	if err != nil {
		return nil, err
	}
	if expectedVersion > 0 && expectedVersion != resource.Version {
		return nil, store.ErrOperationalConflict
	}
	var simulation RemediationSimulation
	if err := json.Unmarshal(resource.Payload, &simulation); err != nil {
		return nil, err
	}
	if simulation.IncidentID != "" {
		if m.getIncident != nil {
			return m.getIncident(ctx, simulation.IncidentID)
		}
		return nil, fmt.Errorf("%w: simulation already created incident %s", ErrInvalidState, simulation.IncidentID)
	}
	if !simulation.Decision.Permitted() {
		return nil, fmt.Errorf("%w: simulation is blocked: %s", ErrInvalidState, simulation.Decision.HumanSummary)
	}
	if strings.TrimSpace(rationale) == "" {
		return nil, fmt.Errorf("incident rationale is required")
	}
	incident, err := m.createIncident(ctx, types.Signal{
		Target: types.Target{Node: simulation.Node, GPUUUID: simulation.DeviceID}, Class: simulation.Class,
		Severity: simulation.Severity, Source: types.SourceManual, ObservedAt: m.now().UTC(),
		Evidence: map[string]string{"actor": actor, "trigger": "simulation", "simulation_id": simulation.ID, "rationale": rationale},
	})
	if err != nil {
		return nil, err
	}
	simulation.IncidentID = incident.ID
	simulation.ResourceVersion = resource.Version + 1
	payload, err := marshalPayload(simulation)
	if err != nil {
		return nil, err
	}
	resource.State, resource.Actor, resource.Payload = "incident-created", actor, payload
	if err := m.resources.UpdateOperationalResource(ctx, resource, resource.Version); err != nil {
		return nil, err
	}
	simulation.ResourceVersion = resource.Version
	if err := m.audit(ctx, types.ResourceRemediationSimulation, simulation.ID, actor, "create-incident", "", simulation.DecisionSnapshotID, map[string]string{"incident_id": incident.ID, "rationale": rationale}, "created"); err != nil {
		return nil, err
	}
	return incident, nil
}

// CreateIncidentFromSimulationRequest makes the simulation bridge safe under
// browser retry and client disconnect. The request key is bound to the frozen
// simulation version and rationale; a changed rationale is a different audit
// decision and is refused rather than silently attached to the old incident.
func (m *Manager) CreateIncidentFromSimulationRequest(ctx context.Context, actor, simulationID, rationale, idempotencyKey string, expectedVersion int) (*types.Incident, bool, error) {
	if err := m.requireResources(); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(simulationID) == "" || strings.TrimSpace(rationale) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return nil, false, fmt.Errorf("actor, simulation ID, rationale, and idempotency key are required")
	}
	digest := digestJSON(struct {
		SimulationID    string `json:"simulation_id"`
		Rationale       string `json:"rationale"`
		ExpectedVersion int    `json:"expected_version"`
	}{SimulationID: simulationID, Rationale: rationale, ExpectedVersion: expectedVersion})
	storageKey := scopedIdempotencyKey("create-incident", idempotencyKey)
	if existing, lookupErr := m.resources.GetOperationalIdempotency(ctx, types.ResourceRemediationSimulation, actor, storageKey); lookupErr == nil {
		if existing.RequestDigest != digest || existing.ResourceID != simulationID {
			return nil, false, ErrIdempotencyConflict
		}
		simulation, getErr := m.getSimulation(ctx, simulationID)
		if getErr != nil {
			return nil, false, getErr
		}
		if simulation.IncidentID == "" || m.getIncident == nil {
			return nil, false, fmt.Errorf("%w: original simulation-to-incident request did not complete", ErrInvalidState)
		}
		incident, getErr := m.getIncident(ctx, simulation.IncidentID)
		return incident, true, getErr
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return nil, false, lookupErr
	}
	// Validate the frozen object before reserving a retry key. A blocked,
	// stale-version simulation is a normal review outcome, not a completed
	// mutation; reserving it would make the corrected request impossible to
	// submit with the same client retry key.
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourceRemediationSimulation, simulationID)
	if err != nil {
		return nil, false, err
	}
	if expectedVersion > 0 && expectedVersion != resource.Version {
		return nil, false, store.ErrOperationalConflict
	}
	var simulation RemediationSimulation
	if err := json.Unmarshal(resource.Payload, &simulation); err != nil {
		return nil, false, fmt.Errorf("simulation %q is corrupt: %w", simulationID, err)
	}
	if simulation.IncidentID != "" {
		if m.getIncident == nil {
			return nil, false, fmt.Errorf("%w: simulation already created incident %s", ErrInvalidState, simulation.IncidentID)
		}
		incident, getErr := m.getIncident(ctx, simulation.IncidentID)
		return incident, false, getErr
	}
	if m.createIncident == nil {
		return nil, false, fmt.Errorf("%w: incident creator is unavailable", ErrUnavailable)
	}
	if !simulation.Decision.Permitted() {
		return nil, false, fmt.Errorf("%w: simulation is blocked: %s", ErrInvalidState, simulation.Decision.HumanSummary)
	}
	record, created, err := m.resources.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{
		Kind: types.ResourceRemediationSimulation, Actor: actor, Key: storageKey, RequestDigest: digest, ResourceID: simulationID,
	})
	if err != nil {
		return nil, false, err
	}
	if !created {
		if record.RequestDigest != digest || record.ResourceID != simulationID {
			return nil, false, ErrIdempotencyConflict
		}
		stored, getErr := m.getSimulation(ctx, simulationID)
		if getErr != nil {
			return nil, false, getErr
		}
		if stored.IncidentID == "" || m.getIncident == nil {
			return nil, false, fmt.Errorf("%w: original simulation-to-incident request did not complete", ErrInvalidState)
		}
		incident, getErr := m.getIncident(ctx, stored.IncidentID)
		return incident, true, getErr
	}
	incident, err := m.CreateIncidentFromSimulation(ctx, actor, simulationID, rationale, expectedVersion)
	return incident, false, err
}

// ListAuditEvents exposes append-only operational events to the API/console.
// The store query is cursor-paged and never returns raw diagnostic payloads.
func (m *Manager) ListAuditEvents(ctx context.Context, filter types.OperationalAuditFilter) ([]*types.OperationalAuditEvent, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	return m.resources.ListOperationalAuditEvents(ctx, filter)
}

func (m *Manager) audit(ctx context.Context, kind types.OperationalResourceKind, resourceID, actor, action, requestID, decisionID string, params map[string]string, result string) error {
	return m.resources.AppendOperationalAudit(ctx, &types.OperationalAuditEvent{
		Kind: kind, ResourceID: resourceID, Time: m.now().UTC(), Actor: actor, Action: action,
		RequestID: requestID, DecisionID: decisionID, Params: params, Result: result,
	})
}

func newID(prefix string) string { return prefix + "-" + uuid.NewString() }

func marshalPayload(value any) (json.RawMessage, error) {
	blob, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(blob), nil
}

func digestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func digestJSON(value any) string {
	blob, _ := json.Marshal(value)
	return digestBytes(blob)
}

// scopedIdempotencyKey keeps the caller-visible Idempotency-Key local to one
// operation.  Reusing "retry-1" for a candidate upload and a later logical
// revocation is normal client behaviour; sharing one storage partition for
// those unrelated effects would turn that safe reuse into a false conflict.
// The raw key still appears in the audit request_id so an operator can trace
// the exact value the client sent.
func scopedIdempotencyKey(operation, key string) string {
	return operation + ":" + key
}

func evidenceDigest(snapshot decision.Snapshot) string {
	if snapshot.Report == nil {
		return ""
	}
	blob, _ := json.Marshal(snapshot.Report)
	return digestBytes(blob)
}
