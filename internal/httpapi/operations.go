package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/internal/operations"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	maxOperationalRequestBytes = 1 << 20
	maxIdempotencyKeyBytes     = 256
)

func (s *Server) operationsManager(w http.ResponseWriter) *operations.Manager {
	provider, ok := s.operator.(OperationsProvider)
	if !ok || provider.Operations() == nil {
		http.Error(w, "v0.4.0 operational workflows are unavailable", http.StatusServiceUnavailable)
		return nil
	}
	return provider.Operations()
}

func operationalIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		http.Error(w, "Idempotency-Key header is required", http.StatusBadRequest)
		return "", false
	}
	if len(key) > maxIdempotencyKeyBytes {
		http.Error(w, "Idempotency-Key is too long", http.StatusBadRequest)
		return "", false
	}
	return key, true
}

func operationsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "resource not found", http.StatusNotFound)
	case errors.Is(err, store.ErrOperationalConflict), errors.Is(err, operations.ErrIdempotencyConflict), errors.Is(err, operations.ErrInvalidState):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, operations.ErrIncompleteInventory):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, operations.ErrUnavailable):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func (s *Server) handleFleetReadiness(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	filter, err := readinessFilterFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	items, nextNode, err := manager.FleetReadinessPage(r.Context(), filter)
	if err != nil {
		operationsError(w, err)
		return
	}
	response := map[string]any{"items": items, "evaluator_version": decision.EvaluatorVersion}
	if nextNode != "" {
		response["next_cursor"] = encodeReadinessCursor(nextNode)
	}
	writeJSON(w, response)
}

func (s *Server) handleNodeReadiness(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	item, err := manager.NodeReadiness(r.Context(), r.PathValue("node"))
	if err != nil {
		operationsError(w, err)
		return
	}
	writeJSON(w, item)
}

func (s *Server) handleNodeEvidence(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	item, err := manager.NodeReadiness(r.Context(), r.PathValue("node"))
	if err != nil {
		operationsError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"node": item.Node, "evidence_refs": item.Decision.EvidenceRefs,
		"evidence_age": evidenceAge(item.Snapshot), "config_digest": item.Decision.ConfigDigest,
	})
}

func evidenceAge(snapshot decision.Snapshot) string {
	if snapshot.Report == nil || snapshot.Report.ObservedAt.IsZero() || snapshot.EvaluatedAt.IsZero() {
		return ""
	}
	if snapshot.Report.ObservedAt.After(snapshot.EvaluatedAt) {
		return "0s"
	}
	return snapshot.EvaluatedAt.Sub(snapshot.Report.ObservedAt).String()
}

func writeOperationalJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// Candidate bytes deliberately travel as the HTTP body rather than a JSON
// string.  This avoids a second escaping/parser layer and lets deployments
// apply content-type scanning controls before the isolated compiler boundary.
func (s *Server) handleCreateCandidate(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if contentType != "application/yaml" && contentType != "application/x-yaml" && contentType != "application/json" && contentType != "text/yaml" {
		http.Error(w, "candidate Content-Type must be application/yaml or application/json", http.StatusUnsupportedMediaType)
		return
	}
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return
	}
	actor, err := s.resolveActor(r, r.URL.Query().Get("actor"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxOperationalRequestBytes)
	content, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, "reading candidate failed", http.StatusBadRequest)
		return
	}
	var expiry time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("expires_at")); raw != "" {
		expiry, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			http.Error(w, "expires_at must be RFC3339", http.StatusBadRequest)
			return
		}
	}
	candidate, replayed, err := manager.CreateCandidate(r.Context(), actor, operations.CandidateUpload{
		Content: content, Tenant: strings.TrimSpace(r.URL.Query().Get("tenant")), Cluster: strings.TrimSpace(r.URL.Query().Get("cluster")),
		IdempotencyKey: key, ExpiresAt: expiry,
	})
	if err != nil {
		operationsError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/api/v1/candidates/"+candidate.ID)
	writeOperationalJSON(w, http.StatusCreated, candidate)
}

func (s *Server) handleGetCandidate(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	candidate, err := manager.GetCandidate(r.Context(), r.PathValue("id"))
	if err != nil {
		operationsError(w, err)
		return
	}
	writeJSON(w, candidate)
}

func (s *Server) handleListCandidates(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	options, err := operationalListOptions(r, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	items, err := manager.ListCandidates(r.Context(), options)
	if err != nil {
		operationsError(w, err)
		return
	}
	writeOperationalPage(w, items, options.Limit, func(item *operations.CandidateConfiguration) (time.Time, string) { return item.CreatedAt, item.ID })
}

type revokeCandidateRequest struct {
	Actor           string `json:"actor"`
	Reason          string `json:"reason"`
	ResourceVersion int    `json:"resource_version,omitempty"`
}

func (s *Server) handleRevokeCandidate(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request revokeCandidateRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	actor, err := s.resolveActor(r, request.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	candidate, replayed, err := manager.RevokeCandidate(r.Context(), actor, r.PathValue("id"), request.Reason, key, request.ResourceVersion)
	if err != nil {
		operationsError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeOperationalJSON(w, http.StatusOK, candidate)
}

type actorOnlyRequest struct {
	Actor string `json:"actor"`
}

func (s *Server) handleCreatePreview(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request actorOnlyRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	actor, err := s.resolveActor(r, request.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	preview, replayed, err := manager.CreatePreview(r.Context(), actor, r.PathValue("id"), key)
	if err != nil {
		operationsError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeOperationalJSON(w, http.StatusCreated, preview)
}

func (s *Server) handleGetCandidatePreview(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	preview, err := manager.LatestPreviewForCandidate(r.Context(), r.PathValue("id"))
	if err != nil {
		operationsError(w, err)
		return
	}
	writeJSON(w, preview)
}

func (s *Server) handleGetPreview(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	preview, err := manager.GetPreview(r.Context(), r.PathValue("id"))
	if err != nil {
		operationsError(w, err)
		return
	}
	writeJSON(w, preview)
}

func (s *Server) handleListPreviews(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	options, err := operationalListOptions(r, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	items, err := manager.ListPreviews(r.Context(), options)
	if err != nil {
		operationsError(w, err)
		return
	}
	writeOperationalPage(w, items, options.Limit, func(item *operations.PolicyImpactPreview) (time.Time, string) { return item.CreatedAt, item.ID })
}

type healthCheckAPIRequest struct {
	Actor    string                        `json:"actor"`
	Node     string                        `json:"node"`
	DeviceID string                        `json:"device_id,omitempty"`
	Profile  operations.HealthCheckProfile `json:"profile"`
	Reason   string                        `json:"reason"`
	Deadline time.Time                     `json:"deadline,omitempty"`
	Tenant   string                        `json:"tenant,omitempty"`
	Cluster  string                        `json:"cluster,omitempty"`
}

func (s *Server) handleCreateHealthCheck(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request healthCheckAPIRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	actor, err := s.resolveActor(r, request.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	elevatedAuthorizationGranted := false
	disruptionBudgetApproved := false
	if request.Profile == operations.HealthCheckExtended {
		if !s.requireVerifiedRole(w, r, roleExtendedDiagnostics) || !s.requireVerifiedRole(w, r, roleDisruptionBudgetApprover) {
			return
		}
		elevatedAuthorizationGranted = true
		disruptionBudgetApproved = true
	}
	run, replayed, err := manager.CreateHealthCheck(r.Context(), actor, operations.HealthCheckRequest{
		Node: request.Node, DeviceID: request.DeviceID, Profile: request.Profile, Reason: request.Reason,
		Deadline: request.Deadline, Tenant: request.Tenant, Cluster: request.Cluster,
		ElevatedAuthorizationGranted: elevatedAuthorizationGranted, DisruptionBudgetApproved: disruptionBudgetApproved,
		IdempotencyKey: key,
	})
	if err != nil {
		operationsError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	if run.State == "queued" {
		writeOperationalJSON(w, http.StatusAccepted, run)
	} else {
		writeOperationalJSON(w, http.StatusCreated, run)
	}
}

func (s *Server) handleGetHealthCheck(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	run, err := manager.GetHealthCheck(r.Context(), r.PathValue("id"))
	if err != nil {
		operationsError(w, err)
		return
	}
	writeJSON(w, run)
}

func (s *Server) handleListHealthChecks(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	options, err := operationalListOptions(r, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	items, err := manager.ListHealthChecks(r.Context(), options)
	if err != nil {
		operationsError(w, err)
		return
	}
	writeOperationalPage(w, items, options.Limit, func(item *operations.HealthCheckRun) (time.Time, string) { return item.CreatedAt, item.ID })
}

func (s *Server) handleListNodeHealthChecks(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	options, err := operationalListOptions(r, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	runs, nextAt, nextID, err := manager.ListNodeHealthChecksPage(r.Context(), r.PathValue("node"), options)
	if err != nil {
		operationsError(w, err)
		return
	}
	response := map[string]any{"items": runs}
	if !nextAt.IsZero() && nextID != "" {
		response["next_cursor"] = encodeOperationalCursor(nextAt, nextID)
	}
	writeJSON(w, response)
}

type cancelHealthCheckRequest struct {
	Actor           string `json:"actor"`
	ResourceVersion int    `json:"resource_version,omitempty"`
}

func (s *Server) handleCancelHealthCheck(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request cancelHealthCheckRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	actor, err := s.resolveActor(r, request.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	run, replayed, err := manager.CancelHealthCheckRequest(r.Context(), actor, r.PathValue("id"), key, request.ResourceVersion)
	if err != nil {
		operationsError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeOperationalJSON(w, http.StatusOK, run)
}

type simulationAPIRequest struct {
	Actor     string                       `json:"actor"`
	Node      string                       `json:"node"`
	DeviceID  string                       `json:"device_id,omitempty"`
	Action    types.AcceleratorAction      `json:"action,omitempty"`
	Scope     types.AcceleratorTargetScope `json:"scope,omitempty"`
	Class     types.ProblemClass           `json:"class"`
	Severity  types.Severity               `json:"severity,omitempty"`
	Rationale string                       `json:"rationale"`
	Tenant    string                       `json:"tenant,omitempty"`
	Cluster   string                       `json:"cluster,omitempty"`
}

func (s *Server) handleCreateSimulation(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request simulationAPIRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	actor, err := s.resolveActor(r, request.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	simulation, replayed, err := manager.CreateSimulation(r.Context(), actor, operations.SimulationRequest{
		Node: request.Node, DeviceID: request.DeviceID, Action: request.Action, Scope: request.Scope,
		Class: request.Class, Severity: request.Severity, Rationale: request.Rationale,
		Tenant: request.Tenant, Cluster: request.Cluster, IdempotencyKey: key,
	})
	if err != nil {
		operationsError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeOperationalJSON(w, http.StatusCreated, simulation)
}

func (s *Server) handleGetSimulation(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	simulation, err := manager.GetSimulation(r.Context(), r.PathValue("id"))
	if err != nil {
		operationsError(w, err)
		return
	}
	writeJSON(w, simulation)
}

func (s *Server) handleListSimulations(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	options, err := operationalListOptions(r, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	items, err := manager.ListSimulations(r.Context(), options)
	if err != nil {
		operationsError(w, err)
		return
	}
	writeOperationalPage(w, items, options.Limit, func(item *operations.RemediationSimulation) (time.Time, string) { return item.CreatedAt, item.ID })
}

type incidentFromSimulationRequest struct {
	Actor           string `json:"actor"`
	SimulationID    string `json:"simulation_id"`
	Rationale       string `json:"rationale"`
	ResourceVersion int    `json:"resource_version,omitempty"`
}

func (s *Server) handleCreateIncidentFromSimulation(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request incidentFromSimulationRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	actor, err := s.resolveActor(r, request.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	incident, replayed, err := manager.CreateIncidentFromSimulationRequest(r.Context(), actor, request.SimulationID, request.Rationale, key, request.ResourceVersion)
	if err != nil {
		operationsError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeOperationalJSON(w, http.StatusCreated, incident)
}

func positiveLimit(raw string) (int, error) {
	if raw == "" {
		return 100, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 || limit > 500 {
		return 0, errors.New("limit must be an integer between 1 and 500")
	}
	return limit, nil
}

type operationalCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

type readinessCursor struct {
	Node string `json:"node"`
}

type auditCursor struct {
	ID int64 `json:"id"`
}

func readinessFilterFromRequest(r *http.Request) (operations.ReadinessFilter, error) {
	limit, err := positiveLimit(r.URL.Query().Get("limit"))
	if err != nil {
		return operations.ReadinessFilter{}, err
	}
	filter := operations.ReadinessFilter{
		Vendor:  types.AcceleratorVendor(strings.TrimSpace(r.URL.Query().Get("vendor"))),
		Profile: strings.TrimSpace(r.URL.Query().Get("profile")),
		Tenant:  strings.TrimSpace(r.URL.Query().Get("tenant")),
		Cluster: strings.TrimSpace(r.URL.Query().Get("cluster")),
		Limit:   limit,
	}
	if state := strings.TrimSpace(r.URL.Query().Get("state")); state != "" {
		filter.State = decision.State(state)
		switch filter.State {
		case decision.StateEligible, decision.StateObservedOnly, decision.StateBlocked, decision.StateUnknown:
		default:
			return operations.ReadinessFilter{}, errors.New("state must be Eligible, ObservedOnly, Blocked, or Unknown")
		}
	}
	if filter.Vendor != "" {
		switch filter.Vendor {
		case types.AcceleratorVendorNVIDIA, types.AcceleratorVendorAMD, types.AcceleratorVendorIntel:
		default:
			return operations.ReadinessFilter{}, errors.New("vendor must be nvidia, amd, or intel")
		}
	}
	if raw := r.URL.Query().Get("stale"); raw != "" {
		value, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return operations.ReadinessFilter{}, errors.New("stale must be true or false")
		}
		filter.StaleOnly = value
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		blob, decodeErr := base64.RawURLEncoding.DecodeString(raw)
		if decodeErr != nil {
			return operations.ReadinessFilter{}, errors.New("cursor is invalid")
		}
		var cursor readinessCursor
		if decodeErr := json.Unmarshal(blob, &cursor); decodeErr != nil || strings.TrimSpace(cursor.Node) == "" {
			return operations.ReadinessFilter{}, errors.New("cursor is invalid")
		}
		filter.AfterNode = cursor.Node
	}
	return filter, nil
}

func encodeReadinessCursor(node string) string {
	blob, _ := json.Marshal(readinessCursor{Node: node})
	return base64.RawURLEncoding.EncodeToString(blob)
}

func operationalListOptions(r *http.Request, includeExpiredDefault bool) (operations.OperationalListOptions, error) {
	limit, err := positiveLimit(r.URL.Query().Get("limit"))
	if err != nil {
		return operations.OperationalListOptions{}, err
	}
	options := operations.OperationalListOptions{
		State: r.URL.Query().Get("state"), Tenant: r.URL.Query().Get("tenant"), Cluster: r.URL.Query().Get("cluster"),
		Limit: limit, IncludeExpired: includeExpiredDefault,
	}
	if raw := r.URL.Query().Get("include_expired"); raw != "" {
		value, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return operations.OperationalListOptions{}, errors.New("include_expired must be true or false")
		}
		options.IncludeExpired = value
	}
	for _, bound := range []struct {
		name   string
		target *time.Time
	}{{"since", &options.CreatedSince}, {"until", &options.CreatedUntil}} {
		raw := r.URL.Query().Get(bound.name)
		if raw == "" {
			continue
		}
		value, parseErr := time.Parse(time.RFC3339, raw)
		if parseErr != nil {
			return operations.OperationalListOptions{}, fmt.Errorf("%s must be RFC3339", bound.name)
		}
		*bound.target = value.UTC()
	}
	if !options.CreatedSince.IsZero() && !options.CreatedUntil.IsZero() && options.CreatedUntil.Before(options.CreatedSince) {
		return operations.OperationalListOptions{}, errors.New("until must not be before since")
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		blob, decodeErr := base64.RawURLEncoding.DecodeString(raw)
		if decodeErr != nil {
			return operations.OperationalListOptions{}, errors.New("cursor is invalid")
		}
		var cursor operationalCursor
		if decodeErr := json.Unmarshal(blob, &cursor); decodeErr != nil || cursor.CreatedAt.IsZero() || strings.TrimSpace(cursor.ID) == "" {
			return operations.OperationalListOptions{}, errors.New("cursor is invalid")
		}
		options.AfterCreatedAt, options.AfterID = cursor.CreatedAt.UTC(), cursor.ID
	}
	return options, nil
}

func encodeOperationalCursor(createdAt time.Time, id string) string {
	blob, _ := json.Marshal(operationalCursor{CreatedAt: createdAt.UTC(), ID: id})
	return base64.RawURLEncoding.EncodeToString(blob)
}

func writeOperationalPage[T any](w http.ResponseWriter, items []T, limit int, cursor func(T) (time.Time, string)) {
	response := map[string]any{"items": items}
	if len(items) == limit && len(items) > 0 {
		createdAt, id := cursor(items[len(items)-1])
		response["next_cursor"] = encodeOperationalCursor(createdAt, id)
	}
	writeJSON(w, response)
}

type autonomyPlanAPIRequest struct {
	Actor          string                                  `json:"actor"`
	Selector       map[string]string                       `json:"selector"`
	PolicyRef      string                                  `json:"policy_ref"`
	ProfileRef     string                                  `json:"profile_ref"`
	AllowedActions []types.AcceleratorAction               `json:"allowed_actions"`
	Evidence       autonomyEvidenceAPIRequest              `json:"evidence"`
	Guardrails     operations.AutonomyGuardrails           `json:"guardrails"`
	Rollout        autonomyRolloutAPIRequest               `json:"rollout"`
	Approvals      operations.AutonomyApprovalRequirements `json:"approvals"`
	SimulationID   string                                  `json:"simulation_id,omitempty"`
	ExpiresAt      time.Time                               `json:"expires_at"`
	Tenant         string                                  `json:"tenant,omitempty"`
	Cluster        string                                  `json:"cluster,omitempty"`
}

type autonomyEvidenceAPIRequest struct {
	MaxAge          string   `json:"max_age"`
	RequiredSources []string `json:"required_sources"`
}

type autonomyRolloutAPIRequest struct {
	CanaryNodes    int    `json:"canary_nodes"`
	BakeDuration   string `json:"bake_duration"`
	ExpansionSteps []int  `json:"expansion_steps,omitempty"`
}

func (r autonomyPlanAPIRequest) compile() (operations.AutonomyPlanRequest, error) {
	evidenceMaxAge, err := time.ParseDuration(r.Evidence.MaxAge)
	if err != nil || evidenceMaxAge <= 0 {
		return operations.AutonomyPlanRequest{}, errors.New("evidence.max_age must be a positive Go duration")
	}
	bakeDuration, err := time.ParseDuration(r.Rollout.BakeDuration)
	if err != nil || bakeDuration <= 0 {
		return operations.AutonomyPlanRequest{}, errors.New("rollout.bake_duration must be a positive Go duration")
	}
	return operations.AutonomyPlanRequest{
		Selector: r.Selector, PolicyRef: r.PolicyRef, ProfileRef: r.ProfileRef, AllowedActions: r.AllowedActions,
		Evidence:   operations.AutonomyEvidenceRequirements{MaxAge: evidenceMaxAge, RequiredSources: r.Evidence.RequiredSources},
		Guardrails: r.Guardrails,
		Rollout:    operations.AutonomyRolloutPolicy{CanaryNodes: r.Rollout.CanaryNodes, BakeDuration: bakeDuration, ExpansionSteps: r.Rollout.ExpansionSteps},
		Approvals:  r.Approvals, SimulationID: r.SimulationID, ExpiresAt: r.ExpiresAt, Tenant: r.Tenant, Cluster: r.Cluster,
	}, nil
}

func (s *Server) handleCreateAutonomyPlan(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request autonomyPlanAPIRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	actor, err := s.resolveActor(r, request.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	planRequest, err := request.compile()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	planRequest.IdempotencyKey = key
	plan, replayed, err := manager.CreateAutonomyPlan(r.Context(), actor, planRequest)
	if err != nil {
		operationsError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/api/v1/autonomy/plans/"+plan.ID)
	writeOperationalJSON(w, http.StatusCreated, plan)
}

func (s *Server) handleListAutonomyPlans(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	options, err := operationalListOptions(r, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	plans, err := manager.ListAutonomyPlansWithOptions(r.Context(), options)
	if err != nil {
		operationsError(w, err)
		return
	}
	writeOperationalPage(w, plans, options.Limit, func(plan *operations.GPUAutonomyPlan) (time.Time, string) { return plan.CreatedAt, plan.ID })
}

func (s *Server) handleGetAutonomyPlan(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	plan, err := manager.GetAutonomyPlan(r.Context(), r.PathValue("id"))
	if err != nil {
		operationsError(w, err)
		return
	}
	writeJSON(w, plan)
}

func (s *Server) handleGetAutonomyRollout(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	rollout, err := manager.GetAutonomyRollout(r.Context(), r.PathValue("id"))
	if err != nil {
		operationsError(w, err)
		return
	}
	writeJSON(w, rollout)
}

type autonomyMutationAPIRequest struct {
	Actor           string `json:"actor"`
	Role            string `json:"role,omitempty"`
	Reason          string `json:"reason,omitempty"`
	SimulationID    string `json:"simulation_id,omitempty"`
	ResourceVersion int    `json:"resource_version,omitempty"`
}

func (s *Server) decodeAutonomyMutation(w http.ResponseWriter, r *http.Request) (string, autonomyMutationAPIRequest, bool) {
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return "", autonomyMutationAPIRequest{}, false
	}
	var request autonomyMutationAPIRequest
	if !decodeStrict(w, r, &request) {
		return "", autonomyMutationAPIRequest{}, false
	}
	actor, err := s.resolveActor(r, request.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", autonomyMutationAPIRequest{}, false
	}
	return actor + "\x00" + key, request, true
}

func splitAutonomyActorKey(value string) (string, string) {
	actor, key, _ := strings.Cut(value, "\x00")
	return actor, key
}

func (s *Server) handleAttachAutonomySimulation(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	actorKey, request, ok := s.decodeAutonomyMutation(w, r)
	if !ok {
		return
	}
	actor, key := splitAutonomyActorKey(actorKey)
	plan, replayed, err := manager.AttachSimulationRequest(r.Context(), actor, r.PathValue("id"), request.SimulationID, operations.AutonomyMutation{ExpectedVersion: request.ResourceVersion, IdempotencyKey: key})
	s.writeAutonomyMutation(w, plan, replayed, err)
}

func (s *Server) handleApproveAutonomyPlan(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	actorKey, request, ok := s.decodeAutonomyMutation(w, r)
	if !ok {
		return
	}
	if !s.requireVerifiedRole(w, r, request.Role) {
		return
	}
	actor, key := splitAutonomyActorKey(actorKey)
	plan, replayed, err := manager.ApproveAutonomyPlanRequest(r.Context(), actor, r.PathValue("id"), request.Role, operations.AutonomyMutation{ExpectedVersion: request.ResourceVersion, IdempotencyKey: key})
	s.writeAutonomyMutation(w, plan, replayed, err)
}

func (s *Server) handlePauseAutonomyPlan(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	actorKey, request, ok := s.decodeAutonomyMutation(w, r)
	if !ok {
		return
	}
	actor, key := splitAutonomyActorKey(actorKey)
	plan, replayed, err := manager.PauseAutonomyPlanRequest(r.Context(), actor, r.PathValue("id"), request.Reason, operations.AutonomyMutation{ExpectedVersion: request.ResourceVersion, IdempotencyKey: key})
	s.writeAutonomyMutation(w, plan, replayed, err)
}

func (s *Server) handleResumeAutonomyPlan(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	actorKey, request, ok := s.decodeAutonomyMutation(w, r)
	if !ok {
		return
	}
	actor, key := splitAutonomyActorKey(actorKey)
	plan, replayed, err := manager.ResumeAutonomyPlanRequest(r.Context(), actor, r.PathValue("id"), request.Reason, operations.AutonomyMutation{ExpectedVersion: request.ResourceVersion, IdempotencyKey: key})
	s.writeAutonomyMutation(w, plan, replayed, err)
}

func (s *Server) handleRollbackAutonomyPlan(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	actorKey, request, ok := s.decodeAutonomyMutation(w, r)
	if !ok {
		return
	}
	actor, key := splitAutonomyActorKey(actorKey)
	plan, replayed, err := manager.RollbackAutonomyPlanRequest(r.Context(), actor, r.PathValue("id"), request.Reason, operations.AutonomyMutation{ExpectedVersion: request.ResourceVersion, IdempotencyKey: key})
	s.writeAutonomyMutation(w, plan, replayed, err)
}

func (s *Server) writeAutonomyMutation(w http.ResponseWriter, plan *operations.GPUAutonomyPlan, replayed bool, err error) {
	if err != nil {
		operationsError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeOperationalJSON(w, http.StatusOK, plan)
}

func (s *Server) handleListOperationalAuditEvents(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	limit, err := positiveLimit(r.URL.Query().Get("limit"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter := types.OperationalAuditFilter{
		Kind:       types.OperationalResourceKind(r.URL.Query().Get("kind")),
		ResourceID: r.URL.Query().Get("resource_id"), Tenant: strings.TrimSpace(r.URL.Query().Get("tenant")), Cluster: strings.TrimSpace(r.URL.Query().Get("cluster")), Actor: r.URL.Query().Get("actor"),
		RequestID: r.URL.Query().Get("request_id"), DecisionID: r.URL.Query().Get("decision_id"),
		Action: r.URL.Query().Get("action"), Limit: limit,
	}
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		blob, decodeErr := base64.RawURLEncoding.DecodeString(cursor)
		var decoded auditCursor
		if decodeErr != nil || json.Unmarshal(blob, &decoded) != nil || decoded.ID < 0 {
			http.Error(w, "cursor is invalid", http.StatusBadRequest)
			return
		}
		filter.AfterID = decoded.ID
	}
	for _, field := range []struct {
		raw    string
		target *time.Time
	}{{r.URL.Query().Get("since"), &filter.Since}, {r.URL.Query().Get("until"), &filter.Until}} {
		if field.raw == "" {
			continue
		}
		parsed, parseErr := time.Parse(time.RFC3339, field.raw)
		if parseErr != nil {
			http.Error(w, "since and until must be RFC3339 timestamps", http.StatusBadRequest)
			return
		}
		*field.target = parsed
	}
	events, err := manager.ListAuditEvents(r.Context(), filter)
	if err != nil {
		operationsError(w, err)
		return
	}
	response := map[string]any{"items": events}
	if len(events) == limit {
		response["next_cursor"] = encodeAuditCursor(events[len(events)-1].ID)
	}
	writeJSON(w, response)
}

func encodeAuditCursor(id int64) string {
	blob, _ := json.Marshal(auditCursor{ID: id})
	return base64.RawURLEncoding.EncodeToString(blob)
}
