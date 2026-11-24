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

// Guidance describes the operator's next action when another attempt is not guaranteed.
func (r Reason) Guidance() string {
	switch r {
	case Unavailable:
		return "The cloud reported no capacity for this machine type in this zone. Use a pool in another zone or with another machine type, or wait until the cloud has capacity."
	case Quota:
		return "The account's quota for this machine type is used up. Request a quota increase from the cloud provider."
	case NetworkAddresses:
		return "The pool's subnet has no free addresses for another node. Free addresses or extend the subnet."
	case NotAvailableToSubscription:
		return "This machine type is not available to the subscription in this zone or region right now. Check the subscription's restrictions for this size, or use a pool in another zone or region."
	default:
		return "The cloud could not add a machine of this type; its answer does not tell capacity from quota. Check the account's quota for this machine type first, then the provider's status for the zone."
	}
}
