// SPDX-License-Identifier: LicenseRef-KubeNeuron

package checkpoint

import (
	"fmt"
)

// Bounded gang coordination (docs/workload-safe-recovery-design.md, S3).
//
// A Pod may declare kubeneuron.io/checkpoint-group: <name>; the group is every
// Pod in the same namespace with the same value. When a disruption step on a
// node coordinates with a group member, the member's peers receive the same
// Phase 1 request — the same incident, their own deadline from the same
// formula — and nothing else. Disruption stays node-local: peers on other
// nodes are told, never evicted, their nodes never cordoned, and the step
// waits for them no longer than the same bounded window. There is no group
// leader, no aggregate state on any Pod, and no derivation of a group from an
// owner reference.

// Group policy bounds, mirroring the CRD.
const (
	DefaultMaxGroupSize = 64
	MaxGroupSizeCeiling = 512
)

// GroupPolicy is the compiled spec.safety.checkpointCoordination.groups. The
// zero value is off.
type GroupPolicy struct {
	Enabled bool
	// MaxGroupSize bounds a group; a larger one has its peers skipped (and
	// the step asks only the node-local members), counted and shown in the
	// preview. Zero on an enabled policy is filled with DefaultMaxGroupSize
	// by the configuration layers.
	MaxGroupSize int
	// CrossNode, when false (the default), confines the group to the members
	// on the disrupted node: lead time for local ranks, zero new blast
	// radius. True stamps peers on other nodes too, in allowlisted
	// namespaces only.
	CrossNode bool
}

func (g GroupPolicy) validate(coordinationEnabled bool) error {
	if !g.Enabled {
		return nil
	}
	if !coordinationEnabled {
		return fmt.Errorf("checkpoint coordination: groups.enabled requires checkpointCoordination.enabled")
	}
	if g.MaxGroupSize <= 0 {
		return fmt.Errorf("checkpoint coordination: groups.maxGroupSize must be positive, got %d", g.MaxGroupSize)
	}
	if g.MaxGroupSize > MaxGroupSizeCeiling {
		return fmt.Errorf("checkpoint coordination: groups.maxGroupSize %d exceeds the %d ceiling", g.MaxGroupSize, MaxGroupSizeCeiling)
	}
	return nil
}

// GroupKey identifies a group: namespace and name, which is what makes two
// Pods peers.
type GroupKey struct {
	Namespace string
	Name      string
}

func (k GroupKey) String() string { return k.Namespace + "/" + k.Name }

// GroupOf returns the group a workload declares, when the annotation is
// present and valid.
func GroupOf(w Workload) (GroupKey, bool) {
	name, invalid := w.Group()
	if invalid || name == "" {
		return GroupKey{}, false
	}
	return GroupKey{Namespace: w.Namespace, Name: name}, true
}

// The wait granted to a peer is Policy.BoundedWait, the very formula the
// member on the disrupted node gets, from the same request time: the group
// as a whole can never extend the window past min(maxWait, budget). A peer
// asking for less gets less; nothing a peer carries gets it more. There is
// deliberately no separate function, so the two cannot drift apart.
