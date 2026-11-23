// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// statusServer answers status with one runtime-config body.
func statusServer(t *testing.T, runtimeConfig string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/incidents":
			_, _ = w.Write([]byte("[]"))
		case "/api/v1/pause":
			_ = json.NewEncoder(w).Encode(map[string]bool{"paused": false})
		case "/api/v1/runtime-config":
			if runtimeConfig == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(runtimeConfig))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestStatusNamesAConfigurationLeftUnloaded(t *testing.T) {
	published := func(digest string, ago time.Duration) string {
		at := time.Now().Add(-ago).UTC().Format(time.RFC3339)
		return `{"source_digest":"loaded","published":{"config_digest":"` + digest + `","published_at":"` + at + `"}}`
	}
	for name, tc := range map[string]struct {
		body string
		want bool
	}{
		"unloaded past the threshold":   {published("newer", 7*time.Minute), true},
		"unloaded within the threshold": {published("newer", 2*time.Minute), false},
		"loaded":                        {published("loaded", 7*time.Minute), false},
		"unreadable":                    {`{"source_digest":"loaded","published":{"error":"forbidden"}}`, false},
		"no operator":                   {`{"source_digest":"loaded"}`, false},
		"older controller":              {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := runCommand(t, statusServer(t, tc.body).URL, "status")
			if err != nil {
				t.Fatal(err)
			}
			line := "configuration published 7m"
			if got := strings.Contains(out, "is not loaded by the controller"); got != tc.want || (tc.want && !strings.Contains(out, line)) {
				t.Fatalf("status output %q, want the unloaded-configuration line: %v", out, tc.want)
			}
		})
	}
}
