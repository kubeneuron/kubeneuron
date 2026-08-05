package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- Fix 13: the OIDC domain allowlist must not trust an unverified email ---

func TestOIDCAuthorizeRequiresVerifiedEmail(t *testing.T) {
	gated := OIDCConfig{AllowedEmailDomains: []string{"allowed-corp.com"}}

	// The core defect: an unverified email in the allow-listed domain must be
	// refused, not handed a full operator session.
	if _, err := gated.authorize("x@allowed-corp.com", false, "sub-1"); err == nil {
		t.Fatal("an unverified email must be refused even when its domain is allow-listed")
	}
	// A verified email in the domain is accepted and becomes the audited actor.
	if actor, err := gated.authorize("x@allowed-corp.com", true, "sub-1"); err != nil || actor != "x@allowed-corp.com" {
		t.Fatalf("verified allow-listed email must pass; actor=%q err=%v", actor, err)
	}
	// A verified email outside the allowlist is still refused.
	if _, err := gated.authorize("x@evil.com", true, "sub-1"); err == nil {
		t.Fatal("a verified email outside the allowlist must be refused")
	}
	// An allowlist with no email at all cannot be satisfied.
	if _, err := gated.authorize("", true, "sub-1"); err == nil {
		t.Fatal("an allowlist with no email must be refused")
	}

	// With no allowlist the subject is the fallback actor, but a *present* email
	// must still be verified (it would otherwise become the audited actor).
	open := OIDCConfig{}
	if actor, err := open.authorize("", false, "sub-2"); err != nil || actor != "sub-2" {
		t.Fatalf("subject fallback must work with no email; actor=%q err=%v", actor, err)
	}
	if _, err := open.authorize("x@any.com", false, "sub-2"); err == nil {
		t.Fatal("an unverified email must be refused even without an allowlist — it would be the actor")
	}
}

// --- Fix 14: the auth limiter must not lock out a valid operator behind NAT ---

// fakeSARAuth authenticates exactly one bearer token (a stand-in for a valid
// SubjectAccessReview) and fails everything else, defaulting to 401.
type fakeSARAuth struct{ good string }

func (a fakeSARAuth) AuthenticateOperator(r *http.Request, _ string) (OperatorIdentity, error) {
	if r.Header.Get("Authorization") == "Bearer "+a.good {
		return OperatorIdentity{Actor: "system:serviceaccount:ops:sre-bot", Method: "kubernetes"}, nil
	}
	return OperatorIdentity{}, errors.New("denied")
}

func TestAuthLimiterDoesNotLockOutValidOperatorBehindSharedIP(t *testing.T) {
	s := New(&registrationBackend{})
	s.EnableOperatorAPI(&fakeOperator{}, "static-secret")
	s.SetOperatorAuthenticator(fakeSARAuth{good: "valid-sar-token"})
	handler := s.Routes()

	// One source IP (httptest gives every request the same RemoteAddr) hammers
	// with bad tokens until it exhausts the failure budget.
	for i := 0; i < authFailureLimit+5; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents", "bad-token", ""))
	}
	// A further bad-token request from that source is now throttled.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents", "another-bad-token", ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a brute-forcing source must be throttled; status = %d, want 429", rec.Code)
	}

	// A VALID SAR operator sharing that egress IP must still authenticate — the
	// throttle applies to failures, not to a known principal presenting a good
	// credential.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents", "valid-sar-token", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("a valid operator behind a shared NAT/LB IP must not be locked out; status = %d, want 200", rec.Code)
	}
}

// --- Fix H-F1: a blocked source replaying a bad credential must not force a
// fresh (expensive) TokenReview each time; a valid principal still passes. ---

type countingSARAuth struct {
	calls int
	good  string
}

func (a *countingSARAuth) AuthenticateOperator(r *http.Request, _ string) (OperatorIdentity, error) {
	a.calls++
	if r.Header.Get("Authorization") == "Bearer "+a.good {
		return OperatorIdentity{Actor: "system:serviceaccount:ops:sre-bot", Method: "kubernetes"}, nil
	}
	return OperatorIdentity{}, errors.New("denied")
}

func TestBlockedSourceReplayingBadCredentialSkipsTokenReview(t *testing.T) {
	auth := &countingSARAuth{good: "valid"}
	s := New(&registrationBackend{})
	s.EnableOperatorAPI(&fakeOperator{}, "static-secret")
	s.SetOperatorAuthenticator(auth)
	handler := s.Routes()

	do := func(token string) int {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents", token, ""))
		return rec.Code
	}

	// Exhaust the per-source failure budget with the SAME bad credential; each
	// failure legitimately costs one TokenReview to reach the block.
	for i := 0; i < authFailureLimit; i++ {
		do("same-bad")
	}
	blockedCalls := auth.calls
	if blockedCalls != authFailureLimit {
		t.Fatalf("reaching the block cost %d TokenReviews, want %d", blockedCalls, authFailureLimit)
	}

	// Now blocked. Replaying the same bad credential must be rejected WITHOUT any
	// further TokenReview — otherwise the limiter is cosmetic.
	for i := 0; i < 10; i++ {
		if code := do("same-bad"); code != http.StatusTooManyRequests {
			t.Fatalf("blocked replay status = %d, want 429", code)
		}
	}
	if auth.calls != blockedCalls {
		t.Fatalf("blocked replay triggered %d extra TokenReviews, want 0", auth.calls-blockedCalls)
	}

	// A valid principal from the SAME blocked source is a different credential,
	// not in the negative cache, so it is still verified and passes.
	if code := do("valid"); code != http.StatusOK {
		t.Fatalf("valid principal from a blocked source = %d, want 200", code)
	}
	if auth.calls != blockedCalls+1 {
		t.Fatalf("valid principal should cost exactly one TokenReview; delta = %d", auth.calls-blockedCalls)
	}
}
