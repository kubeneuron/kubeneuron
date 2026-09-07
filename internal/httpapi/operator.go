package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/operations"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// OperatorBackend is the controller surface behind the operator/UI API.
type OperatorBackend interface {
	ListIncidents(ctx context.Context, states []string, node string, limit int) ([]*types.Incident, error)
	IncidentDetail(ctx context.Context, id string) (*types.Incident, []*types.AuditEntry, error)
	DecideApproval(ctx context.Context, id, actor, channel string, decision types.ApprovalDecision, expectedEpoch int, reason string) error
	ResolveIncident(ctx context.Context, id, actor, reason string) error
	Nodes(ctx context.Context) ([]*types.Node, error)
	Node(ctx context.Context, name string) (*types.Node, error)
	SetPaused(ctx context.Context, paused bool, actor string) error
	Paused() bool
}

// IncidentAcknowledgementBackend is an optional v0.4.0 extension. Keeping it
// separate preserves fail-closed compatibility for an older controller: the
// new endpoint returns 503 instead of accepting an acknowledgement it cannot
// persist with idempotency and optimistic concurrency.
type IncidentAcknowledgementBackend interface {
	AcknowledgeIncident(ctx context.Context, id, actor, reason string, expectedVersion int, idempotencyKey string) (*types.Incident, bool, error)
}

// IncidentResolutionBackend is the idempotent v0.4.0 resolution extension.
// The legacy OperatorBackend method remains for compatibility with an older
// out-of-tree controller; current controllers expose this form and the public
// route then requires an idempotency key and displayed resource version.
type IncidentResolutionBackend interface {
	ResolveIncidentRequest(ctx context.Context, id, actor, reason string, expectedVersion int, idempotencyKey string) (*types.Incident, bool, error)
}

// AcceleratorReportOperatorBackend is an optional read-only extension for
// the versioned accelerator observation store. Keeping it separate prevents
// an older controller from returning an empty success response when it cannot
// actually retain or expose the preflight evidence.
type AcceleratorReportOperatorBackend interface {
	AcceleratorReports(ctx context.Context, node string) ([]*types.AgentAcceleratorReport, error)
}

// RecoveryReportBackend is an optional read-only extension that aggregates
// the incident store into the capacity-owner view of a window. It is separate
// from OperatorBackend for the same reason the accelerator reports are: a
// controller that cannot compute the report must say so with a 503 rather
// than return a plausible-looking empty one — a zero recovery report reads as
// "nothing broke", which is the opposite of "I could not tell you".
type RecoveryReportBackend interface {
	RecoveryReport(ctx context.Context, window time.Duration) (*types.RecoveryReport, error)
}

// OperationsProvider exposes the v0.4.0 durable product service.  It remains
// optional so an older/out-of-tree controller fails closed with 503 instead of
// presenting an in-memory-looking candidate, diagnostic, or simulation API.
type OperationsProvider interface {
	Operations() *operations.Manager
}

// OperatorIdentity is the authenticated principal behind an operator API
// request. Actor is empty for the shared static token: the token proves
// possession, not identity, so the caller's self-asserted name is recorded
// with a "token:" prefix instead of being presented as verified.
type OperatorIdentity struct {
	// Actor is the authenticated principal name (e.g.
	// "system:serviceaccount:ops:sre-bot" or an OIDC username). Empty when
	// only the shared static token authenticated the request.
	Actor string
	// Method names the authentication path, e.g. "kubernetes".
	Method string
	// Roles are authorization attributes supplied by the authenticated identity
	// provider. They are deliberately never read from an API request body.
	// Kubernetes authentication maps these to verified group membership.
	Roles []string
}

// OperatorAuthenticator authenticates one operator API request with a
// per-caller credential and authorizes it for the given verb ("get" for
// reads, "update" for mutations). Implementations return HTTPStatusError to
// select the response status.
type OperatorAuthenticator interface {
	AuthenticateOperator(r *http.Request, verb string) (OperatorIdentity, error)
}

type operatorIdentityContextKey struct{}

// OperatorIdentityFromContext returns the identity installed by requireOperator.
func OperatorIdentityFromContext(ctx context.Context) (OperatorIdentity, bool) {
	identity, ok := ctx.Value(operatorIdentityContextKey{}).(OperatorIdentity)
	return identity, ok
}

const (
	roleExtendedDiagnostics      = "diagnostics-extended"
	roleDisruptionBudgetApprover = "disruption-budget-approver"
)

// requireVerifiedRole fences operations whose authorization cannot safely be
// delegated to a shared token or a caller-supplied JSON field. A static token
// intentionally carries no principal or roles, while a Kubernetes identity
// carries only groups verified by TokenReview.
func (s *Server) requireVerifiedRole(w http.ResponseWriter, r *http.Request, role string) bool {
	role = strings.TrimSpace(role)
	identity, ok := OperatorIdentityFromContext(r.Context())
	if !ok || identity.Actor == "" || role == "" {
		http.Error(w, "a verified identity with the required role is required", http.StatusForbidden)
		return false
	}
	for _, granted := range identity.Roles {
		if granted == role {
			return true
		}
	}
	http.Error(w, fmt.Sprintf("authenticated role %q is required", role), http.StatusForbidden)
	return false
}

// EnableOperatorAPI attaches the operator routes behind a required bearer
// token. An empty token keeps the API disabled (fail closed).
func (s *Server) EnableOperatorAPI(backend OperatorBackend, token string) {
	s.operator = backend
	s.operatorToken = token
}

// SetOperatorAuthenticator additionally accepts per-caller credentials (e.g.
// Kubernetes TokenReview + SubjectAccessReview). The static token from
// EnableOperatorAPI keeps working as a break-glass path; audit rows then
// record the caller's claim as "token:<name>" rather than a verified actor.
func (s *Server) SetOperatorAuthenticator(authenticator OperatorAuthenticator) {
	s.operatorAuth = authenticator
}

// SetWebhookToken requires the given bearer token on the Alertmanager
// webhook. Empty keeps the webhook open (development only).
func (s *Server) SetWebhookToken(token string) { s.webhookToken = token }

// SetOperatorTokenProvider resolves the operator token per request so a
// rotated Secret takes effect without a controller restart. The provider's
// value wins whenever it is non-empty.
func (s *Server) SetOperatorTokenProvider(fn func() string) { s.operatorTokenFn = fn }

// SetWebhookTokenProvider is SetOperatorTokenProvider for the webhook token.
func (s *Server) SetWebhookTokenProvider(fn func() string) { s.webhookTokenFn = fn }

func (s *Server) currentOperatorToken() string {
	if s.operatorTokenFn != nil {
		if token := s.operatorTokenFn(); token != "" {
			return token
		}
	}
	return s.operatorToken
}

func (s *Server) currentWebhookToken() string {
	if s.webhookTokenFn != nil {
		if token := s.webhookTokenFn(); token != "" {
			return token
		}
	}
	return s.webhookToken
}

func (s *Server) requireOperator(next http.HandlerFunc) http.HandlerFunc {
	return s.requireOperatorVerb("get", next)
}

func (s *Server) requireOperatorMutation(next http.HandlerFunc) http.HandlerFunc {
	return s.requireOperatorVerb("update", func(w http.ResponseWriter, r *http.Request) {
		if !s.isLeader() {
			http.Error(w, "standby: not the elected leader", http.StatusServiceUnavailable)
			return
		}
		next(w, r)
	})
}

// requireOperationalMutation adds a bounded per-source quota after ordinary
// operator authentication and leader fencing.  Candidate compilation,
// fleet-wide preview, diagnostics, and simulation are intentionally more
// expensive than a normal incident decision; direct Pod access must not turn
// one valid credential into an unbounded parser or inventory-evaluation loop.
func (s *Server) requireOperationalMutation(operation string, limit int, next http.HandlerFunc) http.HandlerFunc {
	return s.requireOperatorMutation(func(w http.ResponseWriter, r *http.Request) {
		if s.operationLimiter != nil {
			allowed, retryAfter := s.operationLimiter.allow(remoteSource(r)+"\x00"+operation, limit)
			if !allowed {
				seconds := int(retryAfter.Seconds())
				if retryAfter%time.Second != 0 {
					seconds++
				}
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				http.Error(w, "operational request quota exceeded; retry later", http.StatusTooManyRequests)
				return
			}
		}
		next(w, r)
	})
}

func (s *Server) requireOperatorVerb(verb string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.operator == nil || s.operatorToken == "" {
			http.Error(w, "operator API disabled: no API token configured", http.StatusServiceUnavailable)
			return
		}
		source := remoteSource(r)
		var identity OperatorIdentity
		if fromSession, ok := s.sessionIdentity(r); ok {
			next(w, r.WithContext(context.WithValue(r.Context(), operatorIdentityContextKey{}, fromSession)))
			return
		}
		switch {
		case bearerMatches(r, s.currentOperatorToken()):
			identity = OperatorIdentity{Method: "static-token"}
		case s.operatorAuth != nil:
			// Authenticate before consulting the per-source-IP throttle: a valid
			// SAR-token operator must never be locked out because other clients
			// behind the same shared egress IP (NAT / load balancer) exhausted the
			// failure budget during an incident. Only a *failed* authentication
			// feeds and is gated by the limiter, so online brute force is still
			// throttled while known principals keep working.
			//
			// But full verification is expensive (a TokenReview round-trip). A
			// blocked source replaying the SAME already-proven-bad credential must
			// not force a fresh TokenReview every time, or the failure limiter is
			// cosmetic. Short-circuit that exact case from a short-TTL negative
			// cache. A *different* credential from the same source is not in the
			// cache and is still verified, so a valid principal behind a shared NAT
			// keeps working.
			// The negative cache short-circuits a REPEATED bad credential, and
			// that is all it can do — an attacker guessing tokens presents a
			// novel one every time, so nothing it sends is ever in the cache
			// and every attempt still costs a full TokenReview against the
			// kube-apiserver. Against the one attacker this limiter exists for,
			// the cache is not the throttle; the concurrency bound below is.
			//
			// Bounding rather than rejecting outright keeps the property the
			// comment above argues for: a valid principal behind a shared NAT
			// egress still authenticates, it just queues behind at most a few
			// verifications instead of amplifying into the apiserver.
			credKey := s.negativeAuth.hash(bearerCredential(r))
			select {
			case s.operatorAuthSlots <- struct{}{}:
				defer func() { <-s.operatorAuthSlots }()
			default:
				metrics.AuthFailures.WithLabelValues("operator").Inc()
				w.Header().Set("Retry-After", "1")
				http.Error(w, "too many authentications in flight", http.StatusServiceUnavailable)
				return
			}
			if s.authLimiter.blocked(source) && s.negativeAuth.seen(credKey) {
				metrics.AuthFailures.WithLabelValues("operator").Inc()
				http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
				return
			}
			// Bounded, like the agent path. Without it a wedged apiserver holds
			// operator requests open for as long as the client will wait —
			// including the panel's, and including the requests an operator
			// makes to find out what is wrong.
			authCtx, cancel := context.WithTimeout(r.Context(), agentAuthenticationTimeout)
			authenticated, err := s.operatorAuth.AuthenticateOperator(r.WithContext(authCtx), verb)
			cancel()
			if err != nil {
				status := http.StatusUnauthorized
				var statusErr HTTPStatusError
				if errors.As(err, &statusErr) {
					status = statusErr.HTTPStatus()
				}
				if status == http.StatusUnauthorized || status == http.StatusForbidden {
					s.authLimiter.record(source)
					s.negativeAuth.remember(credKey)
					metrics.AuthFailures.WithLabelValues("operator").Inc()
					slog.Warn("operator authentication failed", "remote", source, "status", status)
				}
				if s.authLimiter.blocked(source) {
					http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
					return
				}
				if status == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", `Bearer realm="kubeneuron-operator"`)
				}
				http.Error(w, "operator authentication failed", status)
				return
			}
			identity = authenticated
		case s.authLimiter.blocked(source):
			http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
			return
		default:
			s.authLimiter.record(source)
			metrics.AuthFailures.WithLabelValues("operator").Inc()
			slog.Warn("operator authentication failed", "remote", source)
			w.Header().Set("WWW-Authenticate", `Bearer realm="kubeneuron-operator"`)
			http.Error(w, "operator authentication failed", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), operatorIdentityContextKey{}, identity)))
	}
}

// resolveActor derives the audited actor. An authenticated principal is
// authoritative and cannot be overridden by the request body; the static
// shared token records the caller's claim as "token:<claim>" so audit
// readers can tell a verified identity from an asserted one.
func (s *Server) resolveActor(r *http.Request, claimed string) (string, error) {
	identity, _ := OperatorIdentityFromContext(r.Context())
	if identity.Actor != "" {
		return identity.Actor, nil
	}
	claimed = strings.TrimSpace(claimed)
	if claimed == "" {
		return "", fmt.Errorf("actor is required with the static operator token")
	}
	return "token:" + claimed, nil
}

func bearerMatches(r *http.Request, token string) bool {
	// An empty configured token matches nothing, ever.
	//
	// ConstantTimeCompare("", "") returns 1, so without this an empty token
	// plus a bare "Authorization: Bearer " header authenticates. Today the
	// enable-gate above rejects an empty s.operatorToken before reaching here,
	// so this is unreachable — but that gate reads the FIELD while this reads
	// currentOperatorToken(), and the day someone aligns the two (a
	// provider-only install whose file has not landed yet) the difference
	// between "disabled" and "open to everyone" would be this line.
	if token == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	presented := strings.TrimSpace(strings.TrimPrefix(auth, prefix))
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

// registerOperatorRoutes wires the operator/UI API onto the public mux.
func (s *Server) registerOperatorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/logout", s.handleLogout)
	mux.HandleFunc("GET /api/v1/session", s.handleSession)
	mux.HandleFunc("GET /api/v1/auth/oidc/login", s.handleOIDCLogin)
	mux.HandleFunc("GET /api/v1/auth/oidc/callback", s.handleOIDCCallback)
	mux.HandleFunc("GET /api/v1/incidents", s.requireOperator(s.handleListIncidents))
	mux.HandleFunc("GET /api/v1/incidents/{id}", s.requireOperator(s.handleIncidentDetail))
	mux.HandleFunc("POST /api/v1/incidents", s.requireOperatorMutation(s.handleManualIncident))
	mux.HandleFunc("POST /api/v1/incidents/{id}/approve", s.requireOperatorMutation(s.handleDecision(types.ApprovalApproved)))
	mux.HandleFunc("POST /api/v1/incidents/{id}/reject", s.requireOperatorMutation(s.handleDecision(types.ApprovalRejected)))
	mux.HandleFunc("POST /api/v1/incidents/{id}/acknowledge", s.requireOperatorMutation(s.handleAcknowledge))
	mux.HandleFunc("POST /api/v1/incidents/{id}/resolve", s.requireOperatorMutation(s.handleResolve))
	mux.HandleFunc("GET /api/v1/readiness", s.requireOperator(s.handleFleetReadiness))
	mux.HandleFunc("GET /api/v1/audit-events", s.requireOperator(s.handleListOperationalAuditEvents))
	mux.HandleFunc("GET /api/v1/nodes", s.requireOperator(s.handleListNodes))
	mux.HandleFunc("GET /api/v1/nodes/{node}", s.requireOperator(s.handleGetNode))
	mux.HandleFunc("GET /api/v1/nodes/{node}/readiness", s.requireOperator(s.handleNodeReadiness))
	mux.HandleFunc("GET /api/v1/nodes/{node}/evidence", s.requireOperator(s.handleNodeEvidence))
	mux.HandleFunc("GET /api/v1/nodes/{node}/health-checks", s.requireOperator(s.handleListNodeHealthChecks))
	mux.HandleFunc("GET /api/v1/nodes/{node}/accelerators", s.requireOperator(s.handleAcceleratorReports))
	mux.HandleFunc("POST /api/v1/candidates", s.requireOperationalMutation("candidate-upload", 20, s.handleCreateCandidate))
	mux.HandleFunc("GET /api/v1/candidates", s.requireOperator(s.handleListCandidates))
	mux.HandleFunc("GET /api/v1/candidates/{id}", s.requireOperator(s.handleGetCandidate))
	mux.HandleFunc("DELETE /api/v1/candidates/{id}", s.requireOperationalMutation("candidate-revoke", 60, s.handleRevokeCandidate))
	mux.HandleFunc("POST /api/v1/candidates/{id}/preview", s.requireOperationalMutation("candidate-preview", 20, s.handleCreatePreview))
	mux.HandleFunc("GET /api/v1/candidates/{id}/preview", s.requireOperator(s.handleGetCandidatePreview))
	mux.HandleFunc("GET /api/v1/previews", s.requireOperator(s.handleListPreviews))
	mux.HandleFunc("GET /api/v1/previews/{id}", s.requireOperator(s.handleGetPreview))
	mux.HandleFunc("POST /api/v1/health-checks", s.requireOperationalMutation("health-check", 30, s.handleCreateHealthCheck))
	mux.HandleFunc("GET /api/v1/health-checks", s.requireOperator(s.handleListHealthChecks))
	mux.HandleFunc("GET /api/v1/health-checks/{id}", s.requireOperator(s.handleGetHealthCheck))
	mux.HandleFunc("POST /api/v1/health-checks/{id}/cancel", s.requireOperationalMutation("health-check-cancel", 60, s.handleCancelHealthCheck))
	mux.HandleFunc("POST /api/v1/simulations", s.requireOperationalMutation("simulation", 30, s.handleCreateSimulation))
	mux.HandleFunc("GET /api/v1/simulations", s.requireOperator(s.handleListSimulations))
	mux.HandleFunc("GET /api/v1/simulations/{id}", s.requireOperator(s.handleGetSimulation))
	mux.HandleFunc("POST /api/v1/incidents/from-simulation", s.requireOperationalMutation("incident-from-simulation", 30, s.handleCreateIncidentFromSimulation))
	mux.HandleFunc("POST /api/v1/autonomy/plans", s.requireOperationalMutation("autonomy-plan", 20, s.handleCreateAutonomyPlan))
	mux.HandleFunc("GET /api/v1/autonomy/plans", s.requireOperator(s.handleListAutonomyPlans))
	mux.HandleFunc("GET /api/v1/autonomy/plans/{id}", s.requireOperator(s.handleGetAutonomyPlan))
	mux.HandleFunc("GET /api/v1/autonomy/plans/{id}/rollout", s.requireOperator(s.handleGetAutonomyRollout))
	mux.HandleFunc("POST /api/v1/autonomy/plans/{id}/simulation", s.requireOperationalMutation("autonomy-simulation", 30, s.handleAttachAutonomySimulation))
	mux.HandleFunc("POST /api/v1/autonomy/plans/{id}/approve", s.requireOperationalMutation("autonomy-approve", 60, s.handleApproveAutonomyPlan))
	mux.HandleFunc("POST /api/v1/autonomy/plans/{id}/pause", s.requireOperationalMutation("autonomy-pause", 60, s.handlePauseAutonomyPlan))
	mux.HandleFunc("POST /api/v1/autonomy/plans/{id}/resume", s.requireOperationalMutation("autonomy-resume", 60, s.handleResumeAutonomyPlan))
	mux.HandleFunc("POST /api/v1/autonomy/plans/{id}/rollback", s.requireOperationalMutation("autonomy-rollback", 60, s.handleRollbackAutonomyPlan))
	mux.HandleFunc("GET /api/v1/report/recovery", s.requireOperator(s.handleRecoveryReport))
	mux.HandleFunc("GET /api/v1/targets", s.requireOperator(s.handleTargets))
	mux.HandleFunc("GET /api/v1/runtime-config", s.requireOperator(s.handleRuntimeConfig))
	mux.HandleFunc("GET /api/v1/pause", s.requireOperator(s.handleGetPause))
	mux.HandleFunc("POST /api/v1/pause", s.requireOperatorMutation(s.handleSetPause(true)))
	mux.HandleFunc("DELETE /api/v1/pause", s.requireOperatorMutation(s.handleSetPause(false)))
	if s.backupStore != nil {
		mux.HandleFunc("GET /api/v1/backup", s.requireOperator(s.handleBackup))
	}
}

// handleBackup streams a transactionally consistent snapshot of the workflow
// store. The snapshot is produced with VACUUM INTO next to the live database
// (same volume, so no cross-device copies) and removed after streaming; the
// distroless controller image needs no sqlite3 binary for this.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	tmpPath := filepath.Join(s.backupDir, fmt.Sprintf(".backup-%s-%d.db", stamp, os.Getpid()))
	defer func() {
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
			// The next backup uses a fresh name; a leftover file only costs
			// space, so log-and-continue is deliberate.
			slog.Warn("removing backup temp file failed", "path", tmpPath, "err", err)
		}
	}()
	if err := s.backupStore.BackupTo(r.Context(), tmpPath); err != nil {
		http.Error(w, "backup failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		http.Error(w, "backup failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "backup failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "kubeneuron-"+stamp+".db"))
	if _, err := io.Copy(w, f); err != nil {
		slog.Warn("streaming backup interrupted", "err", err)
	}
}

// maxStateFilter caps the ?state= list. See the comment at its use.
const maxStateFilter = 32

func (s *Server) handleListIncidents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var states []string
	if v := strings.TrimSpace(q.Get("state")); v != "" {
		states = strings.Split(v, ",")
		// Every element becomes one SQL placeholder. PostgreSQL's extended
		// protocol refuses a statement past 65535 of them and SQLite's limit is
		// lower, so an unbounded list turns one authenticated GET into a 500.
		// There are nine incident states; anything beyond a generous multiple
		// of that is a mistake or an attempt.
		if len(states) > maxStateFilter {
			http.Error(w, "too many state values", http.StatusBadRequest)
			return
		}
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "bad limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
	incidents, err := s.operator.ListIncidents(r.Context(), states, q.Get("node"), limit)
	if err != nil {
		http.Error(w, "listing incidents failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, incidents)
}

// IncidentDetail is the incident plus its full audit trail.
type IncidentDetail struct {
	Incident *types.Incident     `json:"incident"`
	Audit    []*types.AuditEntry `json:"audit"`
}

func (s *Server) handleIncidentDetail(w http.ResponseWriter, r *http.Request) {
	inc, audit, err := s.operator.IncidentDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "incident not found", http.StatusNotFound)
		return
	}
	writeJSON(w, IncidentDetail{Incident: inc, Audit: audit})
}

// ManualIncidentRequest triggers a manual remediation signal.
type ManualIncidentRequest struct {
	Node     string `json:"node"`
	GPUUUID  string `json:"gpu_uuid,omitempty"`
	GPUIndex int    `json:"gpu_index,omitempty"`
	Class    string `json:"class"`
	Actor    string `json:"actor"`
}

func (s *Server) handleManualIncident(w http.ResponseWriter, r *http.Request) {
	var req ManualIncidentRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Node) == "" || strings.TrimSpace(req.Class) == "" {
		http.Error(w, "node and class are required", http.StatusBadRequest)
		return
	}
	actor, err := s.resolveActor(r, req.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	signal := types.Signal{
		Target:   types.Target{Node: req.Node, GPUUUID: req.GPUUUID, GPUIndex: req.GPUIndex},
		Class:    types.ProblemClass(req.Class),
		Severity: types.SeverityWarning,
		Source:   types.SourceManual,
		Evidence: map[string]string{"actor": actor, "trigger": "manual"},
	}
	// A 202 is a promise that the incident can survive this process. The old
	// in-memory fallback was appropriate only for pre-durable webhook tests;
	// for a human-triggered remediation it could acknowledge work that vanished
	// on a full signal channel or leader restart.
	durable, ok := s.backend.(DurableSignalBackend)
	if !ok {
		http.Error(w, "manual incident intake is unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := durable.IngestSignal(r.Context(), signal); err != nil {
		http.Error(w, "manual incident persistence unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// DecisionRequest carries the audited actor of an approve/reject/resolve.
type DecisionRequest struct {
	Actor  string `json:"actor"`
	Reason string `json:"reason,omitempty"`
	// ParkEpoch is the approval round the client DISPLAYED when the human
	// decided (Incident.ApprovalEpoch at read time). When set (>0) the
	// decision is refused if a re-park has since minted a newer round, so a
	// click can never be recorded against content the human never saw. Zero
	// (older clients) keeps the bind-to-current behavior.
	ParkEpoch int `json:"park_epoch,omitempty"`
}

func (s *Server) handleDecision(decision types.ApprovalDecision) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req DecisionRequest
		if !decodeStrict(w, r, &req) {
			return
		}
		actor, err := s.resolveActor(r, req.Actor)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.operator.DecideApproval(r.Context(), r.PathValue("id"), actor, "api", decision, req.ParkEpoch, req.Reason); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	if backend, ok := s.operator.(IncidentResolutionBackend); ok {
		s.handleIdempotentResolve(w, r, backend)
		return
	}
	var req DecisionRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	actor, err := s.resolveActor(r, req.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.operator.ResolveIncident(r.Context(), r.PathValue("id"), actor, req.Reason); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type resolveIncidentRequest struct {
	Actor           string `json:"actor"`
	Reason          string `json:"reason,omitempty"`
	ResourceVersion int    `json:"resource_version,omitempty"`
}

func (s *Server) handleIdempotentResolve(w http.ResponseWriter, r *http.Request, backend IncidentResolutionBackend) {
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request resolveIncidentRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	actor, err := s.resolveActor(r, request.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	incident, replayed, err := backend.ResolveIncidentRequest(r.Context(), r.PathValue("id"), actor, request.Reason, request.ResourceVersion, key)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "incident not found", http.StatusNotFound)
		case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrOperationalConflict):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeOperationalJSON(w, http.StatusOK, incident)
}

type acknowledgeIncidentRequest struct {
	Actor           string `json:"actor"`
	Reason          string `json:"reason,omitempty"`
	ResourceVersion int    `json:"resource_version,omitempty"`
}

func (s *Server) handleAcknowledge(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.operator.(IncidentAcknowledgementBackend)
	if !ok {
		http.Error(w, "incident acknowledgement is unavailable", http.StatusServiceUnavailable)
		return
	}
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request acknowledgeIncidentRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	actor, err := s.resolveActor(r, request.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	incident, replayed, err := backend.AcknowledgeIncident(r.Context(), r.PathValue("id"), actor, request.Reason, request.ResourceVersion, key)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "incident not found", http.StatusNotFound)
		case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrOperationalConflict):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeOperationalJSON(w, http.StatusOK, incident)
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.operator.Nodes(r.Context())
	if err != nil {
		http.Error(w, "listing nodes failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, nodes)
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	node, err := s.operator.Node(r.Context(), r.PathValue("node"))
	if err != nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	writeJSON(w, node)
}

// handleAcceleratorReports exposes persisted current reports only behind the
// operator bearer token. It has no mutation path and a report's capabilities
// remain subject to the server-owned runtime profile gate.
func (s *Server) handleAcceleratorReports(w http.ResponseWriter, r *http.Request) {
	reportsBackend, ok := s.operator.(AcceleratorReportOperatorBackend)
	if !ok {
		http.Error(w, "accelerator report visibility unavailable", http.StatusServiceUnavailable)
		return
	}
	reports, err := reportsBackend.AcceleratorReports(r.Context(), r.PathValue("node"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "node not found", http.StatusNotFound)
			return
		}
		http.Error(w, "accelerator report visibility unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, reports)
}

const (
	// defaultReportWindow matches `kubeneuronctl report`'s default: a week is
	// the shortest window in which a fleet's weekend behaviour is visible.
	defaultReportWindow = 7 * 24 * time.Hour
	// maxReportWindow rejects windows longer than any retention policy keeps.
	// A year of "no incidents" that is really "no rows" must not be served as
	// an answer. The backend enforces the same bound independently.
	maxReportWindow = 366 * 24 * time.Hour
)

// handleRecoveryReport aggregates the window server-side. The alternative —
// letting the client pull the incident list and add it up — would ship
// thousands of rows to compute six numbers, and would put the definition of
// "recovered" in every client instead of in one place.
func (s *Server) handleRecoveryReport(w http.ResponseWriter, r *http.Request) {
	reportBackend, ok := s.operator.(RecoveryReportBackend)
	if !ok {
		http.Error(w, "recovery report unavailable", http.StatusServiceUnavailable)
		return
	}
	window := defaultReportWindow
	if v := strings.TrimSpace(r.URL.Query().Get("window")); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil || parsed <= 0 || parsed > maxReportWindow {
			http.Error(w, "window must be a positive Go duration no longer than 8784h, e.g. 720h", http.StatusBadRequest)
			return
		}
		window = parsed
	}
	report, err := reportBackend.RecoveryReport(r.Context(), window)
	if err != nil {
		http.Error(w, "recovery report failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, report)
}

// sdTarget is one Prometheus HTTP service-discovery group.
type sdTarget struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// handleTargets serves vmagent/Prometheus http_sd for bare-metal fleets:
// every registered node becomes a dcgm-exporter scrape target. Configure
// vmagent with the operator bearer token (http_sd_configs authorization).
func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.operator.Nodes(r.Context())
	if err != nil {
		http.Error(w, "listing nodes failed", http.StatusInternalServerError)
		return
	}
	port := strings.TrimSpace(r.URL.Query().Get("port"))
	if port == "" {
		port = "9400" // dcgm-exporter default
	}
	if _, err := strconv.Atoi(port); err != nil {
		http.Error(w, "bad port", http.StatusBadRequest)
		return
	}
	groups := make([]sdTarget, 0, len(nodes))
	for _, n := range nodes {
		groups = append(groups, sdTarget{
			Targets: []string{n.Name + ":" + port},
			Labels: map[string]string{
				"node":                n.Name,
				"kubeneuron_platform": n.Platform,
			},
		})
	}
	writeJSON(w, groups)
}

func (s *Server) handleGetPause(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]bool{"paused": s.operator.Paused()})
}

func (s *Server) handleSetPause(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claimed := strings.TrimSpace(r.URL.Query().Get("actor"))
		if r.Method == http.MethodPost {
			var req DecisionRequest
			if !decodeStrict(w, r, &req) {
				return
			}
			claimed = strings.TrimSpace(req.Actor)
		}
		actor, err := s.resolveActor(r, claimed)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.operator.SetPaused(r.Context(), paused, actor); err != nil {
			http.Error(w, "persisting global pause failed", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentEventBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSONDecodeError(w, "bad request", err)
		return false
	}
	if err := ensureJSONEOF(dec); err != nil {
		writeJSONDecodeError(w, "bad request", err)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleRuntimeConfig reports the identity and coarse shape of the runtime
// configuration currently live in this process — the observable end of the
// config pipeline (operator compiles → ConfigMap → kubelet sync → in-place
// reload). When this digest lags KubeNeuron.status.configDigest, the rollout
// has not landed here yet.
func (s *Server) handleRuntimeConfig(w http.ResponseWriter, _ *http.Request) {
	info := s.runtimeConfigInfo.Load()
	if info == nil {
		http.Error(w, "runtime configuration identity not published yet", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, info)
}
