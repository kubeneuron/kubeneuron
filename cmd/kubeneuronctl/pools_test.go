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
 {"pool":"train","cloud":"GKE","accelerator_nodes":2,"replacement_policy":"AddFirst","status":"available","replacement_status":"available","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"full","cloud":"AKS","accelerator_nodes":3,"replacement_policy":"mixed","status":"unavailable","replacement_status":"available","reason":"no room below maxSize","action":"provide room for one more node, or set RemoveFirst in the remediation policy that applies to these nodes","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"denied","cloud":"AKS","accelerator_nodes":1,"replacement_policy":"AddFirst","status":"unknown","reason_code":"read-failed","reason":"permission denied while reading pool configuration","replacement_status":"unknown","replacement_reason_code":"read-failed","replacement_reason":"permission denied while reading pool configuration","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"pending","cloud":"GKE","accelerator_nodes":1,"replacement_policy":"AddFirst","status":"unknown"},
 {"pool":"any-zone","successor_zone":"AnyPoolZone","zones":["zone-a","zone-b"],"cloud":"EKS","accelerator_nodes":4,"replacement_policy":"AddFirst","status":"available","replacement_status":"available","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"gpu","cloud":"GKE","accelerator_nodes":3,"replacement_policy":"RemoveFirst","replacement_status":"available","checked_at":"2026-11-18T12:00:00Z","replacement_held":["3 of 4 nodes — replacement of n1 is waiting for remediation to be resumed (incident inc-1)"]},
 {"pool":"fixed","missing_qualified_profiles":["training-t4"],"automatic_qualification_reasons":["profile training-t4 listed in spec.automatic.qualifiedProfiles does not exist"],"cloud":"EKS","accelerator_nodes":2,"replacement_policy":"RemoveFirst","replacement_status":"available","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"partial-list","missing_qualified_profiles":["training-t4"],"cloud":"EKS","accelerator_nodes":2,"replacement_policy":"RemoveFirst","replacement_status":"available","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"no-match","automatic_qualification_reasons":["no AcceleratorRuntimeProfile selects this pool's nodes"],"cloud":"EKS","accelerator_nodes":2,"replacement_policy":"RemoveFirst","replacement_status":"available","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"overlap","automatic_qualification_reasons":["more than one AcceleratorRuntimeProfile selects this pool's nodes (a and b)"],"cloud":"EKS","accelerator_nodes":2,"replacement_policy":"RemoveFirst","replacement_status":"available","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"unlisted","automatic_qualification_reasons":["node profile other is not listed in spec.automatic.qualifiedProfiles"],"cloud":"EKS","accelerator_nodes":2,"replacement_policy":"RemoveFirst","replacement_status":"available","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"fixed-repair","cloud":"EKS","accelerator_nodes":1,"replacement_policy":"RemoveFirst","replacement_status":"unavailable","replacement_reason_code":"native-repair-on","replacement_reason":"EKS node auto repair is enabled for this node group","replacement_action":"leave repairs to EKS, or turn it off with aws eks update-nodegroup-config --cluster-name training --nodegroup-name fixed-repair --node-repair-config enabled=false; EKS then no longer repairs nodes in this group","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"observe","cloud":"GKE","accelerator_nodes":1,"replacement_policy":"none","status":"not applicable","reason":"no remediation policy replaces these nodes"},
 {"pool":"unwired","cloud":"AKS","accelerator_nodes":1,"replacement_policy":"AddFirst","status":"unknown","reason_code":"no-cloud-provider","reason":"no cloud provider configured","replacement_status":"unknown","replacement_reason_code":"no-cloud-provider","replacement_reason":"no cloud provider configured"},
 {"pool":"zones","cloud":"AKS","accelerator_nodes":2,"replacement_policy":"AddFirst","status":"unavailable","replacement_status":"available","reason":"AKS chooses the zone of a new node in this pool","action":"use a single-zone node pool, set RemoveFirst in the remediation policy that applies to these nodes, or set replacement.successorZone: AnyPoolZone if workloads on these nodes do not need the original zone","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"auto-repair","cloud":"EKS","accelerator_nodes":2,"replacement_policy":"AddFirst","status":"unavailable","reason_code":"native-repair-on","reason":"EKS node auto repair is enabled for this node group","action":"leave repairs to EKS, or turn it off with aws eks update-nodegroup-config --cluster-name training --nodegroup-name auto-repair --node-repair-config enabled=false; EKS then no longer repairs nodes in this group","replacement_status":"unavailable","replacement_reason_code":"native-repair-on","replacement_reason":"EKS node auto repair is enabled for this node group","replacement_action":"leave repairs to EKS, or turn it off with aws eks update-nodegroup-config --cluster-name training --nodegroup-name auto-repair --node-repair-config enabled=false; EKS then no longer repairs nodes in this group","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"no-list","cloud":"EKS","accelerator_nodes":1,"replacement_policy":"AddFirst","status":"unknown","reason_code":"read-failed","reason":"could not read the node group: eks:ListUpdates was denied","action":"allow eks:ListUpdates in the controller's IAM role","replacement_status":"unknown","replacement_reason_code":"read-failed","replacement_reason":"could not read the node group: eks:ListUpdates was denied","replacement_action":"allow eks:ListUpdates in the controller's IAM role","checked_at":"2026-11-18T12:00:00Z"},
 {"pool":"zones","cloud":"GKE","accelerator_nodes":2,"replacement_policy":"AddFirst","status":"unavailable","replacement_status":"available","reason":"add-first would add a node in every zone of this pool","action":"use a single-zone node pool, or set RemoveFirst in the remediation policy that applies to these nodes","checked_at":"2026-11-18T12:00:00Z","reason_code":"multi-zone"},
 {"pool":"zones-any","cloud":"GKE","accelerator_nodes":2,"replacement_policy":"AddFirst","status":"unavailable","replacement_status":"available","reason":"add-first would add a node in every zone of this pool; replacement.successorZone: AnyPoolZone does not change this on GKE","action":"use a single-zone node pool, or set RemoveFirst in the remediation policy that applies to these nodes","checked_at":"2026-11-18T12:00:00Z","reason_code":"multi-zone","successor_zone":"AnyPoolZone"}]}`)
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
