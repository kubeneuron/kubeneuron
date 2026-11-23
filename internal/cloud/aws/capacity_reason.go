// SPDX-License-Identifier: Apache-2.0

package aws

import "github.com/kubeneuron/kubeneuron/internal/cloud/capacityreason"

// CapacityReason interprets a provider code after the caller identifies a capacity failure.
func CapacityReason(code string) capacityreason.Reason {
	switch code {
	// https://docs.aws.amazon.com/ec2/latest/devguide/errors-overview.html
	case "InsufficientInstanceCapacity":
		return capacityreason.Unavailable
	// https://docs.aws.amazon.com/ec2/latest/devguide/errors-overview.html
	case "InstanceLimitExceeded",
		"VcpuLimitExceeded":
		return capacityreason.Quota
	// https://docs.aws.amazon.com/eks/latest/APIReference/API_Issue.html
	case "InsufficientFreeAddresses":
		return capacityreason.NetworkAddresses
	case "AsgInstanceLaunchFailures":
		return capacityreason.Unspecified
	default:
		return capacityreason.Unspecified
	}
}
