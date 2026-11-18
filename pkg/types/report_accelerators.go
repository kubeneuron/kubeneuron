// SPDX-License-Identifier: Apache-2.0

package types

// Accelerator groups of the recovery report. Each has its own unit; hours of
// different groups, and of different resources, are never added together.
const (
	AcceleratorGroupGPU       = "gpu"
	AcceleratorGroupTPU       = "tpu"
	AcceleratorGroupAWSNeuron = "aws-neuron"
)

// AcceleratorGroupOf is the report group of a device unit, and the name of
// its unit of time; empty for a unit the report does not know.
func AcceleratorGroupOf(unit DeviceUnit) (group, hours string) {
	switch unit {
	case DeviceUnitGPU:
		return AcceleratorGroupGPU, "GPU-hours"
	case DeviceUnitTPUChip:
		return AcceleratorGroupTPU, "TPU chip-hours"
	case DeviceUnitAWSDevice:
		return AcceleratorGroupAWSNeuron, "AWS Neuron device-hours"
	}
	return "", ""
}

// AcceleratorGroupReport is one accelerator group of the window. The group
// carries no number of its own: every number is per resource.
type AcceleratorGroupReport struct {
	Group string `json:"group"`
	// Unit names what the hours of its resources count, e.g. TPU chip-hours.
	Unit      string                      `json:"unit"`
	Resources []AcceleratorResourceReport `json:"resources"`
}

// AcceleratorResourceReport is one accelerator resource's share of the
// window, in device-hours of that resource, counted with the same rules as
// the GPU-hours of the report. The device count is the one recorded when
// each incident opened.
type AcceleratorResourceReport struct {
	Resource          string  `json:"resource"`
	Incidents         int     `json:"incidents"`
	DegradedHours     float64 `json:"degraded_hours"`
	RecoveredHours    float64 `json:"recovered_hours"`
	ObservedOnlyHours float64 `json:"observed_only_hours"`
	SupersededHours   float64 `json:"superseded_hours"`
	// SavedEstimate is present when a manual recovery baseline is set.
	SavedEstimate *AcceleratorSavedEstimate `json:"saved_estimate,omitempty"`
}

// AcceleratorSavedEstimate is the device-hours a resource's recoveries saved
// against the manual recovery baseline, an estimate.
type AcceleratorSavedEstimate struct {
	Hours                 float64 `json:"hours"`
	ManualBaselineSeconds float64 `json:"manual_baseline_seconds"`
	// Cost is present when a rate is set for the resource.
	Cost *CostEstimate `json:"cost_estimate,omitempty"`
}

// CostEstimate is device-hours at the operator's rate, an estimate.
type CostEstimate struct {
	Currency    string  `json:"currency"`
	RatePerHour float64 `json:"rate_per_hour"`
	Amount      float64 `json:"amount"`
}

// CostEstimateTotal is the sum of the cost estimates of every resource shown,
// in one currency. Amount is present only when every resource shown has a
// rate; otherwise RateNotSetFor names those without one.
type CostEstimateTotal struct {
	Currency      string   `json:"currency,omitempty"`
	Amount        *float64 `json:"amount,omitempty"`
	RateNotSetFor []string `json:"rate_not_set_for,omitempty"`
}

// DeviceCountUnknown is the share of the window of incidents whose device
// count was not recorded, in node-hours. It is in no accelerator group and
// carries no cost.
type DeviceCountUnknown struct {
	Incidents             int     `json:"incidents"`
	DegradedNodeHours     float64 `json:"degraded_node_hours"`
	RecoveredNodeHours    float64 `json:"recovered_node_hours"`
	ObservedOnlyNodeHours float64 `json:"observed_only_node_hours"`
	SupersededNodeHours   float64 `json:"superseded_node_hours"`
}

// AcceleratorHours is the report's accounting per accelerator resource.
type AcceleratorHours struct {
	Groups []AcceleratorGroupReport `json:"accelerator_groups,omitempty"`
	// SavedCostTotal is present when a manual recovery baseline is set and
	// any resource is shown.
	SavedCostTotal     *CostEstimateTotal  `json:"saved_cost_estimate_total,omitempty"`
	DeviceCountUnknown *DeviceCountUnknown `json:"device_count_unknown,omitempty"`
}
