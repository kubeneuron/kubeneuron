package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	// negativeAuthTTL is how long a credential proven bad is remembered so a
	// blocked source replaying it is rejected without a fresh, expensive
	// verification (a TokenReview round-trip). Short on purpose: a credential
	// that legitimately gains access self-heals within this window.
	negativeAuthTTL = 30 * time.Second
	// negativeAuthMaxEntries bounds the negative cache under credential churn.
	negativeAuthMaxEntries = 4096
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

// negativeAuthCache remembers credentials that failed verification recently,
// keyed by a salted hash so the cache never stores a usable credential. Its
// sole purpose is back-pressure: a source that has already exhausted its
// failure budget and keeps replaying the SAME bad credential is rejected
// without paying for another apiserver TokenReview, while a *different*
// credential (a valid principal behind the same shared NAT egress IP) is never
// in the cache and so is always verified normally.
type negativeAuthCache struct {
	mu      sync.Mutex
	now     func() time.Time
	salt    [16]byte
	entries map[string]time.Time // credential hash -> expiry
}

func newNegativeAuthCache() *negativeAuthCache {
	c := &negativeAuthCache{now: time.Now, entries: make(map[string]time.Time)}
	// A per-process random salt makes the stored hashes useless outside this
	// process and unguessable without the raw credential.
	_, _ = rand.Read(c.salt[:])
	return c
}

// hash derives the cache key for a raw credential. An empty credential still
// hashes to a stable key, so a blocked source spamming the empty Authorization
// header is throttled too.
func (c *negativeAuthCache) hash(credential string) string {
	h := sha256.New()
	_, _ = h.Write(c.salt[:])
	_, _ = h.Write([]byte(credential))
	return hex.EncodeToString(h.Sum(nil))
}

// seen reports whether this credential hash was remembered as bad and has not
// yet expired.
func (c *negativeAuthCache) seen(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	expiry, ok := c.entries[key]
	if !ok {
		return false
	}
	if !c.now().Before(expiry) {
		delete(c.entries, key)
		return false
	}
	return true
}

// remember records a credential hash as bad for negativeAuthTTL.
func (c *negativeAuthCache) remember(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= negativeAuthMaxEntries {
		for k, expiry := range c.entries {
			if !now.Before(expiry) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= negativeAuthMaxEntries {
			c.entries = make(map[string]time.Time)
		}
	}
	c.entries[key] = now.Add(negativeAuthTTL)
}

// bearerCredential extracts the raw bearer credential from a request for
// negative-cache keying. It returns the whole Authorization header when it is
// not a bearer token so any repeated bad credential is still keyed stably.
func bearerCredential(r *http.Request) string {
	return r.Header.Get("Authorization")
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
