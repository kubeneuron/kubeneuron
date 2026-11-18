// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// InterruptionKind is what the cloud announced it will do to an instance.
type InterruptionKind string

const (
	// InterruptionSpotReclaim: a spot or preemptible instance is being taken
	// back; it stops or terminates at the deadline.
	InterruptionSpotReclaim InterruptionKind = "spot-reclaim"
	// InterruptionRebalance: the cloud says a spot instance is at raised risk
	// of being reclaimed. It is a hint; the instance may run on.
	InterruptionRebalance InterruptionKind = "rebalance"
	// InterruptionScheduledMaintenance: a planned event on the instance
	// (reboot, retirement, stop) with a window the cloud published in advance.
	InterruptionScheduledMaintenance InterruptionKind = "scheduled-maintenance"
	// InterruptionHostMaintenance: the host under the instance is about to be
	// serviced; a GPU instance is stopped or restarted rather than moved.
	InterruptionHostMaintenance InterruptionKind = "host-maintenance"
)

// Valid reports whether k is one of the kinds above.
func (k InterruptionKind) Valid() bool {
	switch k {
	case InterruptionSpotReclaim, InterruptionRebalance, InterruptionScheduledMaintenance, InterruptionHostMaintenance:
		return true
	}
	return false
}

// Interruption is one announcement by the cloud that it will take or disturb
// an instance. Only the facts below are kept: the cloud's own message text is
// never stored or logged.
type Interruption struct {
	// InstanceID is the provider's instance, as Provider.InstanceID returns it.
	InstanceID string           `json:"instance_id"`
	Kind       InterruptionKind `json:"kind"`
	// Deadline is when the cloud says it will act; zero when the notice
	// gives no time (a rebalance hint).
	Deadline time.Time `json:"deadline,omitempty"`
	// Source names where the notice was read, such as a node taint or an
	// event queue; it is shown with the incident.
	Source string `json:"source"`
	// Key identifies the notice at its source, so the same notice read twice
	// is recognised. Two different notices for one instance have different
	// keys.
	Key string `json:"key"`
	// Once is set when the source delivers the notice once rather than for
	// as long as it is in force, as an event queue does: its absence from a
	// later read says nothing about the interruption.
	Once bool `json:"once,omitempty"`
	// At is when the cloud sent the notice, when the source says; a queue
	// may deliver notices out of order.
	At time.Time `json:"at,omitempty"`
	// Ended is set when the source reports that an interruption is over.
	// With a Kind it ends the notice of the same Key (the maintenance
	// event closed). Without one it says the instance is running again,
	// which ends the instance's maintenance notices sent before At, never a
	// spot reclaim or a rebalance hint.
	Ended bool `json:"ended,omitempty"`
}

// Validate checks the fields the controller relies on.
func (i Interruption) Validate() error {
	switch {
	case i.InstanceID == "":
		return errors.New("interruption: instance ID is empty")
	case !i.Kind.Valid() && (!i.Ended || i.Kind != ""):
		return fmt.Errorf("interruption: unknown kind %q", i.Kind)
	case i.Source == "":
		return errors.New("interruption: source is empty")
	case i.Key == "":
		return errors.New("interruption: key is empty")
	}
	return nil
}

// InterruptionAware is implemented by a provider that can tell which of the
// given nodes the cloud has announced it will interrupt. The controller polls
// it; a notice is reported for as long as the provider still sees it, and the
// controller recognises repeats by Key. A provider that does not implement it
// reports no interruptions, which is never read as "none announced".
type InterruptionAware interface {
	// Interruptions returns the announced interruptions among nodes, in any
	// order. An error is a failed read: the controller keeps what it already
	// knows and asks again on the next poll.
	Interruptions(ctx context.Context, nodes []NodeRef) ([]Interruption, error)
}

// InterruptionQueue is implemented by an InterruptionAware provider whose
// source is a queue: it delivers each notice once (Interruption.Once) and
// forgets it only when told to. The controller receives from it in a loop of
// its own while it leads, and acknowledges a notice once it is durably
// recorded, so a notice is not lost to a restart between the two.
type InterruptionQueue interface {
	InterruptionAware
	// ReceiveInterruptions waits up to the source's long-poll time for new
	// notices and keeps them for Interruptions. It reports whether any
	// arrived. A message it cannot read is removed from the source at once.
	ReceiveInterruptions(ctx context.Context) (bool, error)
	// AckInterruption tells the source that the notice with key needs
	// nothing more, so the source may delete it. A notice never
	// acknowledged is delivered again.
	AckInterruption(ctx context.Context, key string) error
}
