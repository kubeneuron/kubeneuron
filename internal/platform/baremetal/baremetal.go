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
	// cordoned maps a node to the set of remediations holding it down. On bare
	// metal "cordoned" is KubeNeuron-internal state unless a cordon hook
	// propagates it.
	//
	// A SET, not a flag, because a machine has several GPUs and two incidents
	// can be working two of them at once. With a flag, the first to finish
	// cleared it and the node went back into service while the other was still
	// resetting a GPU on it. The unowned Cordon/Uncordon below keep working by
	// holding one reserved owner, so a caller acting on the node rather than on
	// behalf of an incident still cannot release somebody else's hold.
	cordoned map[string]map[string]struct{}
}

var _ platform.Platform = (*Platform)(nil)
var _ platform.NodePresence = (*Platform)(nil)

// New builds a Platform from an inventory file (optional — pass "" to rely
// solely on agent self-registration) and hooks.
func New(inventoryPath string, hooks Hooks) (*Platform, error) {
	p := &Platform{
		nodes:    map[string]types.Node{},
		hooks:    hooks,
		cordoned: map[string]map[string]struct{}{},
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

// unownedCordonOwner is the holder recorded for a Cordon that named no owner.
// It is a real entry in the set, so an unowned cordon cannot be released by an
// incident's own release and vice versa.
const unownedCordonOwner = "kubeneuron:unowned"

// Cordon marks the node cordoned (KubeNeuron-internal) and runs the cordon
// hook when configured. Prefer CordonForOwner; this one holds the node under a
// single reserved owner.
func (p *Platform) Cordon(ctx context.Context, node string, reason string) error {
	return p.CordonForOwner(ctx, node, unownedCordonOwner, reason)
}

// Uncordon drops the unowned hold. It does NOT return a node another
// remediation is still holding — that was the whole defect.
func (p *Platform) Uncordon(ctx context.Context, node string) error {
	_, _, err := p.ReleaseCordonOwners(ctx, node, []string{unownedCordonOwner})
	return err
}

// CordonForOwner implements platform.Platform. The hook runs only when the node
// goes down, not once per holder: a hook is an operator's script and running it
// again for a node already cordoned is a side effect nobody asked for.
func (p *Platform) CordonForOwner(ctx context.Context, node, owner, reason string) error {
	if owner == "" {
		return fmt.Errorf("cordon of %s: an owner is required", node)
	}
	p.mu.Lock()
	owners, existed := p.cordoned[node]
	_, held := owners[owner]
	p.mu.Unlock()

	if existed {
		// The node is already down. Record the joiner and do not run the hook
		// again: a hook is an operator's script, and running it a second time
		// for a node already cordoned is a side effect nobody asked for.
		if !held {
			p.mu.Lock()
			p.cordoned[node][owner] = struct{}{}
			p.mu.Unlock()
		}
		return nil
	}

	// FIRST holder: the hook is what actually takes the node out of service, so
	// the owner is recorded only once it has succeeded.
	//
	// Recording first was wrong in the direction that matters. A failed hook
	// left the owner in the set, so the next incident to arrive took the
	// "already down" branch above, reported success, and never retried the
	// hook — and the ladder went on to drain and reset a node that had never
	// been removed from service at all.
	if err := p.runHook(ctx, p.hooks.CordonScript, "cordon", node); err != nil {
		return err
	}
	p.mu.Lock()
	if p.cordoned[node] == nil {
		p.cordoned[node] = map[string]struct{}{}
	}
	p.cordoned[node][owner] = struct{}{}
	p.mu.Unlock()
	return nil
}

// ReleaseCordonOwners implements platform.Platform: the node comes back only
// when the last holder leaves.
func (p *Platform) ReleaseCordonOwners(ctx context.Context, node string, owners []string) (bool, int, error) {
	p.mu.Lock()
	held, present := p.cordoned[node]
	if !present {
		p.mu.Unlock()
		return false, 0, nil // nothing holds it; releasing again is a no-op
	}
	for _, owner := range owners {
		delete(held, owner)
	}
	remaining := len(held)
	if remaining == 0 {
		delete(p.cordoned, node)
	}
	p.mu.Unlock()
	if remaining > 0 {
		return false, remaining, nil
	}
	return true, 0, p.runHook(ctx, p.hooks.CordonScript, "uncordon", node)
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
