// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrIdentityChanged requires two complete identities. Missing evidence must
// not be mistaken for proof that the target has changed.
var ErrIdentityChanged = errors.New("cloud node identity changed; fresh verification is required")

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
		return ErrIdentityChanged
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
	// Events are the node's recent events from the sources its provider names
	// (NodeEventsAware), within NodeEventWindow; source, reason and time only.
	// They prove a repair is under way, never that none is.
	Events []NodeEvent `json:"events,omitempty"`
	// EventsKnown is false when the events could not be read in full (a denied
	// or failed list, a truncated one): no events is then not "none happened".
	EventsKnown bool `json:"events_known"`
}

// NodeEvent is one Kubernetes event about a node, without its message: a
// message can name pods. A zero Time is an event whose recorder set no time;
// treat it as recent.
type NodeEvent struct {
	Source string    `json:"source"`
	Reason string    `json:"reason"`
	Time   time.Time `json:"time"`
}

// NodeEventWindow bounds how far back NodeRef.Events reaches.
const NodeEventWindow = time.Hour

// NodeEventsAware is implemented by a provider that judges native repair from
// node events. The platform reads events only for the sources it names, so a
// provider that needs none costs no event access.
type NodeEventsAware interface {
	NodeEventSources() []string
}

type RepairMode string

const (
	RepairUnknown RepairMode = "unknown"
	RepairOff     RepairMode = "off"
	RepairOn      RepairMode = "on"
	RepairVeto    RepairMode = "per-node-veto"
	// RepairNotReadyOnly: native repair cannot be turned off but acts only on a
	// NotReady node, so recovery is allowed only while the node stays Ready.
	RepairNotReadyOnly RepairMode = "not-ready-only"
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
	Mode   RepairMode `json:"mode"`
	Reason string     `json:"reason"`
	// Action, when set, tells an operator what would let recovery go ahead.
	Action   string           `json:"action,omitempty"`
	Evidence []RepairEvidence `json:"evidence,omitempty"`
	// Pool, when set, is the hold a pool assessment reports for this policy.
	Pool *RecoveryHold `json:"-"`
}

// Message is the operator-facing text: the reason, then the action.
func (p RepairPolicy) Message() string { return joinSentences(p.Reason, p.Action) }

// RecoveryHold is a provider's operator-facing reason for holding recovery:
// Reason says what is wrong, Action what to do or when the check passes.
// Neither carries a raw provider error; Cause does, for the controller log.
// PoolReason and PoolAction, when set, are the shorter texts of a pool row.
type RecoveryHold struct {
	Code       string
	Reason     string
	Action     string
	PoolReason string
	PoolAction string
	Cause      error
}

// Reason codes of a RecoveryHold, shared with pool assessments.
const (
	HoldNativeRepair     = "native-repair-on"
	HoldRecentChange     = "pool-changed-recently"
	HoldUpdateRunning    = "pool-update-running"
	HoldActivityRunning  = "pool-activity-running"
	HoldChangedDuringRun = "pool-changed-during-check"
	HoldUnconfirmed      = "repair-unconfirmed"
	HoldReadFailed       = "read-failed"
)

func (h *RecoveryHold) Message() string { return joinSentences(h.Reason, h.Action) }

// Transient reports a hold that clears without the operator: it says nothing
// about whether the pool can take add-first.
func (h *RecoveryHold) Transient() bool {
	switch h.Code {
	case HoldRecentChange, HoldUpdateRunning, HoldActivityRunning, HoldChangedDuringRun:
		return true
	}
	return false
}

func (h *RecoveryHold) Error() string {
	if h.Cause != nil {
		return h.Message() + ": " + h.Cause.Error()
	}
	return h.Message()
}

func (h *RecoveryHold) Unwrap() error { return h.Cause }

func joinSentences(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + " " + b
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
	CheckReplacePermission(ctx context.Context, instanceID string) error
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

// RecycleResult is evidence returned by a guarded recycle operation after the
// same Node and instance rejoin with a new boot. It is not recovery evidence
// for the accelerator or workload.
type RecycleResult struct {
	Before      NodeIdentity
	After       NodeIdentity
	CompletedAt time.Time
}

func (r RecycleResult) Validate(pin NodeIdentity) error {
	if err := pin.Match(r.Before); err != nil {
		return err
	}
	if err := r.After.Validate(); err != nil {
		return err
	}
	sameBoot := r.After
	sameBoot.BootID = pin.BootID
	if err := pin.Match(sameBoot); err != nil {
		return err
	}
	if r.After.BootID == pin.BootID || r.CompletedAt.IsZero() {
		return fmt.Errorf("recycle completion requires a new boot and completion time")
	}
	return nil
}
