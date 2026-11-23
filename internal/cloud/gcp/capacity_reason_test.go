// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"testing"
)

func TestCapacityReason(t *testing.T) {
	for _, tc := range []struct {
		code string
		want string
	}{
		{"RESOURCE_POOL_EXHAUSTED", "no capacity for this machine type"},
		{"ZONE_RESOURCE_POOL_EXHAUSTED", "no capacity for this machine type"},
		{"ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS", "no capacity for this machine type"},
		{"REGION_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS", "no capacity for this machine type"},
		{"POOL_CAPACITY_INSUFFICIENT", "no capacity for this machine type"},
		{"QUOTA_EXCEEDED", "quota exceeded for this machine type"},
		{"scale.up.error.out.of.resources", "no capacity for this machine type"},
		{"scale.up.error.quota.exceeded", "quota exceeded for this machine type"},
		{"GCE_STOCKOUT", "no capacity for this machine type"},
		{"GCE_QUOTA_EXCEEDED", "quota exceeded for this machine type"},
		{"unknown", "no capacity or quota for this machine type"},
	} {
		if got := CapacityReason(tc.code); string(got) != tc.want {
			t.Errorf("CapacityReason(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}
