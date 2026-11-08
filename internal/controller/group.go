package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Bounded gang coordination (docs/workload-safe-recovery-design.md, S3): the
// peers of a coordinated workload's checkpoint group receive the same request
// with their own deadline from the same formula, and nothing else. This file
// finds and stamps the peers; the observation loop in checkpoint.go waits for
// them exactly as it waits for the node's own candidates, within the same
// bounded window. Peers are never evicted and their nodes never cordoned:
// the disruption stays on the one node.

const auditActionGroup = "checkpoint-group-request"

// groupPeers lists the groups the node-local candidates declare and returns
// the peers to stamp: live members of each group that are not candidates
// themselves, eligible under the policy, and — unless crossNode is on — on
// the disrupted node. A group above maxGroupSize (or one the listing could
// not bound) contributes no peers and is counted skipped. One audit row per
// group names its size and what was stamped.
func (c *Controller) groupPeers(ctx context.Context, checkpointer platform.WorkloadCheckpointer, policy checkpoint.Policy, inc *types.Incident, step *playbook.Step, candidates []platform.Workload) []platform.Workload {
	if !policy.Groups.Enabled {
		return nil
	}
	maxSize := policy.Groups.MaxGroupSize
	if maxSize <= 0 {
		maxSize = checkpoint.DefaultMaxGroupSize
	}
	onNode := map[string]struct{}{}
	groups := map[checkpoint.GroupKey]int{}
	for _, w := range candidates {
		onNode[w.UID] = struct{}{}
		if key, ok := checkpoint.GroupOf(w.Checkpoint()); ok {
			groups[key]++
		}
	}
	if len(groups) == 0 {
		return nil
	}
	keys := make([]checkpoint.GroupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

	// Every row below is written under the coordination context (bounded by
	// advisoryAuditTimeout AND by the window): the group walk is part of the
	// disruption step's pre-phase, and the step's reserve for the disruption
	// itself is never spent on audit appends, however many groups there are.
	// Once the window has closed the remaining groups are skipped in ONE row,
	// written under the same cap but past the cancellation, so the fact that
	// they were not asked survives the budget that prevented asking them.
	var peers []platform.Workload
	for i, key := range keys {
		if ctx.Err() != nil {
			rest := make([]string, 0, len(keys)-i)
			for _, k := range keys[i:] {
				metrics.CheckpointGroupRequests.WithLabelValues(string(checkpoint.OutcomeSkipped)).Inc()
				rest = append(rest, k.String())
			}
			c.auditAdvisory(ctx, inc, auditActionGroup, fmt.Sprintf("checkpoint group(s) %s: skipped (coordination budget spent before the group was listed)", checkpointJoin(rest)))
			break
		}
		members, truncated, err := checkpointer.ListGroupMembers(ctx, policy, key.Namespace, key.Name, maxSize)
		if err != nil {
			metrics.CheckpointGroupRequests.WithLabelValues(string(checkpoint.OutcomeSkipped)).Inc()
			c.log.Warn("checkpoint group: listing failed", "incident", inc.ID, "group", key.String(), "err", err)
			c.auditAdvisoryWithin(ctx, inc, auditActionGroup, fmt.Sprintf("checkpoint group %s: skipped (listing failed)", key))
			continue
		}
		if truncated || len(members) > maxSize {
			metrics.CheckpointGroupRequests.WithLabelValues(string(checkpoint.OutcomeSkipped)).Inc()
			c.auditAdvisoryWithin(ctx, inc, auditActionGroup, fmt.Sprintf("checkpoint group %s: skipped (group_too_large: more than %d members); asking only the %d member(s) on %s",
				key, maxSize, groups[key], inc.Target.Node))
			continue
		}
		var stamped, held []string
		for _, m := range members {
			if _, local := onNode[m.UID]; local {
				continue // a candidate: asked by the step itself
			}
			if m.Node == "" {
				continue // not scheduled: nothing to confine a request to
			}
			if m.Node != inc.Target.Node && !policy.Groups.CrossNode {
				held = append(held, checkpointWorkloadID(m))
				continue
			}
			if policy.Eligibility(m.Checkpoint(), inc.Class) != "" {
				continue
			}
			peers = append(peers, m)
			stamped = append(stamped, checkpointWorkloadID(m))
		}
		row := fmt.Sprintf("checkpoint group %s: %d member(s), %d on %s, requesting %d peer(s) %s",
			key, len(members), groups[key], inc.Target.Node, len(stamped), checkpointJoin(stamped))
		if len(held) > 0 {
			row += fmt.Sprintf("; %d on other node(s) not asked (crossNode off) %s", len(held), checkpointJoin(held))
			for range held {
				metrics.CheckpointGroupRequests.WithLabelValues(string(checkpoint.OutcomeSkipped)).Inc()
			}
		}
		c.auditAdvisoryWithin(ctx, inc, auditActionGroup, row)
	}
	return peers
}

// peerDeadline is the deadline a peer is stamped with: the same formula as a
// node-local candidate (Policy.BoundedWait), from the same request time, so
// the group can never extend the window.
func peerDeadline(policy checkpoint.Policy, peer platform.Workload, requestedAt time.Time, budget time.Duration) time.Time {
	return requestedAt.Add(policy.BoundedWait(peer.Checkpoint(), budget))
}
