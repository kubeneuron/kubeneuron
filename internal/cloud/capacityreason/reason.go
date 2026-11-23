// SPDX-License-Identifier: Apache-2.0

// Package capacityreason describes the cause of an already identified capacity failure.
package capacityreason

type Reason string

const (
	NetworkAddresses           Reason = "no free network addresses for a new node"
	NotAvailableToSubscription Reason = "this machine type is not available to this subscription in this zone or region"
	Unavailable                Reason = "no capacity for this machine type"
	Quota                      Reason = "quota exceeded for this machine type"
	Unspecified                Reason = "no capacity or quota for this machine type"
)
