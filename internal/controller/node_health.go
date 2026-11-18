// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/nodehealth"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// healthSource is one place the health of accelerator nodes is read from.
// Every source goes through the same confirmation and incident path; the
// Kubernetes Node is the first, a cloud's monitoring API can be another.
type healthSource interface {
	// name identifies the source in the signal's evidence and in the keys
	// of its pending signals.
	name() string
	// read returns what the source says about every node it covers. A node
	// left out is not covered by this read.
	read(ctx context.Context) ([]nodehealth.NodeHealth, error)
}

// nodeHealthSource reads the health profiles from the Kubernetes Node
// objects.
type nodeHealthSource struct{ reader platform.NodeHealthReader }

func (nodeHealthSource) name() string { return "kubernetes-node" }

func (s nodeHealthSource) read(ctx context.Context) ([]nodehealth.NodeHealth, error) {
	snaps, err := s.reader.NodeHealth(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]nodehealth.NodeHealth, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, nodehealth.Evaluate(snap))
	}
	return out, nil
}

// pendingHealth is one fault signal being confirmed or already reported.
type pendingHealth struct {
	firstSeen time.Time
	reads     int
	reported  bool
}

// healthSignals is the confirmation state of every source's fault signals,
// and the last read of each node.
type healthSignals struct {
	mu      sync.Mutex
	pending map[string]*pendingHealth
	latest  map[string]nodehealth.NodeHealth
}

func (c *Controller) healthSources() []healthSource {
	var out []healthSource
	if reader, ok := c.platform.(platform.NodeHealthReader); ok {
		out = append(out, nodeHealthSource{reader: reader})
	}
	return out
}

// pollNodeHealth reads every health source and reports each fault signal
// once it is confirmed: seen on enough consecutive reads, and for long
// enough. A signal that goes away before that is forgotten, so a flapping
// signal starts over. A signal that turns unknown (stale, or of unknown
// status) is neither confirmed further nor forgotten: unknown is not healthy.
// A failed read changes nothing.
func (c *Controller) pollNodeHealth(ctx context.Context) {
	for _, src := range c.healthSources() {
		nodes, err := src.read(ctx)
		if err != nil {
			if c.logOnce.first("health-read/" + src.name() + "/" + err.Error()) {
				c.log.Warn("could not read accelerator health", "source", src.name(), "err", err)
			}
			continue
		}
		c.observeHealth(ctx, src.name(), nodes)
	}
}

func (c *Controller) observeHealth(ctx context.Context, source string, nodes []nodehealth.NodeHealth) {
	now := c.checkpointNowFunc()()
	prefix := source + "/"
	c.health.mu.Lock()
	if c.health.pending == nil {
		c.health.pending = map[string]*pendingHealth{}
	}
	latest := map[string]nodehealth.NodeHealth{}
	for node, h := range c.health.latest {
		if !strings.HasPrefix(node, prefix) {
			latest[node] = h
		}
	}
	keep := map[string]bool{}
	type report struct {
		node  nodehealth.NodeHealth
		obs   nodehealth.Observation
		key   string
		again bool
	}
	var reports []report
	for _, n := range nodes {
		latest[prefix+n.Node] = n
		for _, o := range n.Observations {
			switch o.State {
			case nodehealth.StateUnknown:
				// Hold the pending signals this one would confirm or clear
				// as they are; with none of a profile's conditions on the
				// node, all of that profile's condition signals.
				hold := healthKey(source, n.Node, o, o.Key)
				for key := range c.health.pending {
					if strings.HasPrefix(key, hold) ||
						o.Rule == "conditions" && strings.HasPrefix(key, source+"/"+n.Node+"/"+o.Profile+"/") && strings.Contains(key, "/condition:") {
						keep[key] = true
					}
				}
			case nodehealth.StateFault:
				key := healthKey(source, n.Node, o, o.Key)
				keep[key] = true
				p := c.health.pending[key]
				if p == nil {
					p = &pendingHealth{firstSeen: now}
					c.health.pending[key] = p
				}
				p.reads++
				if p.reads < o.Reads || now.Sub(p.firstSeen) < o.Hold {
					continue
				}
				reports = append(reports, report{node: n, obs: o, key: key, again: p.reported})
			}
		}
	}
	for key := range c.health.pending {
		if strings.HasPrefix(key, prefix) && !keep[key] {
			delete(c.health.pending, key)
		}
	}
	c.health.latest = latest
	c.health.mu.Unlock()

	c.countNodeHealth(source, nodes)
	for _, r := range reports {
		if r.again && !c.healthStillUnresolved(ctx, r.node.Node, r.obs.Class) {
			continue
		}
		if err := c.IngestSignal(ctx, healthSignal(source, r.node, r.obs, now)); err != nil {
			if c.logOnce.first("health-ingest/" + r.key + "/" + err.Error()) {
				c.log.Warn("could not record an accelerator health signal", "node", r.node.Node, "rule", r.obs.Rule, "err", err)
			}
			continue
		}
		c.health.mu.Lock()
		if p := c.health.pending[r.key]; p != nil {
			p.reported = true
		}
		c.health.mu.Unlock()
	}
}

// healthStillUnresolved reports whether a signal already reported, and
// still present, must be reported again: its incident was closed while the
// node still reports the fault, or the incident is verifying a recovery the
// node contradicts. While the incident is open otherwise, nothing is added.
func (c *Controller) healthStillUnresolved(ctx context.Context, node string, class types.ProblemClass) bool {
	inc, err := c.store.GetOpenIncident(ctx, types.Target{Node: node}, class)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true
	case err != nil:
		return false
	}
	return inc.State == types.StateVerifying
}

func healthKey(source, node string, o nodehealth.Observation, signal string) string {
	return source + "/" + node + "/" + o.Profile + "/" + o.Rule + "/" + signal
}

// healthSignal is the signal of one confirmed fault, with what the node
// reported as its evidence. The condition's message is not part of it.
func healthSignal(source string, n nodehealth.NodeHealth, o nodehealth.Observation, now time.Time) types.Signal {
	severity := types.SeverityCritical
	if o.Class == types.ClassAcceleratorUnallocatable {
		severity = types.SeverityWarning
	}
	evidence := map[string]string{
		"health_source":   source,
		"profile":         o.Profile,
		"profile_version": nodehealth.Version,
		"rule":            o.Rule,
		"signal":          o.Key,
		"other_repairer":  "unknown",
	}
	if o.Reason != "" {
		evidence["reason"] = o.Reason
	}
	if o.Device != "" {
		evidence["device"] = o.Device
	}
	if !o.Since.IsZero() {
		evidence["since"] = o.Since.UTC().Format(time.RFC3339)
	}
	if len(n.OtherRepairers) > 0 {
		evidence["other_repairer"] = strings.Join(n.OtherRepairers, ",")
	}
	if n.Slice != "" {
		evidence["tpu_slice"] = n.Slice
	}
	return types.Signal{
		Target: types.Target{Node: n.Node}, Class: o.Class, Severity: severity,
		Source: types.SourceNodeHealth, ObservedAt: now, Evidence: evidence,
	}
}

// countNodeHealth publishes how many nodes each profile reads in each state.
// Node names stay out of the metric.
func (c *Controller) countNodeHealth(source string, nodes []nodehealth.NodeHealth) {
	counts := map[[2]string]float64{}
	for _, n := range nodes {
		for _, p := range n.Profiles {
			counts[[2]string{p, string(profileState(n, p))}]++
			// A node reporting a signal that is read and not acted on is
			// counted as ignored as well, so the signal is not silent.
			for _, o := range n.Observations {
				if o.Profile == p && o.State == nodehealth.StateIgnored {
					counts[[2]string{p, string(nodehealth.StateIgnored)}]++
					break
				}
			}
		}
	}
	for _, p := range nodehealth.Profiles {
		for _, s := range []nodehealth.State{nodehealth.StateHealthy, nodehealth.StateFault, nodehealth.StateUnknown, nodehealth.StateIgnored} {
			metrics.AcceleratorHealthNodes.WithLabelValues(source, p.Name, string(s)).Set(counts[[2]string{p.Name, string(s)}])
		}
	}
}

func profileState(n nodehealth.NodeHealth, profile string) nodehealth.State {
	state := nodehealth.StateHealthy
	for _, o := range n.Observations {
		if o.Profile != profile {
			continue
		}
		switch o.State {
		case nodehealth.StateFault:
			return nodehealth.StateFault
		case nodehealth.StateUnknown:
			state = nodehealth.StateUnknown
		}
	}
	return state
}

// healthRecoveryRefusal is why no step that takes the node out of service
// may run on it, from the last health read: another component is already
// repairing the node, or the node is one host of a multi-host TPU slice,
// which is recovered as a whole or not at all.
func (c *Controller) healthRecoveryRefusal(node, action string) string {
	c.health.mu.Lock()
	defer c.health.mu.Unlock()
	var repairers []string
	slice := ""
	for _, h := range c.health.latest {
		if h.Node != node {
			continue
		}
		repairers = append(repairers, h.OtherRepairers...)
		if h.Slice != "" {
			slice = h.Slice
		}
	}
	if len(repairers) > 0 {
		slices.Sort(repairers)
		return "another repairer is acting on the node (" + strings.Join(slices.Compact(repairers), ", ") + "): KubeNeuron only observes it"
	}
	if slice != "" && (action == "platform.recycle_node" || action == "platform.replace_node") {
		return "the node is one host of the multi-host TPU slice in node pool " + slice + ": replacing one host is not a recovery of the slice, and slice recovery is not available"
	}
	return ""
}
