// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/evidence"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// EvidenceAction is the audit action of an evidence row.
const EvidenceAction = evidence.AuditAction

type nodeEvidenceReader interface {
	NodeEvidence(context.Context, string) (*evidence.Node, error)
}

// recordEvidence reads the opened incident's node and the other open
// incidents, and records the evidence block in the audit trail. When the
// incident joins a shared group, every other member gets a row with the new
// fault location too. It runs after the open is durable and only records: no step
// reads it to start or authorise anything.
func (c *Controller) recordEvidence(ctx context.Context, opened *types.Incident) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var node *evidence.Node
	if reader, ok := c.platform.(nodeEvidenceReader); ok && opened.Target.Node != "" {
		n, err := reader.NodeEvidence(ctx, opened.Target.Node)
		if err != nil {
			// The evidence reads unknown; the reason is for the operator.
			c.log.Warn("could not read the node for incident evidence", "incident", opened.ID, "node", opened.Target.Node, "err", err)
		}
		node = n
	}
	peers, err := c.evidencePeers(ctx, opened)
	if err != nil {
		c.log.Warn("could not read open incidents for the fault location", "incident", opened.ID, "err", err)
	}
	ev := evidence.Build(opened, node, peers, time.Now())
	if err != nil {
		ev.Location = types.LocationEvidence{Location: types.LocationUnknown, Rule: "the other open incidents could not be read"}
	}
	c.appendEvidence(ctx, opened, ev)
	metrics.IncidentEvidence.WithLabelValues(string(opened.Class), string(ev.Location.Location), string(ev.Software.State), string(ev.Network.State)).Inc()
	if ev.Location.Location != types.LocationShared {
		return
	}
	for _, id := range ev.Location.Members {
		if id == opened.ID {
			continue
		}
		peer, err := c.store.GetIncident(ctx, id)
		if err != nil {
			c.log.Warn("could not regroup an incident by fault location", "incident", id, "group", ev.Location.Group, "err", err)
			continue
		}
		if data, err := json.Marshal(ev.Location); err == nil {
			c.appendEvidenceRow(ctx, peer, map[string]string{"fault_location": string(data)})
		}
	}
}

func (c *Controller) appendEvidence(ctx context.Context, inc *types.Incident, ev types.IncidentEvidence) {
	if data, err := json.Marshal(ev); err == nil {
		c.appendEvidenceRow(ctx, inc, map[string]string{"incident_evidence": string(data)})
	}
}

// appendEvidenceRow records an evidence row: the whole block at open, or only
// the fault location when a later incident regroups this one.
func (c *Controller) appendEvidenceRow(ctx context.Context, inc *types.Incident, params map[string]string) {
	if err := c.store.AppendAudit(ctx, &types.AuditEntry{
		IncidentID: inc.ID, Time: time.Now(), Actor: "system", Action: EvidenceAction,
		FromState: inc.State, ToState: inc.State, Params: params, DryRun: inc.DryRun,
	}); err != nil {
		c.log.Warn("could not record incident evidence", "incident", inc.ID, "err", err)
	}
}

// evidencePeers are the other open incidents of the opened one's class
// within the window, each with the group it already belongs to.
func (c *Controller) evidencePeers(ctx context.Context, opened *types.Incident) ([]evidence.Peer, error) {
	open, err := c.openIncidents(ctx)
	if err != nil {
		return nil, err
	}
	var peers []evidence.Peer
	for _, inc := range open {
		if inc.ID == opened.ID || inc.Class != opened.Class {
			continue
		}
		if d := inc.OpenedAt.Sub(opened.OpenedAt); d > evidence.Window || d < -evidence.Window {
			continue
		}
		peer := evidence.Peer{ID: inc.ID, Node: inc.Target.Node, Class: inc.Class, OpenedAt: inc.OpenedAt}
		if trail, err := c.store.AuditTrail(ctx, inc.ID); err == nil {
			if ev := evidence.Latest(trail); ev != nil {
				peer.Group = ev.Location.Group
			}
		}
		peers = append(peers, peer)
	}
	return peers, nil
}
