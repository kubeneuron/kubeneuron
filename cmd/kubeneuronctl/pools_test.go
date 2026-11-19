// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestPoolsCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/pools" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"pools":[
 {"pool":"train","cloud":"GKE","accelerator_nodes":2,"replacement_policy":"AddFirst","status":"available","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"full","cloud":"AKS","accelerator_nodes":3,"replacement_policy":"mixed","status":"unavailable","reason":"no room below maxSize","action":"provide room for one more node, or set RemoveFirst in the remediation policy that applies to these nodes","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"denied","cloud":"AKS","accelerator_nodes":1,"replacement_policy":"AddFirst","status":"unknown","reason":"permission denied while reading pool configuration","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"pending","cloud":"GKE","accelerator_nodes":1,"replacement_policy":"AddFirst","status":"unknown"},
 {"pool":"disabled","cloud":"EKS","accelerator_nodes":4,"replacement_policy":"AddFirst","status":"unavailable","reason":"add-first on EKS is not enabled in this release","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"fixed","cloud":"EKS","accelerator_nodes":2,"replacement_policy":"RemoveFirst"}]}`)
	}))
	defer server.Close()
	got, err := runCommand(t, server.URL, "pools")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/pools.golden")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("unexpected pools output:\n%s", got)
	}
}
