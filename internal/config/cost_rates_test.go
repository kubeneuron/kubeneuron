// SPDX-License-Identifier: Apache-2.0

package config

import (
	"math"
	"testing"
)

func TestCostRatesValidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rates *CostRates
		ok    bool
	}{
		{"none", nil, true},
		{"set", &CostRates{Currency: "USD", PerDeviceHour: map[string]float64{"nvidia.com/gpu": 4}}, true},
		{"lower-case currency", &CostRates{Currency: "usd", PerDeviceHour: map[string]float64{"nvidia.com/gpu": 4}}, false},
		{"no resource", &CostRates{Currency: "USD"}, false},
		{"negative", &CostRates{Currency: "USD", PerDeviceHour: map[string]float64{"google.com/tpu": -1}}, false},
		{"nan", &CostRates{Currency: "USD", PerDeviceHour: map[string]float64{"google.com/tpu": math.NaN()}}, false},
	} {
		if err := tc.rates.validate(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if rate, ok := (&CostRates{PerDeviceHour: map[string]float64{"amd.com/gpu": 3}}).Rate("amd.com/gpu"); !ok || rate != 3 {
		t.Fatalf("rate %v %v", rate, ok)
	}
	if _, ok := (*CostRates)(nil).Rate("amd.com/gpu"); ok {
		t.Fatal("a nil rate table has a rate")
	}
}
