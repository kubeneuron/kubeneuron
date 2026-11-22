// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// A session cookie is sent by the browser with every request to the panel's
// host, including requests a page on another origin makes to it. SameSite=Lax
// keeps the cookie off cross-SITE subresource requests, but not off requests
// from a sibling origin of the same site (another subdomain), and it does
// nothing about form or text/plain bodies that need no CORS preflight. So a
// state-changing request authenticated by the cookie alone must prove it came
// from the panel's own origin. Bearer-token callers (the CLI, service
// accounts) carry no ambient credential and are not checked here.

// sessionMutationMediaTypes are the request body media types a cookie-
// authenticated mutation may carry. None of them is a CORS-safelisted type,
// so a cross-origin page cannot send one without a preflight this server
// never answers. The YAML types are the candidate upload formats
// (handleCreateCandidate).
var sessionMutationMediaTypes = map[string]bool{
	"application/json":   true,
	"application/yaml":   true,
	"application/x-yaml": true,
	"text/yaml":          true,
}

// checkSessionMutation reports why a cookie-authenticated state-changing
// request must be refused, with the HTTP status to answer, or 0 if it may
// proceed.
func (s *Server) checkSessionMutation(r *http.Request) (int, string) {
	if reason := s.crossOriginReason(r); reason != "" {
		return http.StatusForbidden, "cross-origin request refused: " + reason
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || !sessionMutationMediaTypes[strings.ToLower(mediaType)] {
			return http.StatusUnsupportedMediaType, "request body must be application/json"
		}
	} else if r.ContentLength != 0 {
		// A body of unknown or non-zero length with no declared type.
		return http.StatusUnsupportedMediaType, "request body must be application/json"
	}
	return 0, ""
}

// crossOriginReason returns "" when the request provably comes from the
// panel's own origin. It fails closed: a request that carries none of
// Sec-Fetch-Site, Origin, or Referer is refused, because every browser that
// can hold the session cookie sends at least one of them on a POST or DELETE.
func (s *Server) crossOriginReason(r *http.Request) string {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return "Sec-Fetch-Site is " + site
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if !s.sameOrigin(r, origin) {
			return "Origin " + origin + " is not this server"
		}
		return ""
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		if !s.sameOrigin(r, referer) {
			return "Referer is not this server"
		}
		return ""
	}
	if r.Header.Get("Sec-Fetch-Site") == "same-origin" {
		return ""
	}
	return "no Origin, Referer, or Sec-Fetch-Site header"
}

// sameOrigin compares the scheme-qualified origin of raw (an Origin or
// Referer value) with the host the request was addressed to. The scheme is
// not compared: behind a TLS-terminating proxy the controller may see plain
// HTTP for a page served over HTTPS, and one host:port cannot serve two
// origins that differ only by scheme to the same browser session.
func (s *Server) sameOrigin(r *http.Request, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	got := normalizeHost(u.Host, u.Scheme)
	for _, host := range s.requestHosts(r) {
		if host != "" && got == normalizeHost(host, u.Scheme) {
			return true
		}
	}
	return false
}

// requestHosts is the host the client addressed: the Host header, and, when
// proxy headers are trusted, the first X-Forwarded-Host a reverse proxy set.
func (s *Server) requestHosts(r *http.Request) []string {
	hosts := []string{r.Host}
	if s.trustProxyHeaders {
		forwarded := r.Header.Get("X-Forwarded-Host")
		if i := strings.IndexByte(forwarded, ','); i >= 0 {
			forwarded = forwarded[:i]
		}
		if forwarded = strings.TrimSpace(forwarded); forwarded != "" {
			hosts = append(hosts, forwarded)
		}
	}
	return hosts
}

// normalizeHost lowercases host and drops the scheme's default port, so
// "Panel.example:443" and "panel.example" compare equal under https.
func normalizeHost(host, scheme string) string {
	host = strings.ToLower(host)
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		if strings.Contains(h, ":") {
			return "[" + h + "]"
		}
		return h
	}
	return host
}
