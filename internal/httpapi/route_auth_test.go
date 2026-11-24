// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func (r *routeRecorder) Handle(pattern string, handler http.Handler) {
	r.routes = append(r.routes, recordedRoute{pattern, handler.ServeHTTP})
}

// unauthenticatedRoutes lists every public route that answers without
// credentials, and why. A new route lands in the test below unless it is
// added here on purpose.
var unauthenticatedRoutes = map[string]string{
	"GET /healthz":                   "liveness probe",
	"GET /readyz":                    "readiness probe; it shows only the loaded configuration digest, which is public in the KubeNeuron status",
	"GET /metrics":                   "controller self-metrics for the scraper",
	"GET /{$}":                       "the panel's static page; every API call it makes carries credentials",
	"GET /ui/":                       "the panel's static assets",
	"POST /api/v1/login":             "authenticates with the password in the body and issues the session",
	"POST /api/v1/logout":            "ends the caller's own session, if there is one",
	"GET /api/v1/session":            "says whether the caller has a session",
	"GET /api/v1/auth/oidc/login":    "starts an OIDC login",
	"GET /api/v1/auth/oidc/callback": "completes an OIDC login; the state and code authenticate it",
}

type nopBackupStore struct{}

func (nopBackupStore) BackupTo(context.Context, string) error { return errors.New("not in this test") }

// publicRoutes enumerates every route Routes serves, from the same
// registration, with every optional route switched on.
func publicRoutes(t *testing.T) []recordedRoute {
	t.Helper()
	s, _, _ := basicAuthServer(t)
	s.SetMetricsHandler(http.NotFoundHandler())
	s.ui = http.NotFoundHandler()
	s.SetBackupStore(nopBackupStore{}, t.TempDir())
	rec := &routeRecorder{}
	s.registerPublicRoutes(rec)
	if len(rec.routes) < 60 {
		t.Fatalf("found %d public routes, want the full set", len(rec.routes))
	}
	return rec.routes
}

// Every public route refuses a caller without credentials, except the routes
// listed above. A route registered without authentication fails here by name.
func TestEveryPublicRouteRequiresAuthentication(t *testing.T) {
	registered := map[string]bool{}
	for i, route := range publicRoutes(t) {
		registered[route.pattern] = true
		if _, exempt := unauthenticatedRoutes[route.pattern]; exempt {
			continue
		}
		method, path, _ := strings.Cut(route.pattern, " ")
		req := httptest.NewRequest(method, concretePath(path), strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		// One source per request, so the failed-attempt limiter answers 429
		// for none of them.
		req.RemoteAddr = fmt.Sprintf("10.%d.%d.1:40000", i/250, i%250)
		rec := httptest.NewRecorder()
		route.handler(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials: status %d, want 401 (%s); add it to unauthenticatedRoutes only if it is meant to be public",
				route.pattern, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
	for pattern := range unauthenticatedRoutes {
		if !registered[pattern] {
			t.Errorf("unauthenticatedRoutes lists %s, which is not registered; remove it", pattern)
		}
	}
}

type rejectingAuthenticator struct{}

func (rejectingAuthenticator) AuthenticateAgent(*http.Request) (AgentPrincipal, error) {
	return AgentPrincipal{}, errors.New("no credentials")
}

// The agent listener has no exemptions: every route sits behind agent
// authentication.
func TestEveryAgentRouteRequiresAuthentication(t *testing.T) {
	s := New(&registrationBackend{})
	rec := &routeRecorder{}
	s.registerAgentRoutes(rec)
	if len(rec.routes) < 10 {
		t.Fatalf("found %d agent routes, want the full set", len(rec.routes))
	}
	handler := s.AgentRoutes(rejectingAuthenticator{})
	for i, route := range rec.routes {
		method, path, _ := strings.Cut(route.pattern, " ")
		req := httptest.NewRequest(method, concretePath(path), strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = fmt.Sprintf("10.%d.%d.1:40000", i/250, i%250)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without agent credentials: status %d, want 401 (%s)", route.pattern, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
}
