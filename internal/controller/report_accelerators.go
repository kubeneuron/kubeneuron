// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// incidentAccelerators is what an incident covers of its node's accelerators
// when it opens: one device of the node's resource for a device-scoped
// incident, every device of the node for a node-scoped one. The agent's GPUs
// are the node's inventory where it registered, named by their vendor; the
// devices the Kubernetes Node advertises otherwise. Nil when the count or the
// resource is not known: nothing is guessed.
func incidentAccelerators(node *types.Node, inc *types.Incident) []types.IncidentAccelerator {
	if node == nil {
		return nil
	}
	var all []types.IncidentAccelerator
	if len(node.GPUs) > 0 {
		resource := agentGPUResource(inc.Vendor, node.GPUs)
		if resource == "" {
			return nil
		}
		all = []types.IncidentAccelerator{{Resource: resource, Unit: types.DeviceUnitGPU, Count: int64(len(node.GPUs))}}
	} else {
		for _, d := range node.Devices {
			if d.Capacity > 0 && d.Unit != "" {
				all = append(all, types.IncidentAccelerator{Resource: d.Resource, Unit: d.Unit, Count: d.Capacity})
			}
		}
	}
	if !inc.Target.IsDeviceScoped() {
		return all
	}
	// One device, of the node's one resource; with several, which one is
	// not known.
	if len(all) != 1 {
		return nil
	}
	all[0].Count = 1
	return all
}

// agentGPUResource is the extended resource of an agent's GPUs: from the
// incident's vendor, else from what every GPU's model or UUID says. Empty
// when they do not say.
func agentGPUResource(vendor types.AcceleratorVendor, gpus []types.GPUInfo) string {
	switch vendor {
	case types.AcceleratorVendorNVIDIA:
		return "nvidia.com/gpu"
	case types.AcceleratorVendorAMD:
		return "amd.com/gpu"
	}
	resource := ""
	for _, g := range gpus {
		var r string
		model := strings.ToUpper(g.Model)
		switch {
		case strings.HasPrefix(model, "NVIDIA"), strings.HasPrefix(g.UUID, "GPU-"), strings.HasPrefix(g.UUID, "MIG-"):
			r = "nvidia.com/gpu"
		case strings.HasPrefix(model, "AMD"):
			r = "amd.com/gpu"
		default:
			return ""
		}
		if resource != "" && r != resource {
			return ""
		}
		resource = r
	}
	return resource
}

// recordedGPUs is how many GPUs an incident recorded when it opened, the
// same count its rows per resource use; false when it recorded nothing, and
// the inventory now is all there is.
func recordedGPUs(inc *types.Incident) (int, bool) {
	if len(inc.Accelerators) == 0 {
		return 0, false
	}
	n := 0
	for _, a := range inc.Accelerators {
		if a.Unit == types.DeviceUnitGPU && a.Count > 0 {
			n += int(a.Count)
		}
	}
	return n, true
}

// coversGPU reports whether an incident belongs in the report's GPU-hour
// fields, which count nvidia.com/gpu and amd.com/gpu only. An incident whose
// recorded devices are none of them does not; one with no record is judged
// by its node's inventory now, as those fields always were.
func coversGPU(inc *types.Incident, inventory map[string]*types.Node) bool {
	if len(inc.Accelerators) > 0 {
		for _, a := range inc.Accelerators {
			if a.Unit == types.DeviceUnitGPU {
				return true
			}
		}
		return false
	}
	node := inventory[inc.Target.Node]
	if node == nil || len(node.GPUs) > 0 || len(node.Devices) == 0 {
		return true
	}
	for _, d := range node.Devices {
		if d.Unit == types.DeviceUnitGPU {
			return true
		}
	}
	return false
}

// acceleratorAccum adds up the window per accelerator resource, and in
// node-hours for incidents with no recorded device count.
type acceleratorAccum struct {
	rows map[string]*types.AcceleratorResourceReport
	unit map[string]types.DeviceUnit
	// recovered and saved are the intervals to count once, by resource and
	// then node.
	recovered, saved map[string]map[string][]gpuBoundary
	unknown          types.DeviceCountUnknown
	unknownRecovered map[string][]gpuBoundary
}

func newAcceleratorAccum() *acceleratorAccum {
	return &acceleratorAccum{rows: map[string]*types.AcceleratorResourceReport{}, unit: map[string]types.DeviceUnit{},
		recovered: map[string]map[string][]gpuBoundary{}, saved: map[string]map[string][]gpuBoundary{},
		unknownRecovered: map[string][]gpuBoundary{}}
}

// accelOutcome is which bucket an incident's hours go to besides degraded.
type accelOutcome int

const (
	accelOpen accelOutcome = iota
	accelSuperseded
	accelObservedOnly
	accelRecovered
)

// add counts one incident: hours is its in-window span in hours; recovered
// and saved are its intervals to count once when it recovered, saved nil
// when it saved nothing.
func (a *acceleratorAccum) add(inc *types.Incident, hours float64, outcome accelOutcome, device string, recovered, saved *[2]time.Time) {
	node := inc.Target.Node
	if len(inc.Accelerators) == 0 {
		u := &a.unknown
		u.Incidents++
		u.DegradedNodeHours += hours
		switch outcome {
		case accelSuperseded:
			u.SupersededNodeHours += hours
		case accelObservedOnly:
			u.ObservedOnlyNodeHours += hours
		case accelRecovered:
			a.unknownRecovered[node] = append(a.unknownRecovered[node],
				gpuBoundary{recovered[0], "", 1}, gpuBoundary{recovered[1], "", -1})
		}
		return
	}
	for _, acc := range inc.Accelerators {
		if acc.Count <= 0 {
			continue
		}
		row := a.rows[acc.Resource]
		if row == nil {
			row = &types.AcceleratorResourceReport{Resource: acc.Resource}
			a.rows[acc.Resource] = row
			a.unit[acc.Resource] = acc.Unit
		}
		h := hours * float64(acc.Count)
		row.Incidents++
		row.DegradedHours += h
		switch outcome {
		case accelSuperseded:
			row.SupersededHours += h
		case accelObservedOnly:
			row.ObservedOnlyHours += h
		case accelRecovered:
			n := int(acc.Count)
			add := func(m map[string]map[string][]gpuBoundary, span [2]time.Time) {
				if m[acc.Resource] == nil {
					m[acc.Resource] = map[string][]gpuBoundary{}
				}
				m[acc.Resource][node] = append(m[acc.Resource][node], gpuBoundary{span[0], device, n}, gpuBoundary{span[1], device, -n})
			}
			add(a.recovered, *recovered)
			if saved != nil {
				add(a.saved, *saved)
			}
		}
	}
}

// result is the accounting, with the saved hours and their cost when a
// baseline is set.
func (a *acceleratorAccum) result(baselineSeconds float64, rates *config.CostRates) types.AcceleratorHours {
	var out types.AcceleratorHours
	groups := map[string]*types.AcceleratorGroupReport{}
	resources := make([]string, 0, len(a.rows))
	for r := range a.rows {
		resources = append(resources, r)
	}
	sort.Strings(resources)
	var total float64
	var missing []string
	for _, r := range resources {
		row := a.rows[r]
		row.DegradedHours = roundHours(row.DegradedHours)
		row.RecoveredHours = roundHours(distinctGPUHours(a.recovered[r]))
		row.ObservedOnlyHours = roundHours(row.ObservedOnlyHours)
		row.SupersededHours = roundHours(row.SupersededHours)
		if baselineSeconds > 0 {
			est := &types.AcceleratorSavedEstimate{Hours: roundHours(distinctGPUHours(a.saved[r])), ManualBaselineSeconds: baselineSeconds}
			if rate, ok := rates.Rate(r); ok {
				est.Cost = &types.CostEstimate{Currency: rates.Currency, RatePerHour: rate, Amount: roundMoney(est.Hours * rate)}
				total += est.Cost.Amount
			} else {
				missing = append(missing, r)
			}
			row.SavedEstimate = est
		}
		group, unit := types.AcceleratorGroupOf(a.unit[r])
		if group == "" {
			group, unit = string(a.unit[r]), string(a.unit[r])+"-hours"
		}
		g := groups[group]
		if g == nil {
			g = &types.AcceleratorGroupReport{Group: group, Unit: unit}
			groups[group] = g
		}
		g.Resources = append(g.Resources, *row)
	}
	for _, name := range []string{types.AcceleratorGroupGPU, types.AcceleratorGroupTPU, types.AcceleratorGroupAWSNeuron} {
		if g := groups[name]; g != nil {
			out.Groups = append(out.Groups, *g)
			delete(groups, name)
		}
	}
	rest := make([]string, 0, len(groups))
	for name := range groups {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	for _, name := range rest {
		out.Groups = append(out.Groups, *groups[name])
	}
	if baselineSeconds > 0 && len(resources) > 0 {
		out.SavedCostTotal = &types.CostEstimateTotal{RateNotSetFor: missing}
		if rates != nil {
			out.SavedCostTotal.Currency = rates.Currency
		}
		if len(missing) == 0 {
			amount := roundMoney(total)
			out.SavedCostTotal.Amount = &amount
		}
	}
	if a.unknown.Incidents > 0 {
		u := a.unknown
		u.DegradedNodeHours = roundHours(u.DegradedNodeHours)
		u.RecoveredNodeHours = roundHours(distinctGPUHours(a.unknownRecovered))
		u.ObservedOnlyNodeHours = roundHours(u.ObservedOnlyNodeHours)
		u.SupersededNodeHours = roundHours(u.SupersededNodeHours)
		out.DeviceCountUnknown = &u
	}
	return out
}

func roundMoney(v float64) float64 { return math.Round(v*100) / 100 }
