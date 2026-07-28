package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
)

// Browser sessions for the embedded panel. Humans sign in with a username
// and password (bcrypt hashes from a Kubernetes Secret) or through OIDC;
// automation keeps using bearer credentials. Sessions live in the leader's
// memory: a controller restart or failover signs everyone out, which is
// deliberately fail-closed and cheap to recover from.

const (
	sessionCookie   = "kn_session"
	sessionTTL      = 12 * time.Hour
	sessionIDBytes  = 32
	maxLoginBytes   = 4 << 10
	bcryptMaxSecret = 72 // bcrypt truncates beyond 72 bytes; reject instead
)

type session struct {
	actor   string
	method  string
	expires time.Time
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]session
	now      func() time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]session), now: time.Now}
}

func (st *sessionStore) create(actor, method string) (string, error) {
	raw := make([]byte, sessionIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	st.mu.Lock()
	defer st.mu.Unlock()
	for key, existing := range st.sessions { // opportunistic expiry sweep
		if st.now().After(existing.expires) {
			delete(st.sessions, key)
		}
	}
	st.sessions[id] = session{actor: actor, method: method, expires: st.now().Add(sessionTTL)}
	return id, nil
}

func (st *sessionStore) lookup(id string) (session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	got, ok := st.sessions[id]
	if !ok || st.now().After(got.expires) {
		delete(st.sessions, id)
		return session{}, false
	}
	return got, true
}

func (st *sessionStore) revoke(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.sessions, id)
}

// SetBasicUsersDir enables username/password sign-in. Every file in dir is
// one user: the filename is the username, the content a bcrypt hash —
// exactly how the operator mounts the referenced Kubernetes Secret. Files
// are read per attempt, so rotating the Secret needs no restart.
func (s *Server) SetBasicUsersDir(dir string) { s.basicUsersDir = dir }

// BasicAuthEnabled reports whether password sign-in is configured.
func (s *Server) BasicAuthEnabled() bool { return s.basicUsersDir != "" }

func (s *Server) verifyBasicUser(username, password string) bool {
	if s.basicUsersDir == "" || username == "" || password == "" || len(password) > bcryptMaxSecret {
		return false
	}
	// The username becomes a filename: refuse anything path-like outright.
	if strings.ContainsAny(username, "/\\") || strings.HasPrefix(username, ".") {
		return false
	}
	hash, err := os.ReadFile(filepath.Join(s.basicUsersDir, username))
	if err != nil {
		// Equalize timing between unknown users and wrong passwords.
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$0123456789012345678901uGZwLBIV1i6Sw2Qh6ePGXH7Vc7XG8om"), []byte(password))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(strings.TrimSpace(string(hash))), []byte(password)) == nil
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, actor, method string) error {
	id, err := s.sessions.create(actor, method)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil, MaxAge: int(sessionTTL.Seconds()),
	})
	return nil
}

// sessionIdentity resolves the request's session cookie, if any.
func (s *Server) sessionIdentity(r *http.Request) (OperatorIdentity, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return OperatorIdentity{}, false
	}
	got, ok := s.sessions.lookup(cookie.Value)
	if !ok {
		return OperatorIdentity{}, false
	}
	return OperatorIdentity{Actor: got.actor, Method: got.method}, true
}

// handleLogin signs a human in with username/password from the users Secret.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.operator == nil || s.operatorToken == "" {
		http.Error(w, "operator API disabled: no API token configured", http.StatusServiceUnavailable)
		return
	}
	if !s.BasicAuthEnabled() {
		http.Error(w, "password sign-in is not configured", http.StatusNotFound)
		return
	}
	source := remoteSource(r)
	if s.authLimiter.blocked(source) {
		http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.verifyBasicUser(strings.TrimSpace(req.Username), req.Password) {
		s.authLimiter.record(source)
		metrics.AuthFailures.WithLabelValues("operator").Inc()
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}
	if err := s.issueSession(w, r, "user:"+strings.TrimSpace(req.Username), "basic"); err != nil {
		http.Error(w, "session unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"actor": "user:" + strings.TrimSpace(req.Username)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.revoke(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	w.WriteHeader(http.StatusNoContent)
}

// handleSession tells the panel who is signed in and which sign-in methods
// exist. It intentionally works unauthenticated so the login page can
// render the right controls.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"authenticated": false,
		"basic":         s.BasicAuthEnabled(),
		"sso":           s.oidc != nil,
	}
	if identity, ok := s.sessionIdentity(r); ok {
		out["authenticated"] = true
		out["actor"] = identity.Actor
		out["method"] = identity.Method
	}
	writeJSON(w, out)
}
