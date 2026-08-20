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
	SetPaused(paused bool, actor string)
	Paused() bool
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
	return s.requireOperatorVerb("update", next)
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
			credKey := s.negativeAuth.hash(bearerCredential(r))
			if s.authLimiter.blocked(source) && s.negativeAuth.seen(credKey) {
				metrics.AuthFailures.WithLabelValues("operator").Inc()
				http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
				return
			}
			authenticated, err := s.operatorAuth.AuthenticateOperator(r, verb)
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
	mux.HandleFunc("POST /api/v1/incidents/{id}/resolve", s.requireOperatorMutation(s.handleResolve))
	mux.HandleFunc("GET /api/v1/nodes", s.requireOperator(s.handleListNodes))
	mux.HandleFunc("GET /api/v1/nodes/{node}", s.requireOperator(s.handleGetNode))
	mux.HandleFunc("GET /api/v1/nodes/{node}/accelerators", s.requireOperator(s.handleAcceleratorReports))
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

func (s *Server) handleListIncidents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var states []string
	if v := strings.TrimSpace(q.Get("state")); v != "" {
		states = strings.Split(v, ",")
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
	s.backend.HandleSignal(r, types.Signal{
		Target:   types.Target{Node: req.Node, GPUUUID: req.GPUUUID, GPUIndex: req.GPUIndex},
		Class:    types.ProblemClass(req.Class),
		Severity: types.SeverityWarning,
		Source:   types.SourceManual,
		Evidence: map[string]string{"actor": actor, "trigger": "manual"},
	})
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
		s.operator.SetPaused(paused, actor)
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
