// SPDX-License-Identifier: Apache-2.0

package gcp

import "github.com/kubeneuron/kubeneuron/internal/cloud/capacityreason"

// CapacityReason interprets a provider code after the caller identifies a capacity failure.
func CapacityReason(code string) capacityreason.Reason {
	switch code {
	// https://cloud.google.com/compute/docs/reference/rest/v1/errors
	case "RESOURCE_POOL_EXHAUSTED",
		"ZONE_RESOURCE_POOL_EXHAUSTED",
		"ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS",
		"REGION_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS",
		"POOL_CAPACITY_INSUFFICIENT":
		return capacityreason.Unavailable
	// https://cloud.google.com/compute/docs/reference/rest/v1/errors
	case "QUOTA_EXCEEDED":
		return capacityreason.Quota
	// https://cloud.google.com/kubernetes-engine/docs/troubleshooting/cluster-autoscaler-scale-up
	case "scale.up.error.out.of.resources":
		return capacityreason.Unavailable
	// https://cloud.google.com/kubernetes-engine/docs/troubleshooting/cluster-autoscaler-scale-up
	case "scale.up.error.quota.exceeded":
		return capacityreason.Quota
	// https://cloud.google.com/kubernetes-engine/docs/reference/rest/v1/StatusCondition
	case "GCE_STOCKOUT":
		return capacityreason.Unavailable
	// https://cloud.google.com/kubernetes-engine/docs/reference/rest/v1/StatusCondition
	case "GCE_QUOTA_EXCEEDED":
		return capacityreason.Quota
	default:
		return capacityreason.Unspecified
	}
}
