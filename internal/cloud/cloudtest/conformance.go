// SPDX-License-Identifier: Apache-2.0

// Package cloudtest holds the contract checks every cloud provider passes.
package cloudtest

import (
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// Case is what a provider's conformance run needs from it.
type Case struct {
	// Provider is a live provider over a fake client.
	Provider cloud.Provider
	// ProviderID is a node providerID of this cloud; Instance is its instance.
	ProviderID, Instance string
	// Foreign are providerIDs this provider must refuse.
	Foreign []string
}

// Conformance checks what the contract promises of any provider: it is
// registered under its name with the capabilities it reports, parses its own
// providerID, refuses an empty or foreign one, and carries the native-repair
// contract if it can replace a node. No call reaches a cloud.
func Conformance(t *testing.T, c Case) {
	t.Helper()
	p := c.Provider
	declared, ok := cloud.DeclaredCapabilities(p.Name())
	if !ok {
		t.Fatalf("provider %q is not registered", p.Name())
	}
	if !cloud.Registered(p.Name()) {
		t.Fatalf("provider %q does not report as registered", p.Name())
	}
	caps := p.Capabilities()
	if caps.ReinitializeInPlace && !declared.ReinitializeInPlace || caps.Replace && !declared.Replace {
		t.Fatalf("live capabilities %+v exceed the declared %+v", caps, declared)
	}
	got, err := p.InstanceID(c.ProviderID)
	if err != nil || got != c.Instance {
		t.Fatalf("InstanceID(%q) = %q, %v; want %q", c.ProviderID, got, err, c.Instance)
	}
	for _, id := range append([]string{""}, c.Foreign...) {
		if got, err := p.InstanceID(id); err == nil {
			t.Fatalf("InstanceID(%q) = %q, want a refusal", id, got)
		}
	}
	if _, aware := p.(cloud.RepairAwareProvider); !aware && caps.Replace {
		t.Fatalf("provider %q replaces nodes without the native-repair contract", p.Name())
	}
}
