// Package agentrpc is the primary actuator: it dispatches actions to the
// kubeneuron-agent on the target node through the durable store-backed work
// queue. The agent polls for work over its existing authenticated mTLS
// channel and posts results back — no per-node server, listener, or
// certificate is required, and the trust model stays exactly the proven
// agent->controller one. The gRPC push contract in
// api/proto/agent/v1/agent.proto remains a possible future low-latency
// transport.
package agentrpc

import (
	"context"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/actuator"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	// resultPollInterval is how often Execute checks for a posted result.
	resultPollInterval = 500 * time.Millisecond
	// defaultStaleAfter mirrors the agent registration staleness window.
	defaultStaleAfter = 90 * time.Second
)

// Actuator dispatches actions to kubeneuron-agent instances via the queue.
type Actuator struct {
	store      store.Store
	staleAfter time.Duration
	now        func() time.Time
}

var _ actuator.Actuator = (*Actuator)(nil)

// New builds an agent actuator on the given store. staleAfter bounds how old
// the node's last heartbeat may be for Healthy; zero uses the default 90s.
func New(st store.Store, staleAfter time.Duration) *Actuator {
	if staleAfter <= 0 {
		staleAfter = defaultStaleAfter
	}
	return &Actuator{store: st, staleAfter: staleAfter, now: time.Now}
}

// Name implements actuator.Actuator.
func (a *Actuator) Name() string { return "agent" }

// Capabilities implements actuator.Actuator. The agent can do everything
// except out-of-band power control.
func (a *Actuator) Capabilities() []types.ActionType {
	return []types.ActionType{
		types.ActionIdleCheck,
		types.ActionWaitIdle,
		types.ActionGPUReset,
		types.ActionRunDiag,
		types.ActionCollectBundle,
		types.ActionReboot,
		types.ActionDriverReload,
		types.ActionDriverReinstall,
		types.ActionRunScript,
	}
}

// Execute enqueues the action for the node's agent and waits for its result.
// Enqueue is idempotent on the deterministic action ID: a replay after a
// controller restart re-attaches to the same queued action, and an
// already-completed one returns its stored result immediately.
func (a *Actuator) Execute(ctx context.Context, node types.Node, act types.Action) (*types.ActionResult, error) {
	if node.Name == "" {
		return nil, fmt.Errorf("agentrpc: node name is required")
	}
	if err := a.store.EnqueueAction(ctx, node.Name, act.Params["incident_id"], act); err != nil {
		return nil, fmt.Errorf("agentrpc: enqueue: %w", err)
	}
	tick := time.NewTicker(resultPollInterval)
	defer tick.Stop()
	for {
		queued, err := a.store.GetAction(ctx, act.ID)
		if err != nil {
			return nil, fmt.Errorf("agentrpc: reading action state: %w", err)
		}
		if queued.Cancelled {
			// The incident moved on (escalation or quarantine) before any
			// agent claimed this entry; fail the step without executing.
			return nil, fmt.Errorf("agentrpc: %s on %s was cancelled before delivery", act.Type, node.Name)
		}
		if queued.Done && queued.Result != nil {
			if !queued.Result.OK {
				return queued.Result, fmt.Errorf("agentrpc: %s on %s failed: %s",
					act.Type, node.Name, queued.Result.Error)
			}
			return queued.Result, nil
		}
		select {
		case <-ctx.Done():
			// The action stays queued; a replay with the same ID re-attaches
			// and the agent-side idempotency cache prevents double execution.
			return nil, fmt.Errorf("agentrpc: waiting for %s on %s: %w", act.Type, node.Name, ctx.Err())
		case <-tick.C:
		}
	}
}

// Healthy reports whether the node's agent heartbeat is current.
func (a *Actuator) Healthy(ctx context.Context, node types.Node) error {
	n, err := a.store.GetNode(ctx, node.Name)
	if err != nil {
		return fmt.Errorf("agentrpc: node %s unknown: %w", node.Name, err)
	}
	if n.AgentLastSeen.IsZero() {
		return fmt.Errorf("agentrpc: agent on %s has never registered", node.Name)
	}
	if age := a.now().Sub(n.AgentLastSeen); age > a.staleAfter {
		return fmt.Errorf("agentrpc: agent heartbeat on %s is stale (%s old)", node.Name, age.Round(time.Second))
	}
	return nil
}
