// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/nodehealth"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// HealthPolicy carries the opt-in health sources of
// spec.safety.acceleratorHealth. The zero value reads no optional source.
type HealthPolicy struct {
	// DRA reads the device taints of resource.k8s.io/v1 ResourceSlices.
	DRA bool
}

// draHealthReader is a platform that reads the devices DRA drivers publish
// for the nodes, with their taints: one snapshot for every existing Node.
type draHealthReader interface {
	DRAHealth(ctx context.Context) ([]types.NodeHealthSnapshot, error)
}

const draHealthSourceName = "dra"

// draHealthSource reads the device taints of the DRA drivers. A device
// without a taint gives no observation. A device with a pending fault that
// its existing Node no longer publishes, or a read that cannot be made, is
// unknown: the fault is held, never cleared by what is not there. A Node
// that is gone, as a replaced node ends, takes its pending faults with it.
type draHealthSource struct {
	reader draHealthReader
	c      *Controller
}

func (draHealthSource) name() string { return draHealthSourceName }

func (s draHealthSource) read(ctx context.Context) ([]nodehealth.NodeHealth, error) {
	snaps, err := s.reader.DRAHealth(ctx)
	outcome := "ok"
	if err != nil {
		outcome = "list-failed"
		var derr *platform.DRAHealthError
		if errors.As(err, &derr) {
			outcome = derr.Reason
		}
		if s.c.logOnce.first("dra-health/" + outcome) {
			s.c.log.Warn("could not read device health from the DRA ResourceSlices; their pending faults are held", "reason", outcome, "err", err)
		}
		snaps = nil
	}
	metrics.DRAHealthReads.WithLabelValues(outcome).Inc()
	nodes := map[string]*nodehealth.NodeHealth{}
	var order []string
	exists := map[string]bool{}
	published := map[string]bool{}
	counts := map[[2]string]float64{}
	quiet := map[string][]draQuietDevice{}
	for _, snap := range snaps {
		exists[snap.Node] = true
		h, states := nodehealth.EvaluateDRA(snap)
		for _, d := range snap.DRADevices {
			published[snap.Node+"/"+nodehealth.DRADeviceKey(d)] = true
			if profile := nodehealth.DRAProfile(d.Driver); profile != "" {
				state := states[nodehealth.DRADeviceKey(d)]
				counts[[2]string{profile, state}]++
				if state == nodehealth.DRANoHealthTaint || state == nodehealth.DRAUnmonitored {
					quiet[snap.Node] = append(quiet[snap.Node], draQuietDevice{profile: profile, device: d.Device, state: state})
				}
			}
		}
		if len(h.Observations) > 0 {
			nodes[snap.Node] = &h
			order = append(order, snap.Node)
		}
	}
	// A pending fault of a device its existing Node did not publish in this
	// read is held; that of a Node that is gone is not.
	s.c.health.mu.Lock()
	s.c.health.draQuiet = quiet
	var held []string
	for key := range s.c.health.pending {
		if strings.HasPrefix(key, draHealthSourceName+"/") {
			held = append(held, key)
		}
	}
	s.c.health.mu.Unlock()
	for _, key := range held {
		// healthKey: source/node/profile/rule/signal; the signal is the
		// device key, then "=" and the taint.
		parts := strings.SplitN(key, "/", 5)
		if len(parts) != 5 {
			continue
		}
		node, profile, device := parts[1], parts[2], parts[4]
		if i := strings.Index(device, "="); i >= 0 {
			device = device[:i]
		}
		if published[node+"/"+device] || err == nil && !exists[node] {
			continue
		}
		h := nodes[node]
		if h == nil {
			h = &nodehealth.NodeHealth{Node: node, State: nodehealth.StateUnknown}
			nodes[node] = h
			order = append(order, node)
		}
		if h.State != nodehealth.StateFault {
			h.State = nodehealth.StateUnknown
		}
		if !slices.Contains(h.Profiles, profile) {
			h.Profiles = append(h.Profiles, profile)
		}
		why := "the device is no longer published in a ResourceSlice for the node"
		if err != nil {
			why = "the ResourceSlices could not be read: " + outcome
		}
		h.Observations = append(h.Observations, nodehealth.Observation{Profile: profile, Rule: nodehealth.RuleDRADeviceTaint,
			State: nodehealth.StateUnknown, Key: device, Why: why, Reads: 1})
		counts[[2]string{profile, string(nodehealth.StateUnknown)}]++
	}
	for _, p := range nodehealth.Profiles {
		for _, state := range draDeviceStates {
			metrics.DRAHealthDevices.WithLabelValues(p.Name, state).Set(counts[[2]string{p.Name, state}])
		}
	}
	out := make([]nodehealth.NodeHealth, 0, len(order))
	for _, node := range order {
		out = append(out, *nodes[node])
	}
	return out, nil
}

// draQuietDevice is a device the DRA read lists without a health signal.
type draQuietDevice struct{ profile, device, state string }

// draDeviceStates are the states the DRA device metric counts.
var draDeviceStates = []string{
	string(nodehealth.StateFault), string(nodehealth.StateUnknown), string(nodehealth.StateIgnored),
	nodehealth.DRANoHealthTaint, nodehealth.DRAUnmonitored,
}
