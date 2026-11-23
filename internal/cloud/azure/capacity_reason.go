// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"strings"

	"github.com/kubeneuron/kubeneuron/internal/cloud/capacityreason"
)

// CapacityReason interprets a provider code after the caller identifies a capacity failure.
func CapacityReason(code string) capacityreason.Reason {
	switch strings.ToLower(code) {
	// https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/availability-performance/cluster-node-virtual-machine-failed-state
	case "allocationfailed",
		"zonalallocationfailed",
		"overconstrainedallocationrequest",
		"overconstrainedzonalallocationrequest":
		return capacityreason.Unavailable
	// https://learn.microsoft.com/troubleshoot/azure/azure-kubernetes/create-upgrade-delete/quota-exceeded-during-creation-upgrade
	case "quotaexceeded",
		"insufficientvcpuquota":
		return capacityreason.Quota
	// https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/availability-performance/cluster-node-virtual-machine-failed-state
	case "resourcegroupquotaexceeded":
		return capacityreason.Quota
	// https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/error-codes/aks-error-code-page
	case "akscapacityerror", "akscapacityheavyusage":
		return capacityreason.Unavailable
	// https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/error-codes/aks-error-code-page
	case "skunotavailable":
		return capacityreason.NotAvailableToSubscription
	case "subnetisfull", "insufficientsubnetsize", "publicipcountlimitreached":
		return capacityreason.NetworkAddresses
	default:
		return capacityreason.Unspecified
	}
}
