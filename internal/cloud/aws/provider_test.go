package aws

import (
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/cloud"
)

// TestInstanceIDFromProviderID exercises the aws:// providerID parsing that now
// lives in the provider. It moved out of internal/platform/kubernetes so the
// generic platform never carries the scheme.
func TestInstanceIDFromProviderID(t *testing.T) {
	for _, tc := range []struct {
		providerID, want string
		wantErr          bool
	}{
		{"aws:///us-east-1a/i-0abc123def456", "i-0abc123def456", false},
		{"aws:///us-east-1f/i-deadbeef", "i-deadbeef", false},
		{"", "", true}, // no cloud instance
		{"gce://project/zone/instance", "", true}, // not AWS
		{"aws:///us-east-1a/not-an-instance", "", true},
	} {
		got, err := instanceIDFromProviderID(tc.providerID)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("instanceIDFromProviderID(%q) = %q, %v; want %q, err=%t", tc.providerID, got, err, tc.want, tc.wantErr)
		}
	}
}

// TestProviderRegisteredWithCapabilities proves the aws provider registers under
// its name and declares both node-remediation primitives, so the operator's
// capability gate admits RecycleNode and ReplaceNode on AWS.
func TestProviderRegisteredWithCapabilities(t *testing.T) {
	caps, ok := cloud.DeclaredCapabilities(providerName)
	if !ok {
		t.Fatalf("provider %q is not registered", providerName)
	}
	if !caps.ReinitializeInPlace || !caps.Replace {
		t.Fatalf("aws capabilities = %+v, want both primitives declared", caps)
	}
	// The live provider's method must match the registered declaration.
	r := NewWithAPI(&fakeEC2{})
	if r.Name() != providerName {
		t.Fatalf("Name() = %q, want %q", r.Name(), providerName)
	}
	if r.Capabilities() != caps {
		t.Fatalf("Capabilities() = %+v, want %+v", r.Capabilities(), caps)
	}
}
