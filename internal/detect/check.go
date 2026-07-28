// Package detect normalizes raw observations — agent XID events and
// Alertmanager alerts — into typed Signals that drive the incident state
// machine. All detection policy (which XID codes matter, which alert names
// map to which problem class) lives here, in one place.
package detect

import "github.com/kubeneuron/kubeneuron/pkg/types"

// Check matches one kind of raw input and turns it into a Signal.
// Implementations must be stateless; thresholds and dedup are handled by the
// incident state machine, not by checks.
type Check interface {
	// Name identifies the check in logs and metrics.
	Name() string
	// Match returns the normalized signal and true when the input is
	// something this check understands and considers actionable.
	Match(in SignalInput) (types.Signal, bool)
}

// SignalInput is a union of the raw inputs the controller receives. Exactly
// one field is non-nil.
type SignalInput struct {
	AgentEvent *types.AgentEvent
	Alert      *Alert
}
