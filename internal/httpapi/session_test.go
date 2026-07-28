package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func basicAuthServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret-pass"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alice"), hash, 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(&registrationBackend{})
	s.EnableOperatorAPI(&fakeOperator{}, "static-token")
	s.SetBasicUsersDir(dir)
	return s, s.Routes(), dir
}

func login(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	return rec
}

func TestPasswordLoginIssuesAuditedSession(t *testing.T) {
	_, handler, _ := basicAuthServer(t)

	rec := login(t, handler, `{"username":"alice","password":"s3cret-pass"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" || !cookie.HttpOnly {
		t.Fatalf("session cookie = %+v, want HttpOnly value", cookie)
	}

	// The session authenticates API calls, and mutations are audited under
	// the verified user identity — the body actor is ignored.
	srv, handler2, _ := basicAuthServer(t)
	opRef := &fakeOperator{}
	srv.operator = opRef
	rec = login(t, handler2, `{"username":"alice","password":"s3cret-pass"}`)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	approve := httptest.NewRequest("POST", "/api/v1/incidents/inc-1/approve", strings.NewReader(`{"actor":"mallory"}`))
	approve.Header.Set("Content-Type", "application/json")
	approve.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler2.ServeHTTP(rec, approve)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("session approve = %d %s", rec.Code, rec.Body.String())
	}
	if len(opRef.decisions) != 1 || opRef.decisions[0] != "inc-1:approved:user:alice" {
		t.Fatalf("decisions = %v, want the verified user:alice actor", opRef.decisions)
	}

	// Logout revokes the session.
	out := httptest.NewRequest("POST", "/api/v1/logout", nil)
	out.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler2.ServeHTTP(rec, out)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", rec.Code)
	}
	after := httptest.NewRequest("GET", "/api/v1/incidents", nil)
	after.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler2.ServeHTTP(rec, after)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session = %d, want 401", rec.Code)
	}
}

func TestPasswordLoginRejectsAndThrottles(t *testing.T) {
	_, handler, _ := basicAuthServer(t)
	for _, body := range []string{
		`{"username":"alice","password":"wrong"}`,
		`{"username":"bob","password":"s3cret-pass"}`,
		`{"username":"../../etc/passwd","password":"x"}`,
		`{"username":".hidden","password":"x"}`,
	} {
		if rec := login(t, handler, body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("login %s = %d, want 401", body, rec.Code)
		}
	}
	// Failed logins burn the same per-source budget as bad tokens.
	for i := 0; i < authFailureLimit; i++ {
		login(t, handler, `{"username":"alice","password":"wrong"}`)
	}
	if rec := login(t, handler, `{"username":"alice","password":"wrong"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("post-budget login = %d, want 429", rec.Code)
	}
}

func TestSessionEndpointAdvertisesMethods(t *testing.T) {
	s := New(&registrationBackend{})
	s.EnableOperatorAPI(&fakeOperator{}, "static-token")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/session", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"authenticated":false`) ||
		!strings.Contains(body, `"basic":false`) || !strings.Contains(body, `"sso":false`) {
		t.Fatalf("bare session = %d %s", rec.Code, body)
	}

	srv, handler, _ := basicAuthServer(t)
	_ = srv
	rec = login(t, handler, `{"username":"alice","password":"s3cret-pass"}`)
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	req := httptest.NewRequest("GET", "/api/v1/session", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body = rec.Body.String()
	if !strings.Contains(body, `"authenticated":true`) || !strings.Contains(body, `"actor":"user:alice"`) ||
		!strings.Contains(body, `"method":"basic"`) || !strings.Contains(body, `"basic":true`) {
		t.Fatalf("authenticated session = %s", body)
	}
}

func TestOIDCEndpointsFailClosedWhenUnconfigured(t *testing.T) {
	s := New(&registrationBackend{})
	s.EnableOperatorAPI(&fakeOperator{}, "static-token")
	for _, path := range []string{"/api/v1/auth/oidc/login", "/api/v1/auth/oidc/callback"} {
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404 when SSO is not configured", path, rec.Code)
		}
	}
}
