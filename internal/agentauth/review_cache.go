// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"time"

	"k8s.io/utils/lru"
)

const positiveReviewTTL = time.Minute

// Only the binding verified by TokenReview is reusable. A principal is
// constructed again after checking the current Kubernetes objects.
type reviewedBinding struct {
	serviceAccountUID, podName, podUID string
}

type reviewKey struct {
	tokenDigest [sha256.Size]byte
	audience    string
}

type cachedReview struct {
	binding reviewedBinding
	until   time.Time
}

type positiveReviews struct{ entries *lru.Cache }

func (c positiveReviews) get(key reviewKey, now time.Time) (reviewedBinding, bool) {
	value, ok := c.entries.Get(key)
	if ok {
		entry := value.(cachedReview)
		if now.Before(entry.until) {
			return entry.binding, true
		}
	}
	return reviewedBinding{}, false
}

func (c positiveReviews) remove(key reviewKey) { c.entries.Remove(key) }

// admit is called only after the API authenticated these exact token bytes.
// JWT claims can shorten the cache lifetime but cannot establish identity.
func (c positiveReviews) admit(key reviewKey, token string, binding reviewedBinding, started, now time.Time) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	t, err := decoder.Token()
	if err != nil || t != json.Delim('{') {
		return
	}
	var exp int64
	seen := false
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil {
			return
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return
		}
		if field == "exp" {
			if seen || json.Unmarshal(value, &exp) != nil {
				return
			}
			seen = true
		}
	}
	if _, err = decoder.Token(); err != nil {
		return
	}
	if _, err = decoder.Token(); err != io.EOF || !seen {
		return
	}
	until := time.Unix(exp, 0).Add(-time.Second)
	if limit := started.Add(positiveReviewTTL); limit.Before(until) {
		until = limit
	}
	if now.Before(until) {
		c.entries.Add(key, cachedReview{binding: binding, until: until})
	}
}
