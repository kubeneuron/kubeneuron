package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// RuntimeContractQualificationVersion identifies the persisted shape and
// lifecycle semantics of a RuntimeContractQualification payload. It changes
// whenever a stored qualification could be interpreted differently by a newer
// reader, exactly like config.RuntimeContractCoverageVersion.
const RuntimeContractQualificationVersion = "runtime-contract-qualification/v1"

const (
	// maxQualificationCohort bounds one qualification to a reviewable cohort.
	// A fleet-wide rollout is a sequence of qualifications, not one giant one.
	maxQualificationCohort = 32
	// maxQualificationSamples and maxQualificationDuration bound how much
	// sustained evidence one qualification may demand, so a typo cannot create a
	// resource that can only ever expire.
	maxQualificationSamples  = 1000
	maxQualificationDuration = 14 * 24 * time.Hour
	// maxQualificationLifetime bounds the retention anchor of the resource.
	maxQualificationLifetime = 30 * 24 * time.Hour
	// qualificationExpiryMargin is the minimum slack between the earliest
	// possible ready instant (creation plus the minimum duration) and expiry. A
	// qualification whose window ends at the exact moment it could become ready
	// is a resource designed to expire, so it is rejected up front.
	qualificationExpiryMargin = 5 * time.Minute
	// maxQualificationObservations bounds the per-node coverage history kept in
	// the payload. Counters and the initial evidence are never pruned. Each
	// observation is also appended to the audit chain in the same transaction
	// as the counter update (see Observe), so the chain lists every committed
	// observation even after the payload window has been pruned.
	maxQualificationObservations = 48
	// maxQualificationDriftReasons bounds the invalidation explanation.
	maxQualificationDriftReasons = 8
	// qualificationTenantLabel and qualificationClusterLabel are the
	// controller-owned node labels a qualification's scope is derived from.
	// They are the same labels operationalNodeScopeMatches consults.
	qualificationTenantLabel  = "kubeneuron.io/tenant"
	qualificationClusterLabel = "kubeneuron.io/cluster"
)

// RuntimeContractQualificationState is deliberately minimal. There is no
// approved or promoted state here: a ReadyForApproval qualification is
// evidence a human may act on through a separate, later workflow. Nothing in
// this package consumes it as authority.
type RuntimeContractQualificationState string

const (
	QualificationObserving        RuntimeContractQualificationState = "Observing"
	QualificationReadyForApproval RuntimeContractQualificationState = "ReadyForApproval"
	QualificationInvalidated      RuntimeContractQualificationState = "Invalidated"
	QualificationExpired          RuntimeContractQualificationState = "Expired"
)

func (s RuntimeContractQualificationState) terminal() bool {
	return s == QualificationInvalidated || s == QualificationExpired
}

// RuntimeContractQualificationRequirements is the explicit, bounded evidence
// bar. Both must be met: MinSamples successful observations, and MinDuration
// elapsed since the first successful observation.
type RuntimeContractQualificationRequirements struct {
	MinSamples  int           `json:"min_samples"`
	MinDuration time.Duration `json:"min_duration"`
}

// RuntimeContractQualificationRequest is the API input. Nodes is an explicit
// cohort; the manager freezes each node's current UID itself and never trusts
// a caller-supplied UID or profile digest. Tenant and Cluster are optional
// assertions: when supplied they must equal the controller-owned scope labels
// of every cohort node, but the persisted scope is always derived from the
// labels, never copied from the request.
type RuntimeContractQualificationRequest struct {
	Nodes          []string                                 `json:"nodes"`
	Vendor         types.AcceleratorVendor                  `json:"vendor"`
	Tenant         string                                   `json:"tenant,omitempty"`
	Cluster        string                                   `json:"cluster,omitempty"`
	Requirements   RuntimeContractQualificationRequirements `json:"requirements"`
	ExpiresAt      time.Time                                `json:"expires_at"`
	IdempotencyKey string                                   `json:"-"`
}

// RuntimeContractQualificationMutation binds an observation to the version
// the caller last read and to a request idempotency key.
type RuntimeContractQualificationMutation struct {
	ExpectedVersion int    `json:"resource_version,omitempty"`
	IdempotencyKey  string `json:"-"`
}

// RuntimeContractQualificationNode is one frozen cohort member. UID is the
// platform identity at creation; a recreated node with the same name is drift.
type RuntimeContractQualificationNode struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}

// RuntimeContractQualificationProfile is the selected profile identity every
// cohort member was bound to at creation.
type RuntimeContractQualificationProfile struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
	Digest     string `json:"digest"`
}

// RuntimeContractQualificationNodeCoverage is the compact per-node evidence
// retained for one observation. It carries the coverage axes and the bindings
// the sample was judged against rather than the full coverage report, so a
// bounded history stays small enough to persist and export.
type RuntimeContractQualificationNodeCoverage struct {
	Node              string                                  `json:"node"`
	NodeUID           string                                  `json:"node_uid"`
	ConfigDigest      string                                  `json:"config_digest"`
	Profile           RuntimeContractQualificationProfile     `json:"profile"`
	Selection         config.RuntimeContractSelection         `json:"selection"`
	Attestation       config.RuntimeContractAttestation       `json:"attestation"`
	VerificationDepth config.RuntimeContractVerificationDepth `json:"verification_depth"`
	Reasons           []config.RuntimeContractReasonCode      `json:"reasons,omitempty"`
	ReportObservedAt  time.Time                               `json:"report_observed_at,omitzero"`
}

// RuntimeContractQualificationObservation is one recorded observation of the
// whole cohort. Successful is true only when every node was Exact,
// FreshCompatible, and Full against unchanged bindings. Drift lists the
// binding violations that invalidated the qualification; Coverage is empty
// when inventory drift made per-node coverage meaningless. Degraded lists the
// nodes whose bindings held but whose evidence was not Full; after
// ReadyForApproval such a regression also invalidates.
type RuntimeContractQualificationObservation struct {
	At         time.Time                                  `json:"at"`
	Actor      string                                     `json:"actor"`
	Successful bool                                       `json:"successful"`
	Coverage   []RuntimeContractQualificationNodeCoverage `json:"coverage,omitempty"`
	Drift      []string                                   `json:"drift,omitempty"`
	Degraded   []string                                   `json:"degraded,omitempty"`
}

// RuntimeContractQualification is the durable, observation-only record. It is
// not authority: admission, incidents, action execution, and GPUAutonomyPlan
// never read it. Bindings frozen at creation (profile identity, config digest,
// cohort UIDs, scope) are immutable; any later change invalidates permanently.
// Tenant and Cluster are the exact controller-owned label values shared by the
// whole cohort at creation, possibly empty when the labels were absent.
type RuntimeContractQualification struct {
	Version         string                                   `json:"version"`
	ID              string                                   `json:"id"`
	ResourceVersion int                                      `json:"resource_version"`
	State           RuntimeContractQualificationState        `json:"state"`
	Vendor          types.AcceleratorVendor                  `json:"vendor"`
	Profile         RuntimeContractQualificationProfile      `json:"profile"`
	ConfigDigest    string                                   `json:"config_digest"`
	Cohort          []RuntimeContractQualificationNode       `json:"cohort"`
	Requirements    RuntimeContractQualificationRequirements `json:"requirements"`
	Tenant          string                                   `json:"tenant,omitempty"`
	Cluster         string                                   `json:"cluster,omitempty"`
	ExpiresAt       time.Time                                `json:"expires_at"`
	// InitialCoverage is the immutable evidence captured at creation. It is
	// not a sample: qualification evidence accrues only through Observe.
	InitialCoverage []RuntimeContractQualificationNodeCoverage `json:"initial_coverage"`
	// Observations retains the most recent bounded window in append order.
	Observations            []RuntimeContractQualificationObservation `json:"observations,omitempty"`
	TotalObservations       int                                       `json:"total_observations"`
	SuccessfulSamples       int                                       `json:"successful_samples"`
	FirstSuccessfulSampleAt *time.Time                                `json:"first_successful_sample_at,omitempty"`
	LastObservedAt          *time.Time                                `json:"last_observed_at,omitempty"`
	ReadyAt                 *time.Time                                `json:"ready_at,omitempty"`
	InvalidatedAt           *time.Time                                `json:"invalidated_at,omitempty"`
	InvalidationReason      string                                    `json:"invalidation_reason,omitempty"`
	ExpiredAt               *time.Time                                `json:"expired_at,omitempty"`
	Actor                   string                                    `json:"actor"`
	CreatedAt               time.Time                                 `json:"created_at"`
	UpdatedAt               time.Time                                 `json:"updated_at"`
}

// qualificationStore returns the operational store as the transactional
// capability every qualification write requires. A qualification mutation is
// only correct when its idempotency claim, its resource write, and its audit
// events commit together, so an OperationalStore that cannot offer that is
// treated as unavailable rather than written to piecemeal. Reads
// (Get/List) need only the plain store and keep using requireResources.
func (m *Manager) qualificationStore() (store.OperationalTransactionalStore, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	transactional, ok := m.resources.(store.OperationalTransactionalStore)
	if !ok {
		return nil, fmt.Errorf("%w: operational store cannot commit qualification writes atomically", ErrUnavailable)
	}
	return transactional, nil
}

func (m *Manager) requireQualificationDependencies() (store.OperationalTransactionalStore, error) {
	resources, err := m.qualificationStore()
	if err != nil {
		return nil, err
	}
	if m.listNodes == nil {
		return nil, fmt.Errorf("%w: node inventory lister is unavailable", ErrUnavailable)
	}
	return resources, m.requireRuntimeContractCoverage()
}

// CreateRuntimeContractQualification freezes an explicit node cohort against
// its currently selected runtime profile and starts observing it. Every input
// is validated and the initial coverage is captured before the idempotency key
// is reserved, so an incomplete inventory or a binding mismatch never poisons
// a retry key. Creation succeeds with less than Full coverage: the qualification
// records what it saw and only Observe can accumulate qualifying samples.
//
// The key reservation, the resource insert, and the create audit event are one
// transaction: either the caller gets back a qualification whose key, row, and
// audit chain all exist, or nothing was written and the same key may be
// retried. A replay of an already committed key returns that qualification.
func (m *Manager) CreateRuntimeContractQualification(ctx context.Context, actor string, request RuntimeContractQualificationRequest) (*RuntimeContractQualification, bool, error) {
	resources, err := m.requireQualificationDependencies()
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return nil, false, fmt.Errorf("actor and idempotency key are required")
	}
	now := m.now().UTC()
	names, err := normalizeQualificationRequest(&request, now)
	if err != nil {
		return nil, false, err
	}
	inventory, err := m.qualificationInventory(ctx)
	if err != nil {
		return nil, false, err
	}
	// Scope is derived from controller-owned labels and frozen, never copied
	// from the request. Every member must carry identical tenant and cluster
	// label values (both may be empty, but only if they are empty everywhere),
	// and a supplied request scope must agree with them. A cohort that spans
	// scopes is rejected even when the request asserted no scope at all.
	cohort := make([]RuntimeContractQualificationNode, 0, len(names))
	var tenant, cluster string
	for index, name := range names {
		node, ok := inventory[name]
		if !ok {
			return nil, false, fmt.Errorf("%w: node %q is not in the managed inventory", ErrIncompleteInventory, name)
		}
		nodeTenant, nodeCluster := qualificationNodeScope(node)
		if (request.Tenant != "" && nodeTenant != request.Tenant) || (request.Cluster != "" && nodeCluster != request.Cluster) {
			return nil, false, fmt.Errorf("%w: node %q is outside requested tenant/cluster scope", ErrInvalidState, name)
		}
		if index == 0 {
			tenant, cluster = nodeTenant, nodeCluster
		} else if nodeTenant != tenant || nodeCluster != cluster {
			return nil, false, fmt.Errorf("%w: node %q has tenant/cluster %q/%q; cohort must share the scope %q/%q of node %q",
				ErrInvalidState, name, nodeTenant, nodeCluster, tenant, cluster, names[0])
		}
		uid, err := qualificationNodeUID(node)
		if err != nil {
			return nil, false, err
		}
		cohort = append(cohort, RuntimeContractQualificationNode{Name: name, UID: uid})
	}
	initial, err := m.captureQualificationCoverage(ctx, request.Vendor, cohort)
	if err != nil {
		return nil, false, err
	}
	// Every member must bind exactly, and to the same profile and compiled
	// configuration. A cohort that spans two profiles would qualify neither.
	var profile RuntimeContractQualificationProfile
	var configDigest string
	for index, coverage := range initial {
		if coverage.Selection != config.RuntimeContractSelectionExact {
			return nil, false, fmt.Errorf("%w: node %q has %s profile selection; qualification requires Exact", ErrInvalidState, coverage.Node, coverage.Selection)
		}
		if !qualificationProfileIdentityComplete(coverage.Profile) || !canonicalQualificationIdentity(coverage.ConfigDigest) {
			return nil, false, fmt.Errorf("%w: node %q selected a profile without an exact, complete identity (unpadded nonblank name, uid, and digest; positive generation) or an exact config digest", ErrIncompleteInventory, coverage.Node)
		}
		if index == 0 {
			profile, configDigest = coverage.Profile, coverage.ConfigDigest
			continue
		}
		if coverage.Profile != profile || coverage.ConfigDigest != configDigest {
			return nil, false, fmt.Errorf("%w: node %q binds to profile %q uid %q generation %d config %q; cohort must share profile %q uid %q generation %d config %q",
				ErrInvalidState, coverage.Node, coverage.Profile.Name, coverage.Profile.UID, coverage.Profile.Generation, coverage.ConfigDigest,
				profile.Name, profile.UID, profile.Generation, configDigest)
		}
	}

	requestDigest := digestJSON(struct {
		Nodes        []string                                 `json:"nodes"`
		Vendor       types.AcceleratorVendor                  `json:"vendor"`
		Tenant       string                                   `json:"tenant"`
		Cluster      string                                   `json:"cluster"`
		Requirements RuntimeContractQualificationRequirements `json:"requirements"`
		ExpiresAt    time.Time                                `json:"expires_at"`
	}{names, request.Vendor, request.Tenant, request.Cluster, request.Requirements, request.ExpiresAt})
	qualification := &RuntimeContractQualification{
		Version: RuntimeContractQualificationVersion, ID: newID("rcq"), ResourceVersion: 1, State: QualificationObserving,
		Vendor: request.Vendor, Profile: profile, ConfigDigest: configDigest, Cohort: cohort, Requirements: request.Requirements,
		Tenant: tenant, Cluster: cluster, ExpiresAt: request.ExpiresAt, InitialCoverage: initial,
		Actor: actor, CreatedAt: now, UpdatedAt: now,
	}
	payload, err := marshalPayload(qualification)
	if err != nil {
		return nil, false, err
	}
	expires := qualification.ExpiresAt
	resource := &types.OperationalResource{
		Kind: types.ResourceRuntimeContractQualification, ID: qualification.ID, State: string(qualification.State), Actor: actor,
		Tenant: qualification.Tenant, Cluster: qualification.Cluster, ConfigDigest: configDigest, Payload: payload,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, Version: qualification.ResourceVersion,
	}
	createParams := map[string]string{
		"vendor": string(request.Vendor), "profile_name": profile.Name, "profile_uid": profile.UID,
		"profile_generation": fmt.Sprint(profile.Generation), "profile_digest": profile.Digest, "config_digest": configDigest,
		"cohort_size": fmt.Sprint(len(cohort)), "initially_full": fmt.Sprint(qualificationCoverageFull(initial)),
		"tenant": tenant, "cluster": cluster,
	}

	// Everything below is one transaction. A failed insert or audit append
	// rolls the key claim back with it, so a transient store failure leaves
	// nothing for a retry of the same key to replay.
	var replayed *RuntimeContractQualification
	err = resources.WithOperationalTx(ctx, func(tx store.OperationalTx) error {
		claimed, created, err := tx.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{
			Kind: types.ResourceRuntimeContractQualification, Actor: actor, Key: scopedIdempotencyKey("create", request.IdempotencyKey),
			RequestDigest: requestDigest, ResourceID: qualification.ID,
		})
		if err != nil {
			return err
		}
		if !created {
			if claimed.RequestDigest != requestDigest {
				return ErrIdempotencyConflict
			}
			// A committed key always has its qualification: they were written
			// together.
			replayed, err = getRuntimeContractQualification(ctx, tx, claimed.ResourceID)
			return err
		}
		if err := tx.CreateOperationalResource(ctx, resource); err != nil {
			return err
		}
		return m.auditThrough(ctx, tx, types.ResourceRuntimeContractQualification, qualification.ID, actor, "create", request.IdempotencyKey, "", createParams, string(qualification.State))
	})
	if err != nil {
		return nil, false, err
	}
	if replayed != nil {
		return replayed, true, nil
	}
	return qualification, false, nil
}

// qualificationNodeScope reads the exact controller-owned scope label values
// of a node. An absent label reads as empty; the values are deliberately not
// trimmed so a frozen value can only ever be matched by the identical label.
func qualificationNodeScope(node *types.Node) (tenant, cluster string) {
	if node == nil {
		return "", ""
	}
	return node.Labels[qualificationTenantLabel], node.Labels[qualificationClusterLabel]
}

// canonicalQualificationIdentity reports whether value is a nonblank identity
// string with no leading or trailing whitespace. Every frozen identity axis
// (node UID, profile name, UID and digest, config digest) is later compared
// exactly, so a padded value could only ever match itself. Trimming it into
// a match at creation would persist an identity the platform never reported;
// persisting it padded would freeze a value nothing canonical can ever equal.
// Either way the read is inconsistent evidence and is rejected instead.
func canonicalQualificationIdentity(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

// qualificationNodeUID returns the exact platform UID of an inventory node. A
// blank or whitespace-padded UID is an incomplete inventory in every phase:
// creation never trims it into a frozen identity, and Observe never mistakes
// it for a recreated node or trims it into a match with the frozen UID.
func qualificationNodeUID(node *types.Node) (string, error) {
	if !canonicalQualificationIdentity(node.UID) {
		return "", fmt.Errorf("%w: node %q has no exact platform UID (inventory reports %q)", ErrIncompleteInventory, node.Name, node.UID)
	}
	return node.UID, nil
}

// qualificationProfileIdentityComplete requires every axis of the profile
// identity a later Observe compares against to be present and exact. A zero
// generation or a blank digest would make profile drift undetectable, and a
// padded axis could never be matched by the canonical value, so both are
// incomplete evidence.
func qualificationProfileIdentityComplete(profile RuntimeContractQualificationProfile) bool {
	return canonicalQualificationIdentity(profile.Name) && canonicalQualificationIdentity(profile.UID) &&
		profile.Generation > 0 && canonicalQualificationIdentity(profile.Digest)
}

// normalizeQualificationRequest trims and bounds the request and returns the
// cohort names in stable order. The request's own slice is never sorted in
// place so a caller-owned slice is not mutated.
func normalizeQualificationRequest(request *RuntimeContractQualificationRequest, now time.Time) ([]string, error) {
	if err := validRuntimeContractVendor(request.Vendor); err != nil {
		return nil, err
	}
	request.Tenant, request.Cluster = strings.TrimSpace(request.Tenant), strings.TrimSpace(request.Cluster)
	if len(request.Nodes) == 0 {
		return nil, fmt.Errorf("qualification requires at least one node")
	}
	if len(request.Nodes) > maxQualificationCohort {
		return nil, fmt.Errorf("qualification cohort exceeds %d nodes", maxQualificationCohort)
	}
	names := make([]string, 0, len(request.Nodes))
	seen := make(map[string]struct{}, len(request.Nodes))
	for _, raw := range request.Nodes {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("qualification node names must not be blank")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("qualification node %q is listed more than once", name)
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	requirements := request.Requirements
	if requirements.MinSamples <= 0 || requirements.MinSamples > maxQualificationSamples {
		return nil, fmt.Errorf("qualification min_samples must be between 1 and %d", maxQualificationSamples)
	}
	if requirements.MinDuration <= 0 || requirements.MinDuration > maxQualificationDuration {
		return nil, fmt.Errorf("qualification min_duration must be between 1ns and %s", maxQualificationDuration)
	}
	if request.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("qualification expiry is required")
	}
	request.ExpiresAt = request.ExpiresAt.UTC()
	earliestReady := now.Add(requirements.MinDuration)
	if request.ExpiresAt.Before(earliestReady.Add(qualificationExpiryMargin)) {
		return nil, fmt.Errorf("qualification expiry must be at least %s after the earliest possible ready time %s", qualificationExpiryMargin, earliestReady.Format(time.RFC3339))
	}
	if request.ExpiresAt.After(now.Add(maxQualificationLifetime)) {
		return nil, fmt.Errorf("qualification expiry exceeds %s from now", maxQualificationLifetime)
	}
	return names, nil
}

// qualificationInventory reads the managed fleet once and indexes it by name.
// A nil, nameless, or duplicate entry is an incomplete inventory: freezing a
// cohort against it could bind the wrong UID. The lister's slice is only read.
func (m *Manager) qualificationInventory(ctx context.Context) (map[string]*types.Node, error) {
	nodes, err := m.listNodes(ctx)
	if err != nil {
		return nil, err
	}
	inventory := make(map[string]*types.Node, len(nodes))
	for _, node := range nodes {
		if node == nil || strings.TrimSpace(node.Name) == "" {
			return nil, fmt.Errorf("%w: inventory contains a node without a name", ErrIncompleteInventory)
		}
		if _, duplicate := inventory[node.Name]; duplicate {
			return nil, fmt.Errorf("%w: inventory lists node %q more than once", ErrIncompleteInventory, node.Name)
		}
		inventory[node.Name] = node
	}
	return inventory, nil
}

// captureQualificationCoverage builds fresh coverage for every cohort member
// in frozen order. A builder error, or a builder answering for a different
// node, vendor, or node UID, fails closed: it can never be recorded as a
// sample. The UID check is an exact identity check, not drift: the caller has
// already established that the inventory still carries member.UID, so a
// builder answering with any other raw UID (padded, blank, or different) has
// read an inconsistent inventory and its coverage cannot be attributed to the
// frozen node.
func (m *Manager) captureQualificationCoverage(ctx context.Context, vendor types.AcceleratorVendor, cohort []RuntimeContractQualificationNode) ([]RuntimeContractQualificationNodeCoverage, error) {
	out := make([]RuntimeContractQualificationNodeCoverage, 0, len(cohort))
	for _, member := range cohort {
		coverage, err := m.buildRuntimeContractCoverage(ctx, member.Name, vendor)
		if err != nil {
			return nil, fmt.Errorf("runtime contract coverage for node %q: %w", member.Name, err)
		}
		if coverage.NodeName != member.Name || coverage.Vendor != vendor {
			return nil, fmt.Errorf("%w: coverage builder answered for node %q vendor %q instead of %q %q", ErrIncompleteInventory, coverage.NodeName, coverage.Vendor, member.Name, vendor)
		}
		if coverage.NodeUID != member.UID {
			return nil, fmt.Errorf("%w: coverage builder answered for node %q uid %q, inventory froze uid %q", ErrIncompleteInventory, member.Name, coverage.NodeUID, member.UID)
		}
		out = append(out, RuntimeContractQualificationNodeCoverage{
			Node: member.Name, NodeUID: member.UID, ConfigDigest: coverage.ConfigDigest,
			Profile: RuntimeContractQualificationProfile{
				Name: coverage.ProfileName, UID: coverage.ProfileUID, Generation: coverage.ProfileGeneration, Digest: coverage.ProfileDigest,
			},
			Selection: coverage.Selection, Attestation: coverage.Attestation, VerificationDepth: coverage.VerificationDepth,
			Reasons: append([]config.RuntimeContractReasonCode(nil), coverage.Reasons...), ReportObservedAt: coverage.ReportObservedAt,
		})
	}
	return out, nil
}

func qualificationCoverageFull(coverage []RuntimeContractQualificationNodeCoverage) bool {
	for _, item := range coverage {
		if item.Selection != config.RuntimeContractSelectionExact || item.Attestation != config.RuntimeContractAttestationFreshCompatible || item.VerificationDepth != config.RuntimeContractVerificationFull {
			return false
		}
	}
	return len(coverage) > 0
}

// ObserveRuntimeContractQualification re-reads the frozen cohort and fresh
// coverage, records one observation, and applies the lifecycle rules:
//
//   - past expiry the qualification becomes Expired without sampling;
//   - any binding drift (node UID, scope, config digest, profile identity or
//     selection) invalidates permanently;
//   - a sample counts only when every node is Exact, FreshCompatible and Full;
//   - ReadyForApproval requires both the minimum successful sample count and
//     the minimum duration since the first successful sample;
//   - once ReadyForApproval, any non-Full observation (missing, stale, or
//     mismatched attestation, or a bad heartbeat) invalidates permanently:
//     readiness never outlives its evidence. Before readiness the same
//     observation is recorded, does not count, and leaves the state Observing.
//
// Inventory, store, or builder failures return an error before anything is
// written or any idempotency key is reserved. The write itself is one
// transaction: the key reservation, the version-guarded resource update, and
// the observe and transition audit events commit together or not at all. An
// optimistic conflict, an audit failure, or an update failure therefore leaves
// no key, no observation, no version bump, and no partial audit behind, and
// the same key may be retried. Because a committed key always has its
// committed observation, a replay of that key returns the recorded state and
// never claims an observation that was not written.
func (m *Manager) ObserveRuntimeContractQualification(ctx context.Context, actor, id string, mutation RuntimeContractQualificationMutation) (*RuntimeContractQualification, bool, error) {
	resources, err := m.requireQualificationDependencies()
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(id) == "" || strings.TrimSpace(mutation.IdempotencyKey) == "" {
		return nil, false, fmt.Errorf("actor, qualification ID, and idempotency key are required")
	}
	requestDigest := digestJSON(struct {
		ID              string `json:"id"`
		Action          string `json:"action"`
		ExpectedVersion int    `json:"expected_version"`
	}{ID: id, Action: "observe", ExpectedVersion: mutation.ExpectedVersion})
	storageKey := scopedIdempotencyKey("observe", mutation.IdempotencyKey)
	// A prior successful retry is answered before the lifecycle is inspected:
	// a replay naturally finds the qualification already advanced. This read
	// is sound only because a key is never committed without its observation.
	if existing, lookupErr := resources.GetOperationalIdempotency(ctx, types.ResourceRuntimeContractQualification, actor, storageKey); lookupErr == nil {
		if existing.RequestDigest != requestDigest || existing.ResourceID != id {
			return nil, false, ErrIdempotencyConflict
		}
		qualification, getErr := getRuntimeContractQualification(ctx, resources, id)
		return qualification, true, getErr
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return nil, false, lookupErr
	}
	resource, qualification, err := loadRuntimeContractQualification(ctx, resources, id)
	if err != nil {
		return nil, false, err
	}
	if mutation.ExpectedVersion > 0 && mutation.ExpectedVersion != qualification.ResourceVersion {
		return nil, false, store.ErrOperationalConflict
	}
	if qualification.State.terminal() {
		return nil, false, fmt.Errorf("%w: qualification %q is %s", ErrInvalidState, id, qualification.State)
	}
	now := m.now().UTC()

	// Capture everything before reserving the retry key. Only expiry needs no
	// capture at all: it is decided by the clock, never by fresh evidence.
	var observation *RuntimeContractQualificationObservation
	var transition string
	if !now.Before(qualification.ExpiresAt) {
		transition = "expire"
	} else {
		observed, err := m.observeQualificationCohort(ctx, actor, now, qualification)
		if err != nil {
			return nil, false, err
		}
		observation = &observed
	}

	// The lifecycle step is computed in memory from the snapshot read above and
	// applied below under the snapshot's version: if another writer advanced
	// the row in between, the guarded update fails and the whole transaction,
	// key included, is discarded.
	qualification.UpdatedAt = now
	var params map[string]string
	result := ""
	switch {
	case transition == "expire":
		qualification.State, qualification.ExpiredAt = QualificationExpired, &now
	case len(observation.Drift) > 0:
		transition = "invalidate"
		qualification.InvalidationReason = strings.Join(observation.Drift, "; ")
		qualification.State, qualification.InvalidatedAt = QualificationInvalidated, &now
		params = map[string]string{"reason": qualification.InvalidationReason}
		result = "drift"
	case !observation.Successful && qualification.State == QualificationReadyForApproval:
		transition = "invalidate"
		qualification.InvalidationReason = "ready qualification regressed to non-Full coverage: " + strings.Join(observation.Degraded, "; ")
		qualification.State, qualification.InvalidatedAt = QualificationInvalidated, &now
		params = map[string]string{"reason": qualification.InvalidationReason}
		result = "not-full"
	default:
		result = "not-full"
		if observation.Successful {
			result = "full"
			qualification.SuccessfulSamples++
			if qualification.FirstSuccessfulSampleAt == nil {
				first := now
				qualification.FirstSuccessfulSampleAt = &first
			}
			if qualification.State == QualificationObserving && qualification.qualified(now) {
				transition = "ready"
				qualification.State, qualification.ReadyAt = QualificationReadyForApproval, &now
			}
		}
	}
	if observation != nil {
		qualification.recordObservation(*observation)
		params = mergeParams(params, map[string]string{
			"successful_samples": fmt.Sprint(qualification.SuccessfulSamples),
			"total_observations": fmt.Sprint(qualification.TotalObservations),
		})
	}
	var replayed *RuntimeContractQualification
	err = resources.WithOperationalTx(ctx, func(tx store.OperationalTx) error {
		record, created, err := tx.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{
			Kind: types.ResourceRuntimeContractQualification, Actor: actor, Key: storageKey,
			RequestDigest: requestDigest, ResourceID: id,
		})
		if err != nil {
			return err
		}
		if !created {
			// Another retry of this key committed between the pre-check and
			// this claim; its observation is committed with it.
			if record.RequestDigest != requestDigest || record.ResourceID != id {
				return ErrIdempotencyConflict
			}
			replayed, err = getRuntimeContractQualification(ctx, tx, id)
			return err
		}
		if err := saveRuntimeContractQualification(ctx, tx, resource, qualification, actor); err != nil {
			return err
		}
		if observation != nil {
			if err := m.auditThrough(ctx, tx, types.ResourceRuntimeContractQualification, id, actor, "observe", mutation.IdempotencyKey, "", params, result); err != nil {
				return err
			}
		}
		if transition != "" {
			if err := m.auditThrough(ctx, tx, types.ResourceRuntimeContractQualification, id, actor, transition, mutation.IdempotencyKey, "", params, string(qualification.State)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if replayed != nil {
		return replayed, true, nil
	}
	return qualification, false, nil
}

// auditThrough appends one qualification audit event through the transaction
// that carries the resource write it describes, so the event and the state it
// records commit together.
func (m *Manager) auditThrough(ctx context.Context, tx store.OperationalTx, kind types.OperationalResourceKind, resourceID, actor, action, requestID, decisionID string, params map[string]string, result string) error {
	return tx.AppendOperationalAudit(ctx, &types.OperationalAuditEvent{
		Kind: kind, ResourceID: resourceID, Time: m.now().UTC(), Actor: actor, Action: action,
		RequestID: requestID, DecisionID: decisionID, Params: params, Result: result,
	})
}

// observeQualificationCohort re-reads inventory and coverage for the frozen
// cohort and classifies the result. Inventory drift (missing, recreated, or
// out-of-scope nodes) is decided before any coverage is built, so a node that
// no longer exists cannot be assessed by name and mistaken for its successor.
// A cohort node whose inventory UID is blank or padded is not drift but an
// inconsistent read: it returns an error before anything is built or written.
func (m *Manager) observeQualificationCohort(ctx context.Context, actor string, now time.Time, qualification *RuntimeContractQualification) (RuntimeContractQualificationObservation, error) {
	observation := RuntimeContractQualificationObservation{At: now, Actor: actor}
	inventory, err := m.qualificationInventory(ctx)
	if err != nil {
		return observation, err
	}
	var drift []string
	for _, member := range qualification.Cohort {
		node, ok := inventory[member.Name]
		if !ok {
			drift = append(drift, fmt.Sprintf("node %q is no longer in the managed inventory", member.Name))
			continue
		}
		current, err := qualificationNodeUID(node)
		if err != nil {
			return observation, err
		}
		if current != member.UID {
			drift = append(drift, fmt.Sprintf("node %q uid changed from %q to %q", member.Name, member.UID, current))
			continue
		}
		// Scope labels are compared exactly to the frozen values, so adding,
		// removing, or changing a label is drift even when the frozen value is
		// empty. A subset match (as operationalNodeScopeMatches offers for
		// filters) would let an unscoped cohort silently acquire a tenant.
		if tenant, cluster := qualificationNodeScope(node); tenant != qualification.Tenant || cluster != qualification.Cluster {
			drift = append(drift, fmt.Sprintf("node %q tenant/cluster labels changed from %q/%q to %q/%q", member.Name, qualification.Tenant, qualification.Cluster, tenant, cluster))
		}
	}
	if len(drift) > 0 {
		observation.Drift = boundQualificationDrift(drift)
		return observation, nil
	}
	// captureQualificationCoverage has already failed closed if any builder
	// answered with a UID other than the frozen one, so every item below is
	// attributed to its frozen node.
	coverage, err := m.captureQualificationCoverage(ctx, qualification.Vendor, qualification.Cohort)
	if err != nil {
		return observation, err
	}
	var degraded []string
	for index, item := range coverage {
		member := qualification.Cohort[index]
		switch {
		case item.ConfigDigest != qualification.ConfigDigest:
			drift = append(drift, fmt.Sprintf("node %q config digest changed from %q to %q", member.Name, qualification.ConfigDigest, item.ConfigDigest))
		case item.Selection != config.RuntimeContractSelectionExact:
			drift = append(drift, fmt.Sprintf("node %q profile selection is %s, no longer Exact", member.Name, item.Selection))
		case item.Profile != qualification.Profile:
			drift = append(drift, fmt.Sprintf("node %q now binds profile %q uid %q generation %d digest %q", member.Name, item.Profile.Name, item.Profile.UID, item.Profile.Generation, item.Profile.Digest))
		case item.Attestation != config.RuntimeContractAttestationFreshCompatible || item.VerificationDepth != config.RuntimeContractVerificationFull:
			degraded = append(degraded, fmt.Sprintf("node %q attestation %s depth %s %v", member.Name, item.Attestation, item.VerificationDepth, item.Reasons))
		}
	}
	observation.Coverage = coverage
	if len(drift) > 0 {
		observation.Drift = boundQualificationDrift(drift)
		return observation, nil
	}
	observation.Degraded = boundQualificationDrift(degraded)
	observation.Successful = len(degraded) == 0
	return observation, nil
}

func boundQualificationDrift(drift []string) []string {
	if len(drift) <= maxQualificationDriftReasons {
		return drift
	}
	bounded := append([]string(nil), drift[:maxQualificationDriftReasons]...)
	return append(bounded, fmt.Sprintf("and %d more", len(drift)-maxQualificationDriftReasons))
}

// qualified answers whether both evidence bars are met at now. It never
// consults the retained observation window, so pruning cannot change it.
func (q *RuntimeContractQualification) qualified(now time.Time) bool {
	if q.SuccessfulSamples < q.Requirements.MinSamples || q.FirstSuccessfulSampleAt == nil {
		return false
	}
	return now.Sub(*q.FirstSuccessfulSampleAt) >= q.Requirements.MinDuration
}

func (q *RuntimeContractQualification) recordObservation(observation RuntimeContractQualificationObservation) {
	q.TotalObservations++
	at := observation.At
	q.LastObservedAt = &at
	q.Observations = append(q.Observations, observation)
	if excess := len(q.Observations) - maxQualificationObservations; excess > 0 {
		q.Observations = append([]RuntimeContractQualificationObservation(nil), q.Observations[excess:]...)
	}
}

func mergeParams(base, extra map[string]string) map[string]string {
	if base == nil {
		base = make(map[string]string, len(extra))
	}
	for key, value := range extra {
		base[key] = value
	}
	return base
}

// qualificationReader is the read every qualification load needs. Both the
// operational store and a transaction-scoped OperationalTx satisfy it, so the
// same decode path serves a plain read and an in-transaction replay.
type qualificationReader interface {
	GetOperationalResource(ctx context.Context, kind types.OperationalResourceKind, id string) (*types.OperationalResource, error)
}

func loadRuntimeContractQualification(ctx context.Context, reader qualificationReader, id string) (*types.OperationalResource, *RuntimeContractQualification, error) {
	resource, err := reader.GetOperationalResource(ctx, types.ResourceRuntimeContractQualification, id)
	if err != nil {
		return nil, nil, err
	}
	qualification, err := decodeRuntimeContractQualification(resource)
	if err != nil {
		return nil, nil, err
	}
	return resource, qualification, nil
}

func decodeRuntimeContractQualification(resource *types.OperationalResource) (*RuntimeContractQualification, error) {
	var qualification RuntimeContractQualification
	if err := json.Unmarshal(resource.Payload, &qualification); err != nil {
		return nil, fmt.Errorf("runtime contract qualification %q is corrupt: %w", resource.ID, err)
	}
	if qualification.Version != RuntimeContractQualificationVersion {
		return nil, fmt.Errorf("runtime contract qualification %q has unsupported version %q", resource.ID, qualification.Version)
	}
	qualification.ResourceVersion = resource.Version
	qualification.State = RuntimeContractQualificationState(resource.State)
	return &qualification, nil
}

func getRuntimeContractQualification(ctx context.Context, reader qualificationReader, id string) (*RuntimeContractQualification, error) {
	_, qualification, err := loadRuntimeContractQualification(ctx, reader, id)
	return qualification, err
}

// saveRuntimeContractQualification writes the lifecycle step through tx,
// guarded by the version the snapshot was read at. ErrOperationalConflict
// means another writer advanced the row since; the caller's transaction then
// rolls back and nothing of this attempt persists.
func saveRuntimeContractQualification(ctx context.Context, tx store.OperationalTx, resource *types.OperationalResource, qualification *RuntimeContractQualification, actor string) error {
	qualification.ResourceVersion = resource.Version + 1
	payload, err := marshalPayload(qualification)
	if err != nil {
		return err
	}
	resource.State, resource.Actor, resource.Payload = string(qualification.State), actor, payload
	if err := tx.UpdateOperationalResource(ctx, resource, resource.Version); err != nil {
		return err
	}
	qualification.ResourceVersion = resource.Version
	return nil
}

// GetRuntimeContractQualification returns one qualification. Terminal and
// expired qualifications remain readable for audit and review.
func (m *Manager) GetRuntimeContractQualification(ctx context.Context, id string) (*RuntimeContractQualification, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("qualification ID is required")
	}
	return getRuntimeContractQualification(ctx, m.resources, id)
}

// ListRuntimeContractQualifications returns a stable, scope-filtered page.
func (m *Manager) ListRuntimeContractQualifications(ctx context.Context, options OperationalListOptions) ([]*RuntimeContractQualification, error) {
	resources, err := m.listOperationalResources(ctx, types.ResourceRuntimeContractQualification, options)
	if err != nil {
		return nil, err
	}
	out := make([]*RuntimeContractQualification, 0, len(resources))
	for _, resource := range resources {
		qualification, err := decodeRuntimeContractQualification(resource)
		if err != nil {
			return nil, err
		}
		out = append(out, qualification)
	}
	return out, nil
}
