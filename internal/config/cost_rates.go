// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"math"
	"regexp"
)

// CostRates are the operator's cost of one device-hour of each accelerator
// resource, in one currency. The recovery report uses them only to show the
// device-hours saved as an estimated cost; there are no default rates.
type CostRates struct {
	Currency      string             `yaml:"currency"`
	PerDeviceHour map[string]float64 `yaml:"per_device_hour"`
}

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

func (r *CostRates) validate() error {
	if r == nil {
		return nil
	}
	if !currencyCode.MatchString(r.Currency) {
		return errors.New("recovery_cost_rates.currency must be an ISO 4217 code such as USD")
	}
	if len(r.PerDeviceHour) == 0 {
		return errors.New("recovery_cost_rates.per_device_hour names no resource")
	}
	for resource, rate := range r.PerDeviceHour {
		if resource == "" || rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) || rate > 1e9 {
			return fmt.Errorf("recovery_cost_rates.per_device_hour[%q] must be a rate of at least 0", resource)
		}
	}
	return nil
}

// Rate is the cost of one device-hour of resource, when one is set.
func (r *CostRates) Rate(resource string) (float64, bool) {
	if r == nil {
		return 0, false
	}
	rate, ok := r.PerDeviceHour[resource]
	return rate, ok
}
