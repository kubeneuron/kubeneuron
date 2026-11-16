// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"fmt"
	"time"
)

// NodeIdentity pins an incident to one Kubernetes object, instance and boot.
// Empty fields are not wildcards. A caller must not replace a missing snapshot
// with the current identity after an approval has been granted.
type NodeIdentity struct {
	Node       string `json:"node"`
	NodeUID    string `json:"node_uid"`
	ProviderID string `json:"provider_id"`
	InstanceID string `json:"instance_id"`
	BootID     string `json:"boot_id"`
}

func (n NodeIdentity) Validate() error {
	if n.Node == "" || n.NodeUID == "" || n.ProviderID == "" || n.InstanceID == "" || n.BootID == "" {
		return fmt.Errorf("cloud identity is incomplete")
	}
	return nil
}

// Match includes boot identity: a reboot invalidates evidence even when the
// instance survives. It does not assert that a mismatch was caused by cloud
// repair; attribution requires independent evidence.
func (n NodeIdentity) Match(current NodeIdentity) error {
	if err := n.Validate(); err != nil {
		return fmt.Errorf("pinned identity: %w", err)
	}
	if err := current.Validate(); err != nil {
		return fmt.Errorf("current identity: %w", err)
	}
	if n != current {
		return fmt.Errorf("cloud node identity changed; fresh verification is required")
	}
	return nil
}

// NodeRef is a live observation. Labels and annotations aid discovery only;
// providers must verify control-plane facts instead of trusting mutable labels.
type NodeRef struct {
	Identity    NodeIdentity      `json:"identity"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Ready       bool              `json:"ready"`
}

type RepairMode string

const (
	RepairUnknown RepairMode = "unknown"
	RepairOff     RepairMode = "off"
	RepairOn      RepairMode = "on"
	RepairVeto    RepairMode = "per-node-veto"
)

// RepairEvidence records the source and observation time of a verdict, not
// credentials or unfiltered provider responses. A verdict is not a lease:
// callers must re-read it immediately before disruptive work.
type RepairEvidence struct {
	Source     string    `json:"source"`
	Resource   string    `json:"resource"`
	Detail     string    `json:"detail"`
	ObservedAt time.Time `json:"observed_at"`
}

type RepairPolicy struct {
	Mode     RepairMode       `json:"mode"`
	Reason   string           `json:"reason"`
	Evidence []RepairEvidence `json:"evidence,omitempty"`
}

// RepairObservation distinguishes an unknown result from a confirmed absence
// of repair. Neither an empty event list nor a running VM proves Known=true.
type RepairObservation struct {
	Known    bool             `json:"known"`
	Active   bool             `json:"active"`
	Replaced bool             `json:"replaced"`
	Reason   string           `json:"reason"`
	Evidence []RepairEvidence `json:"evidence,omitempty"`
}

// RepairAwareProvider is additive to Provider. A legacy provider can still be
// built, but cannot authorize controlled recovery without this contract.
type RepairAwareProvider interface {
	Provider
	RepairPolicy(context.Context, NodeRef) (RepairPolicy, error)
	OpenRepair(context.Context, NodeRef) (RepairObservation, error)
}

// RecoveryEligibility is shared by first-run status and the execution gate.
// ReplaceAllowed is required before cordon/drain. RecycleAllowed=false with
// ReplaceAllowed=true means skip recycle, not abandon the replacement path.
// These booleans require both capability and current authorization evidence.
type RecoveryEligibility struct {
	Identity       NodeIdentity `json:"identity"`
	Mode           RepairMode   `json:"mode"`
	Reason         string       `json:"reason"`
	RecycleAllowed bool         `json:"recycle_allowed"`
	ReplaceAllowed bool         `json:"replace_allowed"`
}
