// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"testing"
)

func TestCapacityReason(t *testing.T) {
	for _, tc := range []struct {
		code string
		want string
	}{
		{"InsufficientInstanceCapacity", "no capacity for this machine type"},
		{"InstanceLimitExceeded", "quota exceeded for this machine type"},
		{"VcpuLimitExceeded", "quota exceeded for this machine type"},
		{"AsgInstanceLaunchFailures", "no capacity or quota for this machine type"},
		{"InsufficientFreeAddresses", "no free network addresses for a new node"},
		{"unknown", "no capacity or quota for this machine type"},
	} {
		if got := CapacityReason(tc.code); string(got) != tc.want {
			t.Errorf("CapacityReason(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}
