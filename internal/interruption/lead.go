// SPDX-License-Identifier: Apache-2.0

package interruption

import (
	"fmt"
	"time"
)

// The lead is how long before an announced deadline the warning and the
// cordon start. A notice further away than the lead only opens the incident
// and notifies, so a maintenance announced days ahead does not hold a node out
// of service for days.
const (
	DefaultLead = 10 * time.Minute
	MinLead     = time.Minute
	MaxLead     = time.Hour
)

// EffectiveLead is the lead in force: the default when unset.
func EffectiveLead(d time.Duration) time.Duration {
	if d == 0 {
		return DefaultLead
	}
	return d
}

// ValidateLead accepts zero (the default) or a lead within the bounds.
func ValidateLead(d time.Duration) error {
	if d != 0 && (d < MinLead || d > MaxLead) {
		return fmt.Errorf("interruption lead %s must be between %s and %s", d, MinLead, MaxLead)
	}
	return nil
}
