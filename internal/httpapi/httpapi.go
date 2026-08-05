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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
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
	// RegisterNode ingests an agent self-registration and answers the node's
	// controller-served arming (unknown when it cannot be computed); the v2
	// route returns the answer to the agent via AgentArmingHeader.
	RegisterNode(r *http.Request, registration types.AgentRegistration) (types.AgentArming, error)
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
	// webhookInsecureAllowed opts the Alertmanager webhook into running without
	// a token. Off by default: with no token the webhook fails closed, because
	// an unauthenticated caller could POST a firing critical alert for any node
	// and drive cordon/drain. Only an explicit development opt-in flips it.
	webhookInsecureAllowed bool
	// trustProxyHeaders lets the panel honor X-Forwarded-Proto when deciding
	// whether a request arrived over TLS, so cookies are marked Secure behind a
	// TLS-terminating load balancer that speaks plain HTTP to the controller.
	trustProxyHeaders bool
	authLimiter       *failureLimiter
	negativeAuth      *negativeAuthCache
	sessions          *sessionStore
	basicUsersDir     string
	oidc              *oidcAuthenticator
	catalog           atomic.Pointer[detect.Catalog]
	metrics           http.Handler
	ui                http.Handler
	backupStore       BackupStore
	backupDir         string
	readyCheck        func() bool
	runtimeConfigInfo atomic.Pointer[RuntimeConfigInfo]
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

// RuntimeConfigInfo identifies the runtime configuration currently live in
// this process. SourceDigest is the operator-compiled snapshot digest (empty
// for file-based deployments); the counts are a coarse shape summary so an
// operator can sanity-check what loaded without reading the ConfigMaps.
type RuntimeConfigInfo struct {
	SourceDigest string    `json:"source_digest,omitempty"`
	LoadedAt     time.Time `json:"loaded_at"`
	Playbooks    int       `json:"playbooks"`
	Policies     int       `json:"policies"`
	SignalRules  int       `json:"signal_rules"`
}

// SetRuntimeConfigInfo publishes the identity of the configuration a
// successful (re)load installed. The digest is world-readable already (it is
// KubeNeuron.status.configDigest), so /readyz may expose it unauthenticated;
// the richer endpoint sits behind the operator token.
func (s *Server) SetRuntimeConfigInfo(info RuntimeConfigInfo) {
	s.runtimeConfigInfo.Store(&info)
}

// SetSignalCatalog installs declarative signal overrides for alert mapping;
// nil keeps the built-in catalog.
func (s *Server) SetSignalCatalog(catalog *detect.Catalog) { s.catalog.Store(catalog) }

// New builds the API server.
func New(backend Backend) *Server {
	return &Server{
		backend:      backend,
		authLimiter:  newFailureLimiter(),
		negativeAuth: newNegativeAuthCache(),
		sessions:     newSessionStore(),
	}
}

// AllowInsecureWebhook opts the Alertmanager webhook into running without a
// token (development only). Without it, and without a configured webhook token,
// the webhook fails closed. Production installs mandate
// spec.notifications.webhookToken, so this is never enabled there.
func (s *Server) AllowInsecureWebhook() { s.webhookInsecureAllowed = true }

// TrustProxyHeaders enables honoring X-Forwarded-Proto when deciding whether a
// request is secure. Enable it only when the controller sits behind a trusted
// TLS-terminating proxy/load balancer, so panel cookies are marked Secure even
// though the proxy speaks plain HTTP to the controller.
func (s *Server) TrustProxyHeaders(trust bool) { s.trustProxyHeaders = trust }

// requestIsSecure reports whether the request reached the controller over TLS,
// directly (r.TLS) or via a trusted TLS-terminating proxy (X-Forwarded-Proto).
// It gates the Secure attribute on session/state cookies.
func (s *Server) requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !s.trustProxyHeaders {
		return false
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i] // the client-facing scheme is the first hop
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
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
		// The loaded-config digest rides on readyz so a harness (or a human
		// with curl) can see WHICH configuration is live without auth — the
		// digest is already world-readable in KubeNeuron.status.configDigest.
		if info := s.runtimeConfigInfo.Load(); info != nil && info.SourceDigest != "" {
			_, _ = fmt.Fprintf(w, "ready config=%s", info.SourceDigest)
			return
		}
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
	mux.HandleFunc("GET "+types.AgentRegistrationV2Path, s.handleRegistrationCapabilityV2)
	mux.HandleFunc("POST "+types.AgentRegistrationV2Path, s.handleRegisterV2)
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

// handleRegistrationCapabilityV2 advertises the v2 registration protocol
// (arming always declared). Same exact-token discipline as v1.
func (s *Server) handleRegistrationCapabilityV2(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, types.AgentRegistrationV2Protocol+"\n")
}

// handleAlertmanager ingests the slow-path webhook. It fails closed: a webhook
// token must be configured (Alertmanager presents it via
// http_config.authorization) unless an operator has explicitly opted into an
// insecure development mode. A firing alert can drive cordon/drain, so an
// unauthenticated webhook is a remediation-driven denial-of-service vector.
func (s *Server) handleAlertmanager(w http.ResponseWriter, r *http.Request) {
	switch token := s.currentWebhookToken(); {
	case token != "":
		if !bearerMatches(r, token) {
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
	case s.webhookInsecureAllowed:
		// Explicit development opt-in: run unauthenticated. Never reached in a
		// production install, which mandates spec.notifications.webhookToken.
	default:
		// Fail CLOSED. Without a token anyone with network reach could POST a
		// firing critical alert for an arbitrary node and drive remediation.
		metrics.AuthFailures.WithLabelValues("webhook").Inc()
		slog.Warn("rejecting Alertmanager webhook: no webhook token configured", "remote", remoteSource(r))
		w.Header().Set("WWW-Authenticate", `Bearer realm="kubeneuron-webhook"`)
		http.Error(w, "webhook authentication is not configured", http.StatusUnauthorized)
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
		sig, ok := s.catalog.Load().SignalFromAlert(alert)
		if !ok {
			continue
		}
		// Cross-check the node the alert claims against known inventory. The node
		// identity comes from spoofable alert labels, so an alert for a node the
		// controller has never seen must not be allowed to drive remediation on an
		// arbitrary target. Drop it (loudly) rather than opening an incident.
		if !s.alertTargetKnown(r.Context(), sig.Target.Node) {
			metrics.AuthFailures.WithLabelValues("webhook").Inc()
			slog.Warn("dropping Alertmanager alert for unknown node",
				"node", sig.Target.Node, "remote", remoteSource(r))
			continue
		}
		s.backend.HandleSignal(r, sig)
	}
	w.WriteHeader(http.StatusAccepted)
}

// alertTargetKnown reports whether the node an alert targets exists in the
// controller's inventory. When no inventory source is wired (the operator API
// is disabled, a development-only configuration) it cannot verify and returns
// true rather than dropping every alert; production wires the operator backend,
// where an unknown or spoofed node label is dropped.
func (s *Server) alertTargetKnown(ctx context.Context, node string) bool {
	node = strings.TrimSpace(node)
	if node == "" {
		return false
	}
	if s.operator == nil {
		return true
	}
	n, err := s.operator.Node(ctx, node)
	return err == nil && n != nil
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
	// Exactly one fault identity is authoritative per event: a genuine XID, or
	// a neutral fault envelope. An event carrying both is ambiguous — the
	// classifier is Fault-first, so a nonzero XID beside a Fault would be
	// silently ignored — and no shipped source produces it, so it is rejected
	// here rather than being interpreted either way.
	if ev.XID != 0 && ev.Fault != nil {
		// The marker header tells the agent this is a semantic verdict on the
		// payload — poison, never retryable — as opposed to a schema-skew or
		// middlebox 400, which must stay retryable.
		w.Header().Set(types.AgentEventRejectedHeader, "conflicting-identity")
		http.Error(w, "event claims both an XID and a neutral fault; exactly one identity may be set", http.StatusBadRequest)
		return
	}
	if err := s.backend.HandleAgentEvent(r, ev); err != nil {
		http.Error(w, "event persistence unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleRegister ingests v1 agent self-registrations. The v1 wire must NOT
// carry destructive_armed: the field is known to the Go struct (shared with
// v2), so strict decoding alone would silently accept it here, quietly
// eroding v1's mixed-version corruption guard. Reject it explicitly.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	s.handleRegisterVersion(w, r, false)
}

// handleRegisterV2 ingests v2 registrations, whose contract is "arming is
// always declared": destructive_armed is required.
func (s *Server) handleRegisterV2(w http.ResponseWriter, r *http.Request) {
	s.handleRegisterVersion(w, r, true)
}

func (s *Server) handleRegisterVersion(w http.ResponseWriter, r *http.Request, v2 bool) {
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
	if v2 && registration.DestructiveArmed == nil {
		http.Error(w, "bad registration: the v2 protocol requires destructive_armed", http.StatusBadRequest)
		return
	}
	if !v2 && registration.DestructiveArmed != nil {
		http.Error(w, "bad registration: destructive_armed is a v2 field; register via "+types.AgentRegistrationV2Path, http.StatusBadRequest)
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
	arming, err := s.backend.RegisterNode(r, registration)
	if err != nil {
		http.Error(w, "registration persistence unavailable", http.StatusServiceUnavailable)
		return
	}
	// The v2 response carries the controller-served arming answer; the agent
	// adopts it each tick. v1 responses stay bare (old agents ignore headers
	// anyway, but the v1 contract is frozen).
	if v2 && arming != types.AgentArmingUnknown {
		w.Header().Set(types.AgentArmingHeader, string(arming))
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
		// Only a genuine authorization/definitive rejection is a 403: a foreign
		// node, an unknown action, a stale/incorrect lease, or a boot mismatch —
		// none of which the agent should retry. Every other error is a store or
		// transient failure and must be signalled honestly as a 5xx so the agent
		// keeps the action in its persisted lease and retries, rather than
		// discarding a real result because the database was briefly unreachable.
		switch {
		case errors.Is(err, store.ErrActionForeignNode),
			errors.Is(err, store.ErrNotFound),
			errors.Is(err, store.ErrLeaseLost),
			errors.Is(err, store.ErrExecutorBootMismatch):
			http.Error(w, "action result rejected", http.StatusForbidden)
		default:
			http.Error(w, "action result could not be recorded", http.StatusServiceUnavailable)
		}
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
