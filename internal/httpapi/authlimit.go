package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// authFailureLimit failed attempts per source address per window trip
	// the limiter. Generous for a fat-fingered human or a misconfigured
	// client, tight enough to make online token guessing useless.
	authFailureLimit  = 20
	authFailureWindow = time.Minute
	// authLimiterMaxSources bounds limiter memory under source-address
	// churn (spoofed connections); a sweep drops expired windows first.
	authLimiterMaxSources = 4096
)

// failureLimiter is a fixed-window per-source failed-authentication
// limiter. Only *failures* count: a legitimate client with the right
// credential is never throttled.
type failureLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	windows map[string]*failureWindow
}

type failureWindow struct {
	start    time.Time
	failures int
}

func newFailureLimiter() *failureLimiter {
	return &failureLimiter{now: time.Now, windows: make(map[string]*failureWindow)}
}

// blocked reports whether the source has exhausted its failure budget for
// the current window.
func (l *failureLimiter) blocked(source string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.windows[source]
	if !ok || l.now().Sub(w.start) >= authFailureWindow {
		return false
	}
	return w.failures >= authFailureLimit
}

// record counts one authentication failure for the source.
func (l *failureLimiter) record(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.windows) >= authLimiterMaxSources {
		l.sweepLocked(now)
	}
	w, ok := l.windows[source]
	if !ok || now.Sub(w.start) >= authFailureWindow {
		l.windows[source] = &failureWindow{start: now, failures: 1}
		return
	}
	w.failures++
}

// sweepLocked drops expired windows; if everything is current, it drops the
// whole map — losing counters is strictly safer than unbounded growth, and
// a real brute-force refills its window within a few requests.
func (l *failureLimiter) sweepLocked(now time.Time) {
	for source, w := range l.windows {
		if now.Sub(w.start) >= authFailureWindow {
			delete(l.windows, source)
		}
	}
	if len(l.windows) >= authLimiterMaxSources {
		l.windows = make(map[string]*failureWindow)
	}
}

// remoteSource is the throttling key: the connection's source IP.
// RemoteAddr, not X-Forwarded-For — a forwarded header is caller-controlled
// and would let an attacker rotate keys for free.
func remoteSource(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
