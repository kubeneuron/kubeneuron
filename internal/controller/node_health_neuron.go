// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/nodehealth"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	// neuronHeldMax bounds the faults kept for the health view.
	neuronHeldMax = 1000
	// neuronHeldFor is how long a fault not acted on is shown.
	neuronHeldFor = 24 * time.Hour
	// neuronActedLag is how long after a fault the incident it opened may
	// have been opened: the agent's clock and the outbox both lag.
	neuronActedLag = 10 * time.Minute
)

// neuronSysfsNotActedOn is why a fault of the AWS Neuron sysfs read opened no
// incident.
const neuronSysfsNotActedOn = "not acted on: enable spec.safety.acceleratorHealth.awsNeuronSysfs.enabled " +
	"to open an incident for it; nothing is held, cordoned or replaced for this fault"

// neuronDevice is the form of a device the agent names: its index in the
// driver's tree.
var neuronDevice = regexp.MustCompile(`^neuron[0-9]{1,3}$`)

// faultEventReader is a store that reads back the archived agent events of
// one vendor's faults.
type faultEventReader interface {
	RecentFaultEvents(ctx context.Context, vendor string, since time.Time, limit int) ([]types.AgentEvent, error)
}

// heldNeuronFault is one fault of the AWS Neuron sysfs read that opened no
// incident.
type heldNeuronFault struct {
	node, device, code, component string
	class                         types.ProblemClass
	at                            time.Time
}

// agentEventSignal is the signal of an agent event, or false when it opens
// no incident. A fault of the AWS Neuron sysfs read opens one only while the
// installation opts in; otherwise it is counted and kept for the health
// view. Every aws fault code is that read's: the gate is on the vendor, so a
// source name an agent spells otherwise cannot pass it.
func (c *Controller) agentEventSignal(ctx context.Context, ev types.AgentEvent) (types.Signal, bool) {
	rc := c.runtimeConfig(ctx)
	sig, ok := rc.Catalog.SignalFromAgentEvent(ev)
	if !ok || ev.Fault == nil || ev.Fault.Vendor != string(types.AcceleratorVendorAWS) || rc.Health.AWSNeuronSysfs {
		return sig, ok
	}
	metrics.AcceleratorFaultsNotActedOn.WithLabelValues(detect.NeuronSysfsSource, string(sig.Class)).Inc()
	c.health.mu.Lock()
	defer c.health.mu.Unlock()
	c.holdNeuronFaultLocked(ev, sig.Class, time.Now())
	return types.Signal{}, false
}

// holdNeuronFaultLocked keeps one fault not acted on, the newest of its
// node, device and code.
func (c *Controller) holdNeuronFaultLocked(ev types.AgentEvent, class types.ProblemClass, now time.Time) {
	held := heldNeuronFault{node: ev.Node, code: ev.Fault.Code, class: class, at: ev.Timestamp}
	if d := ev.Fault.Attributes["device"]; neuronDevice.MatchString(d) {
		held.device = d
	}
	if comp := ev.Fault.Attributes["component"]; agentSourceToken.MatchString(comp) {
		held.component = comp
	}
	if held.at.IsZero() {
		held.at = now
	}
	if c.health.neuronHeld == nil {
		c.health.neuronHeld = map[string]heldNeuronFault{}
	}
	key := held.node + "/" + held.device + "/" + held.code
	prev, known := c.health.neuronHeld[key]
	if known && prev.at.After(held.at) {
		return
	}
	if !known && len(c.health.neuronHeld) >= neuronHeldMax {
		c.dropOldestNeuronHeldLocked()
	}
	c.health.neuronHeld[key] = held
}

// restoreNeuronHeld reads back, once a term, the faults of the last day from
// the event archive, so a restart or a new leader does not show a broken
// device as quiet: the agent sends a counter's rise once. A fault read while
// the installation had opted in has an incident of its node that was open
// when it came; it was acted on and is not shown. The archive is read once a
// term, also when the read fails, since it scans the event table; an event
// sink that cannot read it back, such as an external archive, restores
// nothing.
func (c *Controller) restoreNeuronHeld(ctx context.Context, now time.Time) {
	c.health.mu.Lock()
	done := c.health.neuronRestored
	c.health.mu.Unlock()
	reader, ok := c.sink.(faultEventReader)
	if done || !ok {
		return
	}
	since := now.Add(-neuronHeldFor)
	events, err := reader.RecentFaultEvents(ctx, string(types.AcceleratorVendorAWS), since, neuronHeldMax)
	var incidents []*types.Incident
	if err == nil && len(events) > 0 {
		incidents, err = c.store.ListIncidents(ctx, store.IncidentFilter{ActiveSince: since})
	}
	catalog := c.runtimeConfig(ctx).Catalog
	c.health.mu.Lock()
	defer c.health.mu.Unlock()
	if c.health.neuronRestored {
		return
	}
	c.health.neuronRestored = true
	if err != nil {
		c.log.Warn("reading back the AWS Neuron faults not acted on failed", "err", err)
		return
	}
	// Oldest first, so the bound keeps the newest.
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if sig, ok := catalog.SignalFromAgentEvent(ev); ok && !neuronActedOn(ev, sig.Class, incidents) {
			c.holdNeuronFaultLocked(ev, sig.Class, now)
		}
	}
}

// neuronActedOn reports whether an incident of the fault's node and class
// was open when the fault came, so the fault opened it or was attached to it.
func neuronActedOn(ev types.AgentEvent, class types.ProblemClass, incidents []*types.Incident) bool {
	for _, inc := range incidents {
		if inc.Target.Node != ev.Node || inc.Class != class || inc.OpenedAt.After(ev.Timestamp.Add(neuronActedLag)) {
			continue
		}
		// Only a resolved incident records when it ended; an expired or
		// superseded one ended at its last change of state.
		if !inc.State.Terminal() {
			return true
		}
		ended := inc.StateChangedAt
		if inc.ResolvedAt != nil {
			ended = *inc.ResolvedAt
		}
		if !ended.Before(ev.Timestamp) {
			return true
		}
	}
	return false
}

// dropOldestNeuronHeldLocked forgets the oldest fault kept.
func (c *Controller) dropOldestNeuronHeldLocked() {
	var oldest string
	var at time.Time
	for k, h := range c.health.neuronHeld {
		if oldest == "" || h.at.Before(at) {
			oldest, at = k, h.at
		}
	}
	delete(c.health.neuronHeld, oldest)
}

// neuronHeldRows are the health rows of the faults not acted on of the
// nodes want accepts, from the last day, read back from the event archive
// after a restart. Once the installation opts in they
// are dropped: a fault read before then stays without an incident, and the
// incidents of the faults read after say the rest.
func (c *Controller) neuronHeldRows(ctx context.Context, want func(string) bool, now time.Time) []types.AcceleratorHealthRow {
	enabled := c.runtimeConfig(ctx).Health.AWSNeuronSysfs
	if !enabled {
		c.restoreNeuronHeld(ctx, now)
	}
	c.health.mu.Lock()
	defer c.health.mu.Unlock()
	var rows []types.AcceleratorHealthRow
	for k, h := range c.health.neuronHeld {
		if enabled || now.Sub(h.at) > neuronHeldFor {
			delete(c.health.neuronHeld, k)
			continue
		}
		if !want(h.node) {
			continue
		}
		rows = append(rows, types.AcceleratorHealthRow{Node: h.node, Source: detect.NeuronSysfsSource, Profile: "aws-neuron",
			Rule: h.code, Device: h.device, State: string(nodehealth.StateFault), Class: h.class, Reason: h.component,
			Why: neuronSysfsNotActedOn, Since: h.at, ReadAt: h.at})
	}
	slices.SortFunc(rows, func(a, b types.AcceleratorHealthRow) int {
		return cmp.Or(cmp.Compare(a.Node, b.Node), cmp.Compare(a.Device, b.Device), cmp.Compare(a.Rule, b.Rule))
	})
	return rows
}
