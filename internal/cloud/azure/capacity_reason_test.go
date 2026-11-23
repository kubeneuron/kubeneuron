// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/cloud/capacityreason"
)

func TestCapacityReason(t *testing.T) {
	for _, tc := range []struct {
		code string
		want string
	}{
		{"AllocationFailed", "no capacity for this machine type"},
		{"ZonalAllocationFailed", "no capacity for this machine type"},
		{"OverconstrainedAllocationRequest", "no capacity for this machine type"},
		{"OverconstrainedZonalAllocationRequest", "no capacity for this machine type"},
		{"QuotaExceeded", "quota exceeded for this machine type"},
		{"InsufficientVCPUQuota", "quota exceeded for this machine type"},
		{"ResourceGroupQuotaExceeded", "quota exceeded for this machine type"},
		{"AKSCapacityError", "no capacity for this machine type"},
		{"AksCapacityHeavyUsage", "no capacity for this machine type"},
		{"SkuNotAvailable", "this machine type is not available to this subscription in this zone or region"},
		{"SubnetIsFull", "no free network addresses for a new node"},
		{"InsufficientSubnetSize", "no free network addresses for a new node"},
		{"PublicIPCountLimitReached", "no free network addresses for a new node"},
		{"sKuNoTaVaIlAbLe", "this machine type is not available to this subscription in this zone or region"},
		{"unknown", "no capacity or quota for this machine type"},
	} {
		if got := CapacityReason(tc.code); string(got) != tc.want {
			t.Errorf("CapacityReason(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestCapacityCodesHaveSpecificReasons(t *testing.T) {
	for _, code := range capacityCodes {
		if CapacityReason(code) == capacityreason.Unspecified {
			t.Errorf("capacity code %q needs a specific reason", code)
		}
	}
}
