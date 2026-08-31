// Package baremetal implements platform.Platform for nodes without an
// orchestrator. Inventory comes from a static YAML file
// (configs/inventory.yaml) merged with agent self-registrations; workload
// control is pluggable through operator-provided cordon and drain hook scripts
// (for example, scripts that remove a node from a load balancer or dispatcher).
// A Slurm implementation ("scontrol drain") later slots into the same
// Platform interface as a sibling package.
package baremetal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// Hooks configures operator-provided scripts for workload control.
type Hooks struct {
	// DrainScript is invoked as: <script> drain <node>. A zero exit means
	// the node's workloads are gone.
	DrainScript string `yaml:"drain_script,omitempty"`
	// CordonScript is invoked as: <script> cordon|uncordon <node>.
	CordonScript string `yaml:"cordon_script,omitempty"`
	// CordonStateFile durably records every cordon holder. It is required when
	// CordonScript is configured: losing the owner set after a controller
	// restart can return a node to service while another remediation still owns
	// it.
	CordonStateFile string `yaml:"cordon_state_file,omitempty"`
}

const (
	phaseCordoned    = "cordoned"
	phaseCordoning   = "cordoning"
	phaseUncordoning = "uncordoning"
)

type cordonState struct {
	owners     map[string]struct{}
	heldOwners map[string]struct{}
	reason     string
	phase      string
}

type persistedCordonState struct {
	Version uint                       `json:"version"`
	Cordons map[string]persistedCordon `json:"cordons"`
}

type persistedCordon struct {
	Owners     []string `json:"owners"`
	HeldOwners []string `json:"held_owners,omitempty"`
	Reason     string   `json:"reason"`
	// Phase makes a crash in the middle of a hook visible rather than guessing
	// that an external side effect did or did not happen.
	Phase string `json:"phase"`
}

// Platform implements platform.Platform for bare metal.
type Platform struct {
	nodesMu  sync.RWMutex
	nodes    map[string]types.Node
	hooks    Hooks
	cordonMu sync.Mutex
	// cordons maps a node to the set of remediations holding it down. On bare
	// metal it is persisted whenever a cordon hook is configured.
	//
	// A SET, not a flag, because a machine has several GPUs and two incidents
	// can be working two of them at once. With a flag, the first to finish
	// cleared it and the node went back into service while the other was still
	// resetting a GPU on it. The unowned Cordon/Uncordon below keep working by
	// holding one reserved owner, so a caller acting on the node rather than on
	// behalf of an incident still cannot release somebody else's hold.
	cordons map[string]*cordonState
}

var _ platform.Platform = (*Platform)(nil)
var _ platform.NodePresence = (*Platform)(nil)
var _ platform.CordonOwnership = (*Platform)(nil)
var _ platform.CordonJanitor = (*Platform)(nil)

// New builds a Platform from an inventory file (optional — pass "" to rely
// solely on agent self-registration) and hooks.
func New(inventoryPath string, hooks Hooks) (*Platform, error) {
	if hooks.CordonScript != "" && hooks.CordonStateFile == "" {
		return nil, fmt.Errorf("baremetal cordon hook requires cordon_state_file")
	}
	p := &Platform{
		nodes:   map[string]types.Node{},
		hooks:   hooks,
		cordons: map[string]*cordonState{},
	}
	if err := p.loadCordonState(); err != nil {
		return nil, err
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
	p.nodesMu.Lock()
	defer p.nodesMu.Unlock()
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
	p.nodesMu.RLock()
	defer p.nodesMu.RUnlock()
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
	p.nodesMu.RLock()
	defer p.nodesMu.RUnlock()
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
	p.cordonMu.Lock()
	defer p.cordonMu.Unlock()

	state, exists := p.cordons[node]
	if exists && state.phase == phaseCordoned {
		if _, held := state.owners[owner]; held {
			// Also retry a failed persistence write: a successful caller must
			// never be told the state is durable when it is not.
			return p.persistCordonStateLocked()
		}
		before := state.clone()
		state.owners[owner] = struct{}{}
		if err := p.persistCordonStateLocked(); err != nil {
			p.cordons[node] = before
			return err
		}
		return nil
	}

	// Serialize the complete read--write--hook sequence. A Cordon joining while
	// the last Release is uncordoning must re-cordon; otherwise the release can
	// hand the machine back after the new holder has been recorded.
	before := (*cordonState)(nil)
	if exists {
		before = state.clone()
	} else {
		state = &cordonState{owners: map[string]struct{}{}, heldOwners: map[string]struct{}{}}
		p.cordons[node] = state
	}
	state.owners[owner] = struct{}{}
	state.reason = reason
	state.phase = phaseCordoning
	if err := p.persistCordonStateLocked(); err != nil {
		p.restoreCordonLocked(node, before)
		return err
	}
	if err := p.runHook(ctx, p.hooks.CordonScript, "cordon", node); err != nil {
		p.restoreCordonLocked(node, before)
		if persistErr := p.persistCordonStateLocked(); persistErr != nil {
			return fmt.Errorf("%w (also restoring cordon journal: %v)", err, persistErr)
		}
		return err
	}
	state.phase = phaseCordoned
	if err := p.persistCordonStateLocked(); err != nil {
		// The physical cordon succeeded. Keep the in-memory holder and fail
		// closed; a retry will persist it instead of attempting an uncordon.
		return err
	}
	return nil
}

// ReleaseCordonOwners implements platform.Platform: the node comes back only
// when the last holder leaves.
func (p *Platform) ReleaseCordonOwners(ctx context.Context, node string, owners []string) (bool, int, error) {
	p.cordonMu.Lock()
	defer p.cordonMu.Unlock()

	state, present := p.cordons[node]
	if !present {
		return false, 0, nil // nothing holds it; releasing again is a no-op
	}
	before := state.clone()
	for _, owner := range owners {
		delete(state.owners, owner)
		delete(state.heldOwners, owner)
	}
	remaining := len(state.owners)
	if remaining > 0 {
		state.phase = phaseCordoned
		if err := p.persistCordonStateLocked(); err != nil {
			p.cordons[node] = before
			return false, len(before.owners), err
		}
		return false, remaining, nil
	}

	// Keep an explicit journal entry until the hook completes. A crash in this
	// interval must not look like an ordinary released node on restart.
	state.phase = phaseUncordoning
	if err := p.persistCordonStateLocked(); err != nil {
		p.cordons[node] = before
		return false, len(before.owners), err
	}
	if err := p.runHook(ctx, p.hooks.CordonScript, "uncordon", node); err != nil {
		p.cordons[node] = before
		if persistErr := p.persistCordonStateLocked(); persistErr != nil {
			return false, len(before.owners), fmt.Errorf("%w (also restoring cordon journal: %v)", err, persistErr)
		}
		return false, len(before.owners), err
	}
	delete(p.cordons, node)
	if err := p.persistCordonStateLocked(); err != nil {
		// The hook completed, but leaving an explicit unresolved record is
		// safer than pretending it did not. Startup will stop for human repair.
		p.cordons[node] = &cordonState{owners: map[string]struct{}{}, heldOwners: map[string]struct{}{}, reason: state.reason, phase: phaseUncordoning}
		return false, 0, err
	}
	return true, 0, nil
}

// Drain runs the drain hook. The standalone controller refuses to start a
// bare-metal deployment without one, so enabled automation cannot silently
// claim workloads were drained when no scheduler was contacted.
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

// CordonedNodes implements platform.CordonJanitor. The state is local rather
// than a Kubernetes annotation, so the durable journal is the authority after
// a controller restart.
func (p *Platform) CordonedNodes(ctx context.Context) ([]platform.CordonedNode, error) {
	p.cordonMu.Lock()
	defer p.cordonMu.Unlock()
	out := make([]platform.CordonedNode, 0, len(p.cordons))
	for node, state := range p.cordons {
		if len(state.owners) == 0 {
			continue
		}
		out = append(out, platform.CordonedNode{
			Name: node, Reason: state.reason,
			Owners: sortedSet(state.owners), HeldOwners: sortedSet(state.heldOwners),
		})
	}
	return out, nil
}

// UncordonIfReason is only meaningful for an untracked legacy cordon. Every
// bare-metal cordon has an owner in the journal, so the controller releases it
// through ReleaseCordonOwners instead.
func (p *Platform) UncordonIfReason(context.Context, string, string) (bool, error) {
	return false, nil
}

// MarkCordonHeldIfReason is likewise for an untracked legacy cordon. Returning
// false makes a future caller retry rather than claiming a node-wide handoff.
func (p *Platform) MarkCordonHeldIfReason(context.Context, string, string) (bool, error) {
	return false, nil
}

// MarkCordonHeldIfOwner persistently hands one exact cordon holder to a human.
func (p *Platform) MarkCordonHeldIfOwner(_ context.Context, node, owner string) (bool, error) {
	p.cordonMu.Lock()
	defer p.cordonMu.Unlock()
	state, ok := p.cordons[node]
	if !ok {
		return false, nil
	}
	if _, ok := state.owners[owner]; !ok {
		return false, nil
	}
	if _, held := state.heldOwners[owner]; held {
		return true, nil
	}
	state.heldOwners[owner] = struct{}{}
	if err := p.persistCordonStateLocked(); err != nil {
		delete(state.heldOwners, owner)
		return false, err
	}
	return true, nil
}

func (p *Platform) loadCordonState() error {
	if p.hooks.CordonStateFile == "" {
		return nil
	}
	data, err := os.ReadFile(p.hooks.CordonStateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading baremetal cordon journal: %w", err)
	}
	var persisted persistedCordonState
	if err := json.Unmarshal(data, &persisted); err != nil {
		return fmt.Errorf("decoding baremetal cordon journal: %w", err)
	}
	if persisted.Version != 1 {
		return fmt.Errorf("unsupported baremetal cordon journal version %d", persisted.Version)
	}
	for node, record := range persisted.Cordons {
		if record.Phase != phaseCordoned {
			return fmt.Errorf("baremetal cordon journal for %s is %s; verify the cordon hook result and repair the journal before starting", node, record.Phase)
		}
		owners := make(map[string]struct{}, len(record.Owners))
		for _, owner := range record.Owners {
			if owner != "" {
				owners[owner] = struct{}{}
			}
		}
		if len(owners) == 0 {
			return fmt.Errorf("baremetal cordon journal for %s has no owners", node)
		}
		held := make(map[string]struct{}, len(record.HeldOwners))
		for _, owner := range record.HeldOwners {
			if _, ok := owners[owner]; ok {
				held[owner] = struct{}{}
			}
		}
		p.cordons[node] = &cordonState{owners: owners, heldOwners: held, reason: record.Reason, phase: record.Phase}
	}
	return nil
}

func (p *Platform) persistCordonStateLocked() error {
	if p.hooks.CordonStateFile == "" {
		return nil
	}
	persisted := persistedCordonState{Version: 1, Cordons: make(map[string]persistedCordon, len(p.cordons))}
	for node, state := range p.cordons {
		persisted.Cordons[node] = persistedCordon{
			Owners: sortedSet(state.owners), HeldOwners: sortedSet(state.heldOwners),
			Reason: state.reason, Phase: state.phase,
		}
	}
	data, err := json.Marshal(persisted)
	if err != nil {
		return fmt.Errorf("encoding baremetal cordon journal: %w", err)
	}
	path := p.hooks.CordonStateFile
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating baremetal cordon journal directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("creating baremetal cordon journal: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing baremetal cordon journal: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing baremetal cordon journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing baremetal cordon journal: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("installing baremetal cordon journal: %w", err)
	}
	// Rename makes readers atomic; syncing the directory makes that rename
	// survive a power loss as well. Without it a reboot can resurrect the old
	// owner set after a successful hook, the same class of lost-hold bug this
	// journal exists to prevent.
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening baremetal cordon journal directory: %w", err)
	}
	defer dirFile.Close()
	if err := dirFile.Sync(); err != nil {
		return fmt.Errorf("syncing baremetal cordon journal directory: %w", err)
	}
	return nil
}

func (p *Platform) restoreCordonLocked(node string, state *cordonState) {
	if state == nil {
		delete(p.cordons, node)
		return
	}
	p.cordons[node] = state
}

func (s *cordonState) clone() *cordonState {
	if s == nil {
		return nil
	}
	return &cordonState{owners: cloneSet(s.owners), heldOwners: cloneSet(s.heldOwners), reason: s.reason, phase: s.phase}
}

func cloneSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for value := range in {
		out[value] = struct{}{}
	}
	return out
}

func sortedSet(in map[string]struct{}) []string {
	out := make([]string, 0, len(in))
	for value := range in {
		out = append(out, value)
	}
	slices.Sort(out)
	return out
}
