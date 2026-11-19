// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/kubeneuron/kubeneuron/internal/nodehealth"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// agentHealthSource is the source name of the rows of the agents' signal
// sources.
const agentHealthSource = "agent"

// maxHealthRows bounds one answer of the accelerator health view.
var maxHealthRows = 5000

// agentSourceToken is the form a source name or reason from an agent must
// have to be shown; anything else is not repeated.
var agentSourceToken = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// AcceleratorHealth is the last read of every accelerator health source,
// for every node or for the one named exactly. It reads only what this
// controller holds in memory and changes nothing. Rows are sorted by node,
// source, profile, rule and device; past maxHealthRows the view is
// truncated. The text of a row is KubeNeuron's own; what a source reports is
// repeated only as a bounded reason, never a condition's message or a
// cloud's error.
func (c *Controller) AcceleratorHealth(ctx context.Context, node string) types.AcceleratorHealthView {
	sources := c.healthSources()
	want := func(n string) bool { return node == "" || n == node }
	var rows []types.AcceleratorHealthRow
	nodes := map[string]bool{}
	if node != "" {
		nodes[node] = true
	}

	c.health.mu.Lock()
	for _, src := range sources {
		name := src.name()
		readAt := c.health.readAt[name]
		if !c.health.read[name] {
			why := "not read since this controller became leader: until it is, no step that takes a node out of service runs on any node"
			if at, ok := c.health.failedAt[name]; ok {
				why = "the reads since this controller became leader failed, the last at " + at.UTC().Format(time.RFC3339) +
					": until one succeeds, no step that takes a node out of service runs on any node"
			}
			rows = append(rows, types.AcceleratorHealthRow{Node: node, Source: name, State: types.HealthRowNotReadThisTerm, Why: why})
			continue
		}
		if at, ok := c.health.failedAt[name]; ok {
			rows = append(rows, types.AcceleratorHealthRow{Node: node, Source: name, State: string(nodehealth.StateUnknown), ReadAt: readAt,
				Why: "the last read failed at " + at.UTC().Format(time.RFC3339) + "; the rows of this source are from the read before"})
		}
		for key, h := range c.health.latest {
			if !strings.HasPrefix(key, name+"/") || !want(h.Node) || len(h.Profiles) == 0 && len(h.Observations) == 0 {
				continue
			}
			nodes[h.Node] = true
			rows = append(rows, healthRows(name, h, readAt)...)
		}
		if name != draHealthSourceName {
			continue
		}
		for n, devices := range c.health.draQuiet {
			if !want(n) {
				continue
			}
			nodes[n] = true
			for _, d := range devices {
				why := "the device carries no taint; the driver may not publish health taints, so this is not health"
				if d.state == nodehealth.DRAUnmonitored {
					why = "the driver says it does not watch the device's health"
				}
				rows = append(rows, types.AcceleratorHealthRow{Node: n, Source: name, Profile: d.profile, Rule: nodehealth.RuleDRADeviceTaint,
					Device: bounded(d.device), State: types.HealthRowNoSignal, Reason: d.state, Why: why, ReadAt: readAt})
			}
		}
	}
	c.health.mu.Unlock()

	for _, row := range c.neuronHeldRows(ctx, want, time.Now()) {
		nodes[row.Node] = true
		rows = append(rows, row)
	}

	if read := c.agentSourceReader(); read != nil {
		for n := range nodes {
			rows = append(rows, agentHealthRows(n, read(n), node != "")...)
		}
	}

	slices.SortFunc(rows, func(a, b types.AcceleratorHealthRow) int {
		return cmp.Or(cmp.Compare(a.Node, b.Node), cmp.Compare(a.Source, b.Source), cmp.Compare(a.Profile, b.Profile),
			cmp.Compare(a.Rule, b.Rule), cmp.Compare(a.Device, b.Device), cmp.Compare(a.State, b.State))
	})
	view := types.AcceleratorHealthView{ProfileVersion: nodehealth.Version, Rows: rows}
	if len(rows) > maxHealthRows {
		view.Rows, view.Truncated = rows[:maxHealthRows], true
	}
	if view.Rows == nil {
		view.Rows = []types.AcceleratorHealthRow{}
	}
	return view
}

// agentSignalSourceReader is a controller that holds what the agents report
// about their signal sources: all nodes for "", or the one named exactly.
type agentSignalSourceReader interface {
	AgentSignalSources(node string) []types.AgentSignalSource
}

// agentSourceReader reads the agents' signal sources, or is nil while the
// controller holds none.
func (c *Controller) agentSourceReader() func(string) []types.AgentSignalSource {
	if c.agentSignalSources != nil {
		return c.agentSignalSources
	}
	if r, ok := any(c).(agentSignalSourceReader); ok {
		return r.AgentSignalSources
	}
	return nil
}

// healthRows are the rows of one node's read of one source: one for every
// observation, one for every profile of the node that has none, and one for
// another repairer the node shows, while which KubeNeuron only observes.
func healthRows(source string, h nodehealth.NodeHealth, readAt time.Time) []types.AcceleratorHealthRow {
	var rows []types.AcceleratorHealthRow
	seen := map[string]bool{}
	for _, o := range h.Observations {
		seen[o.Profile] = true
		rows = append(rows, types.AcceleratorHealthRow{Node: h.Node, Source: source, Profile: o.Profile, Rule: o.Rule,
			Device: bounded(o.Device), State: string(o.State), Class: o.Class, Reason: bounded(o.Reason), Why: bounded(o.Why),
			Since: o.Since, ReadAt: readAt})
	}
	for _, p := range h.Profiles {
		if !seen[p] {
			rows = append(rows, types.AcceleratorHealthRow{Node: h.Node, Source: source, Profile: p, State: string(profileState(h, p)), ReadAt: readAt})
		}
	}
	if len(h.OtherRepairers) > 0 {
		rows = append(rows, types.AcceleratorHealthRow{Node: h.Node, Source: source, Rule: "other-repairer", State: string(nodehealth.StateIgnored), ReadAt: readAt,
			Why: bounded("another repairer is acting on the node (" + strings.Join(h.OtherRepairers, ", ") + "): KubeNeuron only observes it")})
	}
	return rows
}

// agentHealthRows are the rows of the agent's signal sources on one node. A
// source the agent can read is not healthy for it; an agent that lists no
// sources, an older one or none at all, gives no data, shown only when the
// node was asked for.
func agentHealthRows(node string, sources []types.AgentSignalSource, asked bool) []types.AcceleratorHealthRow {
	var rows []types.AcceleratorHealthRow
	for _, s := range sources {
		if s.Node != "" && s.Node != node {
			continue
		}
		rule := s.Source
		if !agentSourceToken.MatchString(rule) {
			rule = "unnamed"
		}
		row := types.AcceleratorHealthRow{Node: node, Source: agentHealthSource, Rule: rule, Class: s.Class, ReadAt: s.ReadAt}
		if s.Available {
			row.State = types.HealthRowAvailable
			row.Why = "the agent reads this source; a fault it reads opens an incident, and none read is not health"
		} else {
			row.State = string(nodehealth.StateUnknown)
			row.Why = "the agent cannot read this source"
			if agentSourceToken.MatchString(s.Reason) {
				row.Reason = s.Reason
			}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 && asked {
		rows = append(rows, types.AcceleratorHealthRow{Node: node, Source: agentHealthSource, State: string(nodehealth.StateUnknown),
			Why: "no data: the node's agent reports no signal sources (an older agent, or no agent)"})
	}
	return rows
}

// maxHealthText bounds a text field of a row.
const maxHealthText = 256

// bounded is s cut to maxHealthText, with control characters replaced.
func bounded(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) > maxHealthText {
		s = strings.ToValidUTF8(s[:maxHealthText], "") + "…"
	}
	return s
}
