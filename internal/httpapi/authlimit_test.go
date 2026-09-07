package httpapi

import (
	"testing"
	"time"
)

func TestOperationRateLimiterBoundsAndExpiresWindows(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	limiter := newOperationRateLimiter()
	limiter.now = func() time.Time { return now }
	for attempt := 0; attempt < 2; attempt++ {
		allowed, retry := limiter.allow("source\x00preview", 2)
		if !allowed || retry != 0 {
			t.Fatalf("attempt %d allowed=%v retry=%s", attempt, allowed, retry)
		}
	}
	allowed, retry := limiter.allow("source\x00preview", 2)
	if allowed || retry <= 0 || retry > operationalMutationWindow {
		t.Fatalf("over limit allowed=%v retry=%s", allowed, retry)
	}
	// A distinct expensive operation has its own quota, so a cautious health
	// check cannot be starved by a burst of preview requests.
	if allowed, _ := limiter.allow("source\x00health-check", 1); !allowed {
		t.Fatal("per-operation quota unexpectedly shared")
	}
	now = now.Add(operationalMutationWindow)
	if allowed, _ := limiter.allow("source\x00preview", 2); !allowed {
		t.Fatal("expired quota window did not reset")
	}
}
