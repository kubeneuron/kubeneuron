// Package ssh is the bare-metal fallback actuator, used when the
// kubeneuron-agent on a node is unreachable. It connects to Node.SSHAddr with
// the controller's SSH key and runs a fixed, allow-listed set of commands —
// never arbitrary input from playbook params.
//
// SKELETON: command execution via golang.org/x/crypto/ssh is Phase 2 work
// (the reboot playbook is Phase 2).
package ssh

import (
	"context"
	"fmt"

	"github.com/kubeneuron/kubeneuron/internal/actuator"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Actuator executes fallback actions over SSH.
type Actuator struct {
	// KeyPath is the controller's SSH private key.
	KeyPath string
	// User to connect as (needs sudo rules for the allow-listed commands).
	User string
}

var _ actuator.Actuator = (*Actuator)(nil)

// New builds an SSH actuator.
func New(user, keyPath string) *Actuator {
	return &Actuator{User: user, KeyPath: keyPath}
}

// Name implements actuator.Actuator.
func (a *Actuator) Name() string { return "ssh" }

// Capabilities implements actuator.Actuator. SSH handles only the coarse
// recovery actions needed when the agent is dead.
func (a *Actuator) Capabilities() []types.ActionType {
	return []types.ActionType{
		types.ActionReboot,
		types.ActionCollectBundle,
	}
}

// Execute implements actuator.Actuator.
func (a *Actuator) Execute(ctx context.Context, node types.Node, act types.Action) (*types.ActionResult, error) {
	if node.SSHAddr == "" {
		return nil, fmt.Errorf("ssh: node %s has no ssh_addr in inventory", node.Name)
	}
	// TODO(phase-2): dial node.SSHAddr, run the allow-listed command for
	// act.Type (reboot -> "sudo systemctl reboot"), honoring ctx deadline.
	return nil, fmt.Errorf("ssh: not implemented yet (skeleton)")
}

// Healthy implements actuator.Actuator.
func (a *Actuator) Healthy(ctx context.Context, node types.Node) error {
	if node.SSHAddr == "" {
		return fmt.Errorf("ssh: node %s has no ssh_addr in inventory", node.Name)
	}
	// TODO(phase-2): TCP dial + SSH handshake with a short deadline.
	return fmt.Errorf("ssh: not implemented yet (skeleton)")
}
