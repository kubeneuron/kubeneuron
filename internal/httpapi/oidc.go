package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC single sign-on for the panel: any spec-compliant provider (Okta,
// Keycloak, Dex, Entra, Google, …). The controller runs the authorization
// code flow; the verified identity (email when present, else subject)
// becomes the audited actor. Bearer credentials for CLI/automation are
// untouched.

const (
	oidcStateCookie = "kn_oidc_state"
	oidcStateTTL    = 10 * time.Minute
)

// OIDCConfig configures single sign-on.
type OIDCConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	// RedirectURL must be the externally reachable
	// https://<panel>/api/v1/auth/oidc/callback.
	RedirectURL string
	// AllowedEmailDomains optionally restricts who may sign in (checked
	// against the verified email claim). Empty allows any authenticated
	// principal of the issuer.
	AllowedEmailDomains []string
}

type oidcAuthenticator struct {
	cfg      OIDCConfig
	verifier *gooidc.IDTokenVerifier
	oauth    oauth2.Config
}

// EnableOIDC wires the provider (issuer discovery happens here, so a dead
// issuer fails loudly at startup instead of at first login).
func (s *Server) EnableOIDC(ctx context.Context, cfg OIDCConfig) error {
	provider, err := gooidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return fmt.Errorf("oidc: discover issuer %s: %w", cfg.IssuerURL, err)
	}
	s.oidc = &oidcAuthenticator{
		cfg:      cfg,
		verifier: provider.Verifier(&gooidc.Config{ClientID: cfg.ClientID}),
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       []string{gooidc.ScopeOpenID, "profile", "email"},
		},
	}
	return nil
}

// handleOIDCLogin starts the authorization code flow.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.Error(w, "single sign-on is not configured", http.StatusNotFound)
		return
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "state unavailable", http.StatusInternalServerError)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: state, Path: "/api/v1/auth/oidc",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil, MaxAge: int(oidcStateTTL.Seconds()),
	})
	http.Redirect(w, r, s.oidc.oauth.AuthCodeURL(state), http.StatusFound)
}

// handleOIDCCallback finishes the flow and issues the panel session.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.Error(w, "single sign-on is not configured", http.StatusNotFound)
		return
	}
	stateCookie, err := r.Cookie(oidcStateCookie)
	if err != nil || stateCookie.Value == "" || r.URL.Query().Get("state") != stateCookie.Value {
		http.Error(w, "state mismatch; restart sign-in", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: "", Path: "/api/v1/auth/oidc", MaxAge: -1, HttpOnly: true})

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	token, err := s.oidc.oauth.Exchange(ctx, r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "provider returned no id_token", http.StatusBadGateway)
		return
	}
	idToken, err := s.oidc.verifier.Verify(ctx, rawID)
	if err != nil {
		http.Error(w, "id_token verification failed", http.StatusUnauthorized)
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	_ = idToken.Claims(&claims)

	actor := claims.Email
	if actor == "" {
		actor = idToken.Subject
	}
	if len(s.oidc.cfg.AllowedEmailDomains) > 0 {
		domain := ""
		if at := strings.LastIndexByte(claims.Email, '@'); at > 0 {
			domain = strings.ToLower(claims.Email[at+1:])
		}
		allowed := false
		for _, want := range s.oidc.cfg.AllowedEmailDomains {
			if domain == strings.ToLower(want) {
				allowed = true
				break
			}
		}
		if !allowed {
			http.Error(w, "your account's email domain is not allowed here", http.StatusForbidden)
			return
		}
	}
	if err := s.issueSession(w, r, actor, "oidc"); err != nil {
		http.Error(w, "session unavailable", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}
