// Package baremetal implements platform.Platform for nodes without an
// orchestrator. Inventory comes from a static YAML file
// (configs/inventory.yaml) merged with agent self-registrations; workload
// control is pluggable — "noop" (log only) or an operator-provided drain
// hook script (e.g. remove the node from a load balancer or job dispatcher).
// A Slurm implementation ("scontrol drain") later slots into the same
// Platform interface as a sibling package.
package baremetal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Inventory is the on-disk format of configs/inventory.yaml.
type Inventory struct {
	Nodes []InventoryNode `yaml:"nodes"`
}

// InventoryNode describes one bare-metal node.
type InventoryNode struct {
	Name    string            `yaml:"name"`
	SSHAddr string            `yaml:"ssh_addr,omitempty"`
	BMCAddr string            `yaml:"bmc_addr,omitempty"`
	Labels  map[string]string `yaml:"labels,omitempty"`
	Paused  bool              `yaml:"paused,omitempty"`
}

// Hooks configures the optional operator-provided scripts for workload
// control. Empty fields mean noop.
type Hooks struct {
	// DrainScript is invoked as: <script> drain <node>. A zero exit means
	// the node's workloads are gone.
	DrainScript string `yaml:"drain_script,omitempty"`
	// CordonScript is invoked as: <script> cordon|uncordon <node>.
	CordonScript string `yaml:"cordon_script,omitempty"`
}

// Platform implements platform.Platform for bare metal.
type Platform struct {
	mu    sync.RWMutex
	nodes map[string]types.Node
	hooks Hooks
	// cordoned tracks nodes KubeNeuron has cordoned; on bare metal "cordoned"
	// is KubeNeuron-internal state unless a cordon hook propagates it.
	cordoned map[string]bool
}

var _ platform.Platform = (*Platform)(nil)
var _ platform.NodePresence = (*Platform)(nil)

// New builds a Platform from an inventory file (optional — pass "" to rely
// solely on agent self-registration) and hooks.
func New(inventoryPath string, hooks Hooks) (*Platform, error) {
	p := &Platform{
		nodes:    map[string]types.Node{},
		hooks:    hooks,
		cordoned: map[string]bool{},
	}
	if inventoryPath != "" {
		data, err := os.ReadFile(inventoryPath)
		if err != nil {
			return nil, err
		}
		var inv Inventory
		if err := yaml.Unmarshal(data, &inv); err != nil {
			return nil, fmt.Errorf("%s: %w", inventoryPath, err)
		}
		for _, n := range inv.Nodes {
			p.nodes[n.Name] = types.Node{
				Name:     n.Name,
				Platform: "baremetal",
				Labels:   n.Labels,
				SSHAddr:  n.SSHAddr,
				BMCAddr:  n.BMCAddr,
				Paused:   n.Paused,
			}
		}
	}
	return p, nil
}

// Register merges an agent self-registration into the inventory. Called by
// the controller when an agent POSTs the narrow registration endpoint.
func (p *Platform) Register(n types.Node) {
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, ok := p.nodes[n.Name]
	if !ok {
		n.Platform = "baremetal"
		p.nodes[n.Name] = n
		return
	}
	// Agent data wins for dynamic fields; inventory wins for addresses.
	existing.GPUs = n.GPUs
	existing.BootID = n.BootID
	existing.AgentLastSeen = n.AgentLastSeen
	p.nodes[n.Name] = existing
}

// Name implements platform.Platform.
func (p *Platform) Name() string { return "baremetal" }

// ListNodes returns the merged inventory.
func (p *Platform) ListNodes(ctx context.Context) ([]types.Node, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]types.Node, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, n)
	}
	return out, nil
}

// NodeExists answers from the merged declared/registered inventory. Bare-metal
// has no independent node API, so an unknown node is the strongest presence
// signal it can provide.
func (p *Platform) NodeExists(ctx context.Context, node string) (bool, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.nodes[node]
	return ok, nil
}

// WatchNodes returns a channel that closes on ctx.Done(); bare metal has no
// native watch — registration updates arrive via Register.
func (p *Platform) WatchNodes(ctx context.Context) (<-chan platform.NodeEvent, error) {
	ch := make(chan platform.NodeEvent)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

// Cordon marks the node cordoned (KubeNeuron-internal) and runs the cordon
// hook when configured.
func (p *Platform) Cordon(ctx context.Context, node string, reason string) error {
	p.mu.Lock()
	p.cordoned[node] = true
	p.mu.Unlock()
	return p.runHook(ctx, p.hooks.CordonScript, "cordon", node)
}

// Uncordon reverses Cordon.
func (p *Platform) Uncordon(ctx context.Context, node string) error {
	p.mu.Lock()
	delete(p.cordoned, node)
	p.mu.Unlock()
	return p.runHook(ctx, p.hooks.CordonScript, "uncordon", node)
}

// Drain runs the drain hook, or no-ops with success when none is configured
// (the operator accepted noop drains in their deployment).
func (p *Platform) Drain(ctx context.Context, node string, opts platform.DrainOptions) error {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	return p.runHook(ctx, p.hooks.DrainScript, "drain", node)
}

// NodeWorkloads is unknown on plain bare metal; returns empty.
func (p *Platform) NodeWorkloads(ctx context.Context, node string) ([]platform.Workload, error) {
	return nil, nil
}

// EvictWorkload is not supported on plain bare metal.
func (p *Platform) EvictWorkload(ctx context.Context, w platform.Workload) error {
	return fmt.Errorf("baremetal: targeted workload eviction is not supported without an orchestrator")
}

func (p *Platform) runHook(ctx context.Context, script, verb, node string) error {
	if script == "" {
		return nil // noop by configuration
	}
	cmd := exec.CommandContext(ctx, script, verb, node)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("hook %s %s %s: %w: %s", script, verb, node, err, out)
	}
	return nil
}
