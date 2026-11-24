// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"encoding/base64"
	"testing"
	"time"

	"k8s.io/utils/lru"
)

func TestPositiveReviewDeadline(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, tc := range []struct {
		claims   string
		lifetime time.Duration
	}{
		{`{"exp":1030}`, 29 * time.Second}, {`{"exp":9900}`, time.Minute},
		{`{}`, 0}, {`{"exp":9900,"exp":9901}`, 0}, {`{"exp":"9900"}`, 0},
		{`{"exp":1000}`, 0}, {`{"exp":9900}{}`, 0},
	} {
		t.Run(tc.claims, func(t *testing.T) {
			c := positiveReviews{entries: lru.New(1)}
			key := reviewKey{audience: "agent"}
			binding := reviewedBinding{"sa", "pod", "uid"}
			token := "header." + base64.RawURLEncoding.EncodeToString([]byte(tc.claims)) + ".signature"
			c.admit(key, token, binding, start, start)
			if got, ok := c.get(key, start); ok != (tc.lifetime > 0) || (ok && got != binding) {
				t.Fatalf("unexpected admission: %v", ok)
			}
			if _, ok := c.get(key, start.Add(tc.lifetime)); ok {
				t.Fatal("hit at deadline")
			}
		})
	}
}

func TestPositiveReviewCapacity(t *testing.T) {
	c := positiveReviews{entries: lru.New(1)}
	start := time.Unix(1000, 0)
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":2000}`)) + ".sig"
	first := reviewKey{audience: "a"}
	second := reviewKey{audience: "b"}
	c.admit(first, token, reviewedBinding{}, start, start)
	c.admit(second, token, reviewedBinding{}, start, start)
	if _, ok := c.get(first, start); ok {
		t.Fatal("old review survived eviction")
	}
	if _, ok := c.get(second, start); !ok {
		t.Fatal("latest review missing")
	}
}
