// Package httpapi exposes the controller's REST API: webhook receivers
// (Alertmanager, Slack interactions), the agent event/registration
// endpoints, and the operator/UI API (incidents, approvals, nodes, pause,
// config versions). The embedded Web UI (web/) is served from the same
// mux in Phase 2.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	maxAgentRegistrationBytes      = 1 << 20
	maxAgentEventBytes             = 1 << 20
	maxAgentAcceleratorReportBytes = 1 << 20
	maxAlertmanagerBytes           = 1 << 20
	agentAuthenticationTimeout     = 10 * time.Second
)

// AgentPrincipal is the server-derived identity of an authenticated agent
// request. Handlers must use NodeName from this value rather than trusting the
// node name supplied in JSON by the caller.
type AgentPrincipal struct {
	Namespace      string
	ServiceAccount string
	PodName        string
	PodUID         string
	NodeName       string
	NodeUID        string
}

// AgentAuthenticator authenticates one request and binds it to a live
// Kubernetes Pod and Node. The TLS listener performs certificate validation;
// this second proof maps the projected Pod token to a node identity.
type AgentAuthenticator interface {
	AuthenticateAgent(*http.Request) (AgentPrincipal, error)
}

// HTTPStatusError allows an authenticator to select a safe HTTP status without
// exposing its internal error detail to an untrusted caller.
type HTTPStatusError interface {
	error
	HTTPStatus() int
}

type agentPrincipalContextKey struct{}

// AgentPrincipalFromContext returns the authenticated identity installed by
// AgentRoutes.
func AgentPrincipalFromContext(ctx context.Context) (AgentPrincipal, bool) {
	principal, ok := ctx.Value(agentPrincipalContextKey{}).(AgentPrincipal)
	return principal, ok
}

// Backend is what the HTTP layer needs from the controller. Defined here
// (consumer side) to keep the dependency arrow pointing inward.
type Backend interface {
	// HandleSignal ingests a normalized signal (opens/updates incidents).
	HandleSignal(r *http.Request, sig types.Signal)
	// HandleAgentEvent durably ingests a raw agent event before it is
	// classified. A non-nil error means the receiver must not acknowledge the
	// event: agents rely on a non-2xx response to retain and replay their
	// fsynced spool entry.
	HandleAgentEvent(r *http.Request, ev types.AgentEvent) error
	// RegisterNode ingests an agent self-registration.
	RegisterNode(r *http.Request, registration types.AgentRegistration) error
	// NextAction atomically claims the oldest available action for the node,
	// or returns nil when the queue is empty. The QueuedAction lease token is
	// delivered only to that authenticated node.
	NextAction(r *http.Request, node string) (*types.QueuedAction, error)
	// CompleteAction records the node's result for a queued action. It must
	// reject results for actions queued to a different node or submitted with
	// a stale/incorrect action lease token.
	CompleteAction(r *http.Request, node, actionID, leaseToken string, res types.ActionResult) error
}

// AcceleratorReportBackend is the narrow optional extension for the
// versioned accelerator-preflight report. It intentionally remains separate
// while the controller/store integration lands: a controller that has not
// implemented durable report storage responds 503 instead of accepting an
// observation it would lose. The report protocol carries no execution path.
type AcceleratorReportBackend interface {
	HandleAcceleratorReport(r *http.Request, report types.AgentAcceleratorReport) error
}

// AcceleratorObservationProfileBackend supplies the narrow, server-selected
// profile binding an agent needs before it emits a managed accelerator
// observation. It intentionally returns only a vendor and immutable digest;
// action policy and execution decisions never travel to the node agent.
type AcceleratorObservationProfileBackend interface {
	AcceleratorObservationProfile(ctx context.Context, node string, vendor types.AcceleratorVendor) (*types.AgentAcceleratorObservationProfile, error)
}

// Server carries the route handlers.
type Server struct {
	backend       Backend
	operator      OperatorBackend
	operatorToken string
	operatorAuth  OperatorAuthenticator
	// operatorTokenFn / webhookTokenFn, when set, resolve the current token
	// on every request so a rotated Secret takes effect without a restart.
	operatorTokenFn func() string
	webhookToken    string
	webhookTokenFn  func() string
	authLimiter     *failureLimiter
	sessions        *sessionStore
	basicUsersDir   string
	oidc            *oidcAuthenticator
	catalog         *detect.Catalog
	metrics         http.Handler
	ui              http.Handler
	backupStore     BackupStore
	backupDir       string
	readyCheck      func() bool
}

// BackupStore produces a transactionally consistent database snapshot at the
// given path (which must not exist yet).
type BackupStore interface {
	BackupTo(ctx context.Context, destPath string) error
}

// SetBackupStore enables GET /api/v1/backup: a consistent workflow-store
// snapshot streamed to authenticated operators. dir must be writable and on
// a volume with room for one extra copy of the database.
func (s *Server) SetBackupStore(store BackupStore, dir string) {
	s.backupStore = store
	s.backupDir = dir
}

// SetReadyCheck gates GET /readyz. With leader election an active/standby
// pair keeps only the leader Ready, so Services route every request —
// human, webhook, and agent — to exactly one writer.
func (s *Server) SetReadyCheck(ready func() bool) { s.readyCheck = ready }

// SetUI serves the embedded control panel at GET /{$} and /ui/. The static
// assets are public; every API call the panel makes still requires the
// operator bearer token.
func (s *Server) SetUI(files http.FileSystem) { s.ui = http.FileServer(files) }

// SetMetricsHandler serves Prometheus metrics at GET /metrics on the public
// listener. Metrics are operational aggregates; no secrets appear in them.
func (s *Server) SetMetricsHandler(h http.Handler) { s.metrics = h }

// SetSignalCatalog installs declarative signal overrides for alert mapping;
// nil keeps the built-in catalog.
func (s *Server) SetSignalCatalog(catalog *detect.Catalog) { s.catalog = catalog }

// New builds the API server.
func New(backend Backend) *Server {
	return &Server{backend: backend, authLimiter: newFailureLimiter(), sessions: newSessionStore()}
}

// Routes returns the controller's non-agent HTTP mux. Agent endpoints are
// deliberately absent so health probes and webhook senders never force the
// mTLS listener to weaken client-certificate verification.
//
// Route map — every route listed here is registered. Slack interactive
// approvals, SSE streaming, a metrics proxy, versioned config, and token
// login are design targets (docs/design.md §API) that do NOT exist yet and
// are deliberately not listed:
//
//	POST   /api/v1/webhooks/alertmanager   Alertmanager webhook (slow path)
//	GET    /api/v1/nodes                   fleet state
//	GET    /api/v1/nodes/{node}            node detail
//	GET    /api/v1/incidents               list (filter: ?state=)
//	GET    /api/v1/incidents/{id}          detail incl. audit trail
//	POST   /api/v1/incidents               manual remediation trigger
//	POST   /api/v1/incidents/{id}/approve  approve pending step
//	POST   /api/v1/incidents/{id}/reject   reject pending step
//	POST   /api/v1/incidents/{id}/resolve  manual resolve
//	POST   /api/v1/pause                   big red button
//	DELETE /api/v1/pause                   resume
//	GET    /api/v1/targets                 vmagent http_sd
//	GET    /healthz                        liveness
//	GET    /metrics                        controller self-metrics
//	GET    /, /ui/                         embedded control panel
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/webhooks/alertmanager", s.handleAlertmanager)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.readyCheck != nil && !s.readyCheck() {
			http.Error(w, "standby: not the elected leader", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics)
	}
	if s.ui != nil {
		mux.Handle("GET /{$}", s.ui)
		mux.Handle("GET /ui/", http.StripPrefix("/ui", s.ui))
	}
	s.registerOperatorRoutes(mux)

	// TODO(phase-2): role-based auth (viewer/operator/admin) instead of the
	// single operator token; SSE stream, VM proxy, config versioning, Web UI
	// static serving via go:embed from web/dist.

	return mux
}

// AgentRoutes returns only the routes used by node agents. Every route,
// including the registration capability probe, requires both the mTLS
// listener and a successfully authenticated Pod-bound token.
func (s *Server) AgentRoutes(authenticator AgentAuthenticator) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/events", s.handleAgentEvent)
	mux.HandleFunc("GET "+types.AgentRegistrationPath, s.handleRegistrationCapability)
	mux.HandleFunc("POST "+types.AgentRegistrationPath, s.handleRegister)
	mux.HandleFunc("GET "+types.AgentAcceleratorProfilePath, s.handleAcceleratorObservationProfile)
	mux.HandleFunc("POST "+types.AgentAcceleratorReportPath, s.handleAcceleratorReport)
	// The first lease-bearing action protocol is intentionally versioned. The
	// old unleased route is not registered: mixed controller/agent versions
	// hold actions rather than risking an unconditionally accepted result.
	mux.HandleFunc("GET "+types.AgentActionLeasePath, s.handleNextAction)
	mux.HandleFunc("POST "+types.AgentActionLeasePath+"/{id}/result", s.handleActionResult)
	return authenticateAgent(authenticator, mux)
}

func authenticateAgent(authenticator AgentAuthenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticator == nil {
			http.Error(w, "agent authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		authContext, cancel := context.WithTimeout(r.Context(), agentAuthenticationTimeout)
		principal, err := authenticator.AuthenticateAgent(r.WithContext(authContext))
		cancel()
		if err != nil {
			status := http.StatusUnauthorized
			var statusErr HTTPStatusError
			if errors.As(err, &statusErr) {
				status = statusErr.HTTPStatus()
			}
			message := "agent authentication failed"
			if status == http.StatusForbidden {
				message = "agent is not authorized for the requested node"
			} else if status >= http.StatusInternalServerError {
				message = "agent authentication unavailable"
			}
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				metrics.AuthFailures.WithLabelValues("agent").Inc()
			}
			if status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="kubeneuron-agent", error="invalid_token"`)
			}
			http.Error(w, message, status)
			return
		}
		if strings.TrimSpace(principal.NodeName) == "" {
			http.Error(w, "agent authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		ctx := context.WithValue(r.Context(), agentPrincipalContextKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// handleRegistrationCapability lets a new agent prove that the controller
// understands the narrow registration wire format before sending it. Legacy
// controllers accepted a full Node on the POST route, so the exact token is a
// deliberate mixed-version corruption guard rather than a general health
// response.
func (s *Server) handleRegistrationCapability(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, types.AgentRegistrationProtocol+"\n")
}

// handleAlertmanager ingests the slow-path webhook. When a webhook token is
// configured, Alertmanager must present it (http_config.authorization).
func (s *Server) handleAlertmanager(w http.ResponseWriter, r *http.Request) {
	if token := s.currentWebhookToken(); token != "" && !bearerMatches(r, token) {
		source := remoteSource(r)
		if s.authLimiter.blocked(source) {
			http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
			return
		}
		s.authLimiter.record(source)
		metrics.AuthFailures.WithLabelValues("webhook").Inc()
		slog.Warn("webhook authentication failed", "remote", source)
		w.Header().Set("WWW-Authenticate", `Bearer realm="kubeneuron-webhook"`)
		http.Error(w, "webhook authentication failed", http.StatusUnauthorized)
		return
	}
	var payload detect.AlertmanagerWebhook
	r.Body = http.MaxBytesReader(w, r.Body, maxAlertmanagerBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&payload); err != nil {
		writeJSONDecodeError(w, "bad payload", err)
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		writeJSONDecodeError(w, "bad payload", err)
		return
	}
	for _, alert := range payload.Alerts {
		if sig, ok := s.catalog.SignalFromAlert(alert); ok {
			s.backend.HandleSignal(r, sig)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleAgentEvent ingests the fast-path XID events.
func (s *Server) handleAgentEvent(w http.ResponseWriter, r *http.Request) {
	var ev types.AgentEvent
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentEventBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ev); err != nil {
		writeJSONDecodeError(w, "bad event", err)
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		writeJSONDecodeError(w, "bad event", err)
		return
	}
	principal, ok := AgentPrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "agent authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	if ev.Node != principal.NodeName {
		http.Error(w, "agent is not authorized for the requested node", http.StatusForbidden)
		return
	}
	// Stamp the authenticated value even after the equality check so future
	// wire changes cannot accidentally reintroduce caller-owned identity.
	ev.Node = principal.NodeName
	if err := s.backend.HandleAgentEvent(r, ev); err != nil {
		http.Error(w, "event persistence unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleRegister ingests agent self-registrations.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var registration types.AgentRegistration
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentRegistrationBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&registration); err != nil {
		writeJSONDecodeError(w, "bad registration", err)
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		writeJSONDecodeError(w, "bad registration", err)
		return
	}
	trimmedName := strings.TrimSpace(registration.Name)
	if trimmedName == "" {
		http.Error(w, "bad registration: name is required", http.StatusBadRequest)
		return
	}
	if trimmedName != registration.Name {
		http.Error(w, "bad registration: name must not have leading or trailing whitespace", http.StatusBadRequest)
		return
	}
	principal, ok := AgentPrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "agent authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	if registration.Name != principal.NodeName {
		http.Error(w, "agent is not authorized for the requested node", http.StatusForbidden)
		return
	}
	registration.Name = principal.NodeName
	registration.NodeUID = principal.NodeUID
	if err := s.backend.RegisterNode(r, registration); err != nil {
		http.Error(w, "registration persistence unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAcceleratorObservationProfile resolves the runtime profile for the
// authenticated node only. No matching profile is a normal observation-only
// state (204), while an unavailable or ambiguous resolution fails closed so
// the agent does not send a report carrying a manually supplied digest.
func (s *Server) handleAcceleratorObservationProfile(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if len(query) != 1 || len(query["vendor"]) != 1 {
		http.Error(w, "accelerator profile vendor is required", http.StatusBadRequest)
		return
	}
	vendor := types.AcceleratorVendor(query.Get("vendor"))
	if !vendor.Valid() {
		http.Error(w, "unsupported accelerator profile vendor", http.StatusBadRequest)
		return
	}
	principal, ok := AgentPrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "agent authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	profiles, ok := s.backend.(AcceleratorObservationProfileBackend)
	if !ok {
		http.Error(w, "accelerator profile service unavailable", http.StatusServiceUnavailable)
		return
	}
	profile, err := profiles.AcceleratorObservationProfile(r.Context(), principal.NodeName, vendor)
	if err != nil {
		http.Error(w, "accelerator profile service unavailable", http.StatusServiceUnavailable)
		return
	}
	if profile == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if profile.Vendor != vendor || profile.Validate() != nil {
		http.Error(w, "accelerator profile service unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(profile); err != nil {
		// The response has no side effect and a client retry is safe. There is
		// no useful recovery action after a socket write failure.
		return
	}
}

// handleAcceleratorReport durably receives the local accelerator inventory
// and preflight observation. This endpoint is deliberately observation-only:
// neither a capability declaration nor readiness=ready enables remediation.
func (s *Server) handleAcceleratorReport(w http.ResponseWriter, r *http.Request) {
	var report types.AgentAcceleratorReport
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentAcceleratorReportBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		writeJSONDecodeError(w, "bad accelerator report", err)
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		writeJSONDecodeError(w, "bad accelerator report", err)
		return
	}
	if report.NodeUID != "" {
		http.Error(w, "bad accelerator report: node_uid is server-owned", http.StatusBadRequest)
		return
	}
	if err := report.Validate(); err != nil {
		http.Error(w, "bad accelerator report: "+err.Error(), http.StatusBadRequest)
		return
	}
	principal, ok := AgentPrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "agent authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	if report.Node != principal.NodeName {
		http.Error(w, "agent is not authorized for the requested node", http.StatusForbidden)
		return
	}
	if strings.TrimSpace(principal.NodeUID) == "" {
		http.Error(w, "agent authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	// Stamp the authenticated value even after equality validation so future
	// changes cannot accidentally make wire-supplied identity authoritative.
	report.Node = principal.NodeName
	report.NodeUID = principal.NodeUID
	reportBackend, ok := s.backend.(AcceleratorReportBackend)
	if !ok {
		http.Error(w, "accelerator report persistence unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := reportBackend.HandleAcceleratorReport(r, report); err != nil {
		// A stale observation is not a storage outage. Returning Conflict keeps
		// the agent from treating an obsolete snapshot as acknowledged; its next
		// preflight must produce a newer observation. All other errors remain
		// retryable service failures.
		if errors.Is(err, store.ErrStaleAcceleratorReport) {
			http.Error(w, "accelerator report is stale", http.StatusConflict)
			return
		}
		http.Error(w, "accelerator report persistence unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleNextAction hands the authenticated node its oldest queued action.
// The node identity comes exclusively from the verified principal, so an
// agent can never poll another node's work.
func (s *Server) handleNextAction(w http.ResponseWriter, r *http.Request) {
	principal, ok := AgentPrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "agent authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	queued, err := s.backend.NextAction(r, principal.NodeName)
	if err != nil {
		http.Error(w, "action queue unavailable", http.StatusServiceUnavailable)
		return
	}
	if queued == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if queued.LeaseToken == "" {
		http.Error(w, "action queue returned an unleased action", http.StatusServiceUnavailable)
		return
	}
	if queued.LeaseExpiresAt.IsZero() {
		http.Error(w, "action queue returned a lease without expiry", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(types.AgentActionLeaseHeader, queued.LeaseToken)
	w.Header().Set(types.AgentActionLeaseExpiresHeader, queued.LeaseExpiresAt.UTC().Format(time.RFC3339Nano))
	_ = json.NewEncoder(w).Encode(queued.Action)
}

// handleActionResult records the authenticated node's result for an action.
func (s *Server) handleActionResult(w http.ResponseWriter, r *http.Request) {
	principal, ok := AgentPrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "agent authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	var res types.ActionResult
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentEventBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		writeJSONDecodeError(w, "bad action result", err)
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		writeJSONDecodeError(w, "bad action result", err)
		return
	}
	actionID := r.PathValue("id")
	if actionID == "" || res.ActionID != actionID {
		http.Error(w, "bad action result: action ID mismatch", http.StatusBadRequest)
		return
	}
	leaseToken := r.Header.Get(types.AgentActionLeaseHeader)
	if leaseToken == "" {
		http.Error(w, "bad action result: missing action lease", http.StatusBadRequest)
		return
	}
	if err := s.backend.CompleteAction(r, principal.NodeName, actionID, leaseToken, res); err != nil {
		http.Error(w, "action result rejected", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSONDecodeError(w http.ResponseWriter, prefix string, err error) {
	status := http.StatusBadRequest
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		status = http.StatusRequestEntityTooLarge
	}
	http.Error(w, prefix+": "+err.Error(), status)
}

func ensureJSONEOF(dec *json.Decoder) error {
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
