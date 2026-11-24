// SPDX-License-Identifier: Apache-2.0

package capacityreason

import "testing"

func TestGuidance(t *testing.T) {
	tests := map[Reason]string{
		Unavailable:                "The cloud reported no capacity for this machine type in this zone. Use a pool in another zone or with another machine type, or wait until the cloud has capacity.",
		Quota:                      "The account's quota for this machine type is used up. Request a quota increase from the cloud provider.",
		NetworkAddresses:           "The pool's subnet has no free addresses for another node. Free addresses or extend the subnet.",
		NotAvailableToSubscription: "This machine type is not available to the subscription in this zone or region right now. Check the subscription's restrictions for this size, or use a pool in another zone or region.",
		Unspecified:                "The cloud could not add a machine of this type; its answer does not tell capacity from quota. Check the account's quota for this machine type first, then the provider's status for the zone.",
	}
	for reason, want := range tests {
		if got := reason.Guidance(); got != want {
			t.Errorf("%s: got %q, want %q", reason, got, want)
		}
	}
}
