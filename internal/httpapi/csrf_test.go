// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recordedRoute struct {
	pattern string
	handler func(http.ResponseWriter, *http.Request)
}

type routeRecorder struct{ routes []recordedRoute }

func (r *routeRecorder) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	r.routes = append(r.routes, recordedRoute{pattern, handler})
}

// sessionExempt lists the state-changing routes that are not authenticated by
// the session cookie and so are not subject to the same-origin check. A new
// mutating route lands in the test below unless it is added here on purpose.
var sessionExempt = map[string]string{
	"POST /api/v1/login": "authenticates with a password in the body, issues the cookie",
}

func sessionCookieFor(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	rec := login(t, handler, `{"username":"alice","password":"s3cret-pass"}`)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	t.Fatalf("login = %d %s: no session cookie", rec.Code, rec.Body.String())
	return nil
}

// concretePath turns a mux pattern into a request path.
func concretePath(path string) string {
	for _, wildcard := range []string{"{id}", "{node}"} {
		path = strings.ReplaceAll(path, wildcard, "x")
	}
	return path
}

func mutatingRoutes(t *testing.T, s *Server) []recordedRoute {
	t.Helper()
	rec := &routeRecorder{}
	s.registerOperatorRoutes(rec)
	var out []recordedRoute
	for _, route := range rec.routes {
		method, _, _ := strings.Cut(route.pattern, " ")
		if method == http.MethodGet || method == http.MethodHead {
			continue
		}
		if _, ok := sessionExempt[route.pattern]; ok {
			continue
		}
		out = append(out, route)
	}
	if len(out) < 20 {
		t.Fatalf("found %d mutating operator routes, want the full set", len(out))
	}
	return out
}

func TestEveryCookieMutationRefusesForeignOrigins(t *testing.T) {
	s, handler, _ := basicAuthServer(t)
	cookie := sessionCookieFor(t, handler)

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"cross-site origin", map[string]string{"Origin": "https://attacker.test", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site sibling origin", map[string]string{"Origin": "https://evil.example.com", "Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"sibling origin without fetch metadata", map[string]string{"Origin": "http://evil.example.com"}, http.StatusForbidden},
		{"null origin", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"foreign referer only", map[string]string{"Referer": "https://evil.example.com/page"}, http.StatusForbidden},
		{"no provenance headers", map[string]string{}, http.StatusForbidden},
		{"same origin, text/plain body", map[string]string{"Origin": "http://example.com", "Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		{"same origin, form body", map[string]string{"Origin": "http://example.com", "Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType},
	}
	for _, route := range mutatingRoutes(t, s) {
		method, path, _ := strings.Cut(route.pattern, " ")
		for _, tc := range cases {
			req := httptest.NewRequest(method, concretePath(path), strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			route.handler(rec, req)
			if rec.Code != tc.want {
				t.Errorf("%s / %s: status %d, want %d (%s)", route.pattern, tc.name, rec.Code, tc.want, strings.TrimSpace(rec.Body.String()))
			}
		}
	}
}

func TestCookieMutationFromOwnOriginIsAccepted(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"origin", map[string]string{"Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"}},
		{"origin with default port", map[string]string{"Origin": "http://EXAMPLE.com:80"}},
		{"referer fallback", map[string]string{"Referer": "http://example.com/ui/"}},
		{"fetch metadata only", map[string]string{"Sec-Fetch-Site": "same-origin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, handler, _ := basicAuthServer(t)
			op := &fakeOperator{}
			srv.operator = op
			cookie := sessionCookieFor(t, handler)
			req := httptest.NewRequest("POST", "/api/v1/incidents/inc-1/approve", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent || len(op.decisions) != 1 {
				t.Fatalf("approve = %d %s, decisions %v", rec.Code, rec.Body.String(), op.decisions)
			}
		})
	}
}

func TestCookieMutationBehindTrustedProxyUsesForwardedHost(t *testing.T) {
	srv, handler, _ := basicAuthServer(t)
	op := &fakeOperator{}
	srv.operator = op
	cookie := sessionCookieFor(t, handler)
	send := func() int {
		req := httptest.NewRequest("POST", "/api/v1/incidents/inc-1/approve", strings.NewReader(`{}`))
		req.Host = "kubeneuron-controller.kube-neuron.svc:8080"
		req.Header.Set("X-Forwarded-Host", "panel.example.com")
		req.Header.Set("Origin", "https://panel.example.com")
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := send(); got != http.StatusForbidden {
		t.Fatalf("untrusted X-Forwarded-Host: status %d, want 403", got)
	}
	srv.TrustProxyHeaders(true)
	if got := send(); got != http.StatusNoContent {
		t.Fatalf("trusted X-Forwarded-Host: status %d, want 204", got)
	}
}

// The CLI and service accounts authenticate with a bearer token and carry no
// ambient credential, so the same-origin rule does not apply to them.
func TestBearerMutationIsNotSubjectToOriginCheck(t *testing.T) {
	srv, handler, _ := basicAuthServer(t)
	op := &fakeOperator{}
	srv.operator = op
	req := httptest.NewRequest("POST", "/api/v1/incidents/inc-1/approve", strings.NewReader(`{"actor":"cli"}`))
	req.Header.Set("Authorization", "Bearer static-token")
	req.Header.Set("Origin", "https://attacker.test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || len(op.decisions) != 1 {
		t.Fatalf("bearer approve = %d %s, decisions %v", rec.Code, rec.Body.String(), op.decisions)
	}
}

func TestCookieReadsAreNotSubjectToOriginCheck(t *testing.T) {
	_, handler, _ := basicAuthServer(t)
	cookie := sessionCookieFor(t, handler)
	req := httptest.NewRequest("GET", "/api/v1/incidents", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("GET with a session = %d, reads must not be refused by the origin check", rec.Code)
	}
}
