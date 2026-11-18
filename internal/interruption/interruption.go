// SPDX-License-Identifier: Apache-2.0

// Package interruption reads the interruptions a cloud has announced for a
// node from what the cloud's own Kubernetes components put on the Node
// object. It needs no cloud credentials and runs nothing on the node. Only the
// signals below, each confirmed in the provider's documentation or the
// component's source, are read; a node without one of them has no announced
// interruption as far as this package can tell, which is not the same as
// none.
package interruption

import (
	"strconv"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// Taint is a node taint's key and value.
type Taint struct {
	Key, Value string
}

// Condition is a node condition's type, status and last transition.
type Condition struct {
	Type, Status   string
	LastTransition time.Time
}

// Node is what the signals need of one node.
type Node struct {
	Name, ProviderID string
	Taints           []Taint
	Labels           map[string]string
	Conditions       []Condition
}

// Notice is an announced interruption of a named node.
type Notice struct {
	Node string `json:"node"`
	cloud.Interruption
}

// Signal sources, shown with the incident.
const (
	SourceGKEMaintenanceTaint = "gke:taint/cloud.google.com/impending-node-termination"
	SourceGKEMaintenanceLabel = "gke:label/cloud.google.com/scheduled-maintenance-time"
	SourceAKSScheduledEvent   = "aks:condition/VMEventScheduled"
	SourceNTHSpot             = "aws-node-termination-handler:taint/spot-itn"
	SourceNTHRebalance        = "aws-node-termination-handler:taint/rebalance-recommendation"
	SourceNTHScheduled        = "aws-node-termination-handler:taint/scheduled-maintenance"
)

// taintSignals maps a taint key to what it announces. GKE sets
// impending-node-termination when host maintenance starts on a node that
// cannot live-migrate (GPU and TPU nodes); the AWS Node Termination Handler
// sets its taints, with the event ID as the value, when started with
// --taint-node.
var taintSignals = map[string]struct {
	kind   cloud.InterruptionKind
	source string
}{
	"cloud.google.com/impending-node-termination":           {cloud.InterruptionHostMaintenance, SourceGKEMaintenanceTaint},
	"aws-node-termination-handler/spot-itn":                 {cloud.InterruptionSpotReclaim, SourceNTHSpot},
	"aws-node-termination-handler/rebalance-recommendation": {cloud.InterruptionRebalance, SourceNTHRebalance},
	"aws-node-termination-handler/scheduled-maintenance":    {cloud.InterruptionScheduledMaintenance, SourceNTHScheduled},
}

// assumedNotice is how long after it is first read a notice without a time is
// assumed to take the node, per source. The AWS spot notice comes two
// minutes before the instance stops, and the Node Termination Handler taints
// on receipt. Azure gives a preempted VM at least 30 seconds, and the AKS
// condition does not say which event is scheduled. GKE documents no time
// between its taint and the termination; its default pod grace period is 30
// seconds. The handler's scheduled-maintenance taint comes with an immediate
// drain.
var assumedNotice = map[string]time.Duration{
	SourceNTHSpot:             2 * time.Minute,
	SourceAKSScheduledEvent:   30 * time.Second,
	SourceGKEMaintenanceTaint: 30 * time.Second,
	SourceNTHScheduled:        30 * time.Second,
}

// defaultAssumedNotice is assumed for a source not in assumedNotice.
const defaultAssumedNotice = 30 * time.Second

// AssumedNotice is how long after it is first read a notice from source that
// carries no time is assumed to take the node.
func AssumedNotice(source string) time.Duration {
	if d, ok := assumedNotice[source]; ok {
		return d
	}
	return defaultAssumedNotice
}

const (
	gkeMaintenanceTimeLabel = "cloud.google.com/scheduled-maintenance-time"
	aksScheduledCondition   = "VMEventScheduled"
)

// FromNode returns the interruptions announced on n. InstanceID carries the
// node's provider ID, as the Node object names its instance. A notice without
// a time has a zero Deadline: the signal says the interruption is under way
// or imminent, not when.
func FromNode(n Node) []Notice {
	var out []Notice
	add := func(kind cloud.InterruptionKind, source, key string, deadline time.Time) {
		out = append(out, Notice{Node: n.Name, Interruption: cloud.Interruption{
			InstanceID: n.ProviderID, Kind: kind, Deadline: deadline, Source: source,
			Key: source + "/" + n.Name + "/" + key,
		}})
	}
	for _, t := range n.Taints {
		if s, ok := taintSignals[t.Key]; ok {
			add(s.kind, s.source, t.Value, time.Time{})
		}
	}
	// GKE publishes scheduled host maintenance as the start time in epoch
	// seconds before the maintenance begins.
	if v := strings.TrimSpace(n.Labels[gkeMaintenanceTimeLabel]); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil && sec > 0 {
			add(cloud.InterruptionScheduledMaintenance, SourceGKEMaintenanceLabel, v, time.Unix(sec, 0).UTC())
		}
	}
	// AKS's node-problem-detector reports a pending Azure scheduled event as
	// condition VMEventScheduled; the condition carries no event time.
	for _, c := range n.Conditions {
		if c.Type == aksScheduledCondition && c.Status == "True" {
			add(cloud.InterruptionScheduledMaintenance, SourceAKSScheduledEvent, c.LastTransition.UTC().Format(time.RFC3339), time.Time{})
		}
	}
	return out
}
