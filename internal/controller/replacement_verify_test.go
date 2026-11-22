// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/internal/topology"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const poolNodeName = "n10"

// poolPlatform is a successorPlatform whose nodes carry an EKS nodegroup
// label, and whose pool gains a node once the target is removed: the target
// is then gone from the cluster.
type poolPlatform struct {
	*successorPlatform
	// members are the pool's nodes besides the target; added on removal.
	members []platform.SuccessorNode
	// extra is how many more new nodes the pool gains on removal.
	extra int
	// sameName recreates the instance under the target's name, with a new
	// UID, as a managed instance group can.
	sameName bool
	// resource is what the new nodes and the successor advertise; nvidia.com/gpu when empty.
	resource string
	removed  bool
}

func (p *poolPlatform) CloudNode(ctx context.Context, node string) (cloud.NodeRef, error) {
	ref, err := p.successorPlatform.CloudNode(ctx, node)
	ref.Labels = map[string]string{"eks.amazonaws.com/nodegroup": "train"}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.removed && p.sameName && node == "n1" {
		ref.Identity.NodeUID, ref.Identity.InstanceID, ref.Identity.BootID = "uid-n1-new", "i-n1-new", "boot-n1-new"
		ref.Identity.ProviderID = "aws:///zone/i-n1-new"
	}
	return ref, err
}

func (p *poolPlatform) NodeExists(_ context.Context, node string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return node != "n1" || !p.removed || p.sameName, nil
}

func (p *poolPlatform) ReplacePinnedNode(ctx context.Context, pin cloud.NodeIdentity) error {
	if err := p.successorPlatform.ReplacePinnedNode(ctx, pin); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removed = true
	resource := cmp.Or(p.resource, topology.ResourceWholeGPU)
	for i := 0; i <= p.extra; i++ {
		name, uid := poolNodeName, "uid-"+poolNodeName
		if i > 0 {
			name += strings.Repeat("x", i)
			uid = "uid-" + name
		} else if p.sameName {
			name, uid = "n1", "uid-n1-new"
		}
		// The cluster's clock: a new node may carry a creation time before the
		// controller recorded the pool.
		p.members = append(p.members, platform.SuccessorNode{Found: true, Name: name, UID: uid,
			CreatedAt: time.Now().Add(-time.Minute), Ready: p.nodeReady,
			Allocatable: map[string]int64{resource: p.successorAllocatable}})
	}
	return nil
}

func (p *poolPlatform) SuccessorNode(ctx context.Context, providerID string) (platform.SuccessorNode, error) {
	n, err := p.successorPlatform.SuccessorNode(ctx, providerID)
	if p.resource != "" && n.Allocatable != nil {
		n.Allocatable = map[string]int64{p.resource: n.Allocatable[topology.ResourceWholeGPU]}
	}
	return n, err
}

func (p *poolPlatform) NodeResources(ctx context.Context, node string) (topology.Resources, bool, error) {
	r, ok, err := p.successorPlatform.NodeResources(ctx, node)
	if p.resource != "" && r.Capacity != nil {
		n := r.Capacity[topology.ResourceWholeGPU]
		r = topology.Resources{Capacity: map[string]int64{p.resource: n}, Allocatable: map[string]int64{p.resource: n}}
	}
	return r, ok, err
}

func (p *poolPlatform) PoolNodes(_ context.Context, label, pool string) ([]platform.SuccessorNode, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if label != "eks.amazonaws.com/nodegroup" || pool != "train" {
		return nil, nil
	}
	out := []platform.SuccessorNode{{Found: true, Name: "n2", UID: "uid-n2", CreatedAt: time.Now().Add(-time.Hour), Ready: true}}
	if !p.removed {
		out = append(out, platform.SuccessorNode{Found: true, Name: "n1", UID: "uid-n1", CreatedAt: time.Now().Add(-time.Hour), Ready: true})
	}
	return append(out, p.members...), nil
}

// reportFrom has the agent on node post its accelerator report now.
func reportFrom(t *testing.T, st *sqlite.Store, node, uid string, vendor types.AcceleratorVendor, readiness types.AcceleratorReadiness) {
	t.Helper()
	report := readyNVIDIAResetReport(time.Now(), "profile")
	report.Node, report.NodeUID, report.Vendor, report.Readiness = node, uid, vendor, readiness
	report.Devices[0].ID = "GPU-new-0"
	if readiness == types.AcceleratorReadinessNotReady {
		report.ReadinessReasons = []string{"driver not loaded"}
	}
	if vendor == types.AcceleratorVendorAWS {
		report.Devices = []types.AgentAcceleratorDevice{{ID: "neuron0", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyNeuron}}
		report.DriverVersion, report.RuntimeVersion = "", ""
		report.TopologySafety = types.AcceleratorTopologyUnknown
		report.Capabilities = []types.AgentAcceleratorCapability{}
		report.ReadinessReasons = []string{"device presence observed; runtime not attested"}
	}
	if err := st.UpsertAcceleratorReport(context.Background(), &report); err != nil {
		t.Fatal(err)
	}
}

// openVendorIncident opens a node-scoped incident whose fault names vendor.
func openVendorIncident(t *testing.T, c *Controller, node string, vendor types.AcceleratorVendor) string {
	t.Helper()
	sig := signal(types.ClassFellOffBus, node, "")
	if vendor != "" {
		sig.Evidence["vendor"] = string(vendor)
	}
	if err := c.ingest(context.Background(), sig); err != nil {
		t.Fatal(err)
	}
	inc := latestIncident(t, c, node)
	if inc.Vendor != vendor {
		t.Fatalf("vendor = %q, want %q", inc.Vendor, vendor)
	}
	return inc.ID
}

func latestIncident(t *testing.T, c *Controller, node string) *types.Incident {
	t.Helper()
	ctx := context.Background()
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{Node: node})
	if err != nil || len(incidents) == 0 {
		t.Fatalf("no incident opened on %s: %v", node, err)
	}
	return incidents[0]
}

// untilState reconciles, approving every round, until done.
func untilState(t *testing.T, c *Controller, st *sqlite.Store, id string, done func(*types.Incident) bool) *types.Incident {
	t.Helper()
	return waitSettled(t, c, st, id, func(inc *types.Incident) bool {
		if inc.State == types.StateAwaitingApproval {
			if err := c.DecideApproval(context.Background(), id, "bob", "test", types.ApprovalApproved, inc.ApprovalEpoch, "ok", ""); err != nil {
				t.Fatal(err)
			}
			return false
		}
		return done(inc)
	})
}

func resolveEntry(entries []*types.AuditEntry) *types.AuditEntry {
	for i := len(entries) - 1; i >= 0; i-- {
		if e := entries[i]; e.Action == "resolve" && e.ToState == types.StateResolved {
			return e
		}
	}
	return nil
}

// A cloud replacement resolves on the node that took the target's place: the
// successor the cloud operation named (add-first), or the only node that
// joined the target's pool after the removal (remove-first). The resolution
// names the node, how it was found and what was confirmed, at the depth the
// accelerator allows; the notification names it too, the incident keeps its
// target, and the report counts it recovered with the degraded time ending
// at the confirmation. The old node being gone, or recreated under its name,
// never supersedes the incident first.
func TestReplacementResolvesOnTheNewNode(t *testing.T) {
	const (
		successorSentence = "Replaced by node n9, named by the cloud operation."
		poolSentence      = "Node n10 is Ready. It was the only node that joined pool train after the removal."
		driverDepth       = "Fresh accelerator inventory and driver health observed."
		observedDepth     = "Device presence observed; runtime not attested."
		capacityDepth     = "Verification used Kubernetes node readiness and advertised accelerator capacity."
	)
	for _, tc := range []struct {
		name     string
		mode     recoverymode.Mode
		strategy recoverymode.Strategy
		vendor   types.AcceleratorVendor
		setup    func(*testing.T, *sqlite.Store, *poolPlatform)
		noAgent  bool
		// blind has the old node's agent register no devices, as one that
		// does not inventory its vendor's devices.
		blind    bool
		heard    bool
		node     string
		uid      string
		how      string
		sentence string
		depth    string
	}{
		{name: "add-first nvidia", mode: recoverymode.Automatic, vendor: types.AcceleratorVendorNVIDIA,
			node: successorNodeName, how: replacementBySuccessor, sentence: successorSentence, depth: driverDepth},
		{name: "add-first aws approval", mode: recoverymode.Approval, vendor: types.AcceleratorVendorAWS,
			node: successorNodeName, how: replacementBySuccessor, sentence: successorSentence, depth: observedDepth},
		{name: "remove-first nvidia approval", mode: recoverymode.Approval, strategy: recoverymode.RemoveFirst, vendor: types.AcceleratorVendorNVIDIA,
			node: poolNodeName, how: replacementByPool, sentence: poolSentence, depth: driverDepth},
		{name: "remove-first aws", mode: recoverymode.Automatic, strategy: recoverymode.RemoveFirst, vendor: types.AcceleratorVendorAWS,
			node: poolNodeName, how: replacementByPool, sentence: poolSentence, depth: observedDepth},
		// No report comes for AMD; the successor's agent, heard before the old
		// node is removed, is all the evidence there is.
		{name: "add-first amd approval", mode: recoverymode.Approval, vendor: types.AcceleratorVendorAMD,
			setup: func(_ *testing.T, _ *sqlite.Store, p *poolPlatform) { p.resource = "amd.com/gpu" },
			node:  successorNodeName, how: replacementBySuccessor, sentence: successorSentence, depth: capacityDepth, heard: true},
		{name: "remove-first amd", mode: recoverymode.Automatic, strategy: recoverymode.RemoveFirst, vendor: types.AcceleratorVendorAMD,
			setup: func(_ *testing.T, _ *sqlite.Store, p *poolPlatform) { p.resource = "amd.com/gpu" },
			node:  poolNodeName, how: replacementByPool, sentence: poolSentence, depth: capacityDepth},
		// The incident records no accelerators; the old node's advertised
		// capacity, recorded with the pool, is the base.
		{name: "remove-first amd, agent without devices", mode: recoverymode.Automatic, strategy: recoverymode.RemoveFirst, vendor: types.AcceleratorVendorAMD,
			setup: func(_ *testing.T, _ *sqlite.Store, p *poolPlatform) { p.resource = "amd.com/gpu" }, blind: true,
			node: poolNodeName, how: replacementByPool, sentence: poolSentence, depth: capacityDepth},
		{name: "remove-first tpu node-health without an agent", mode: recoverymode.Automatic, strategy: recoverymode.RemoveFirst, noAgent: true,
			setup: func(t *testing.T, st *sqlite.Store, p *poolPlatform) {
				p.resource = "google.com/tpu"
				p.successorAllocatable = 4
				if err := st.UpsertKubernetesInventory(context.Background(), &types.Node{Name: "n1", UID: "uid-n1",
					Labels:  map[string]string{"pool": "train"},
					Devices: []types.DeviceCount{{Resource: "google.com/tpu", Unit: types.DeviceUnitTPUChip, Capacity: 4}}}); err != nil {
					t.Fatal(err)
				}
			},
			node: poolNodeName, how: replacementByPool, sentence: poolSentence, depth: capacityDepth},
		{name: "remove-first nvidia recreated under the old name", mode: recoverymode.Automatic, strategy: recoverymode.RemoveFirst, vendor: types.AcceleratorVendorNVIDIA,
			setup: func(_ *testing.T, _ *sqlite.Store, p *poolPlatform) { p.sameName = true },
			node:  "n1", uid: "uid-n1-new", how: replacementByPool,
			sentence: "Node n1 is Ready. It was the only node that joined pool train after the removal.", depth: driverDepth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &poolPlatform{successorPlatform: newSuccessorPlatform()}
			c, st, notifier := successorFixture(t, tc.mode, p.successorPlatform, tc.strategy)
			c.platform = p
			registerSuccessorAgent(t, st, 8)
			if tc.setup != nil {
				tc.setup(t, st, p)
			}
			if !tc.noAgent {
				gpus := 8
				if tc.blind {
					gpus = 0
				}
				registerOldNode(t, st, gpus, tc.vendor)
			}
			uid := cmp.Or(tc.uid, "uid-"+tc.node)
			id := openVendorIncident(t, c, "n1", tc.vendor)
			inc := untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
			// A node without an agent sends no evidence to wait for.
			if inc.State != types.StateVerifying && (!tc.noAgent && !tc.heard || inc.State != types.StateResolved) {
				t.Fatalf("state = %s, want VERIFYING after the replacement%s", inc.State, dump(trail(t, st, id)))
			}
			if !tc.noAgent && !tc.heard {
				// Before the new node's evidence: reconciles never supersede it.
				inc = step(t, c, st, id, 5)
				if inc.State != types.StateVerifying {
					t.Fatalf("state = %s before any evidence, want VERIFYING%s", inc.State, dump(trail(t, st, id)))
				}
			}
			if tc.node != successorNodeName && !tc.noAgent {
				registerAgentAs(t, st, tc.node, uid)
			}
			switch tc.depth {
			case driverDepth:
				reportFrom(t, st, tc.node, uid, tc.vendor, types.AcceleratorReadinessReady)
			case observedDepth:
				reportFrom(t, st, tc.node, uid, tc.vendor, types.AcceleratorReadinessObserved)
			}
			inc = untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State != types.StateVerifying })
			entries := trail(t, st, id)
			if inc.State != types.StateResolved {
				t.Fatalf("state = %s, want RESOLVED%s", inc.State, dump(entries))
			}
			resolved := resolveEntry(entries)
			if resolved == nil || resolved.Params["infrastructure_evidence"] != replacementInfrastructureEvidence ||
				resolved.Params["replacement_node"] != tc.node || resolved.Params["replacement_node_uid"] != uid ||
				resolved.Params["replacement_correlation"] != tc.how {
				t.Fatalf("resolve entry = %+v", resolved)
			}
			if !strings.HasSuffix(resolved.Result, "; "+tc.sentence+" "+tc.depth) {
				t.Fatalf("resolution %q, want %q then %q", resolved.Result, tc.sentence, tc.depth)
			}
			if inc.Target.Node != "n1" {
				t.Fatalf("target = %+v, want the incident's original target", inc.Target)
			}
			// The old node's agent reported its devices: the successor is accepted with its own.
			if tc.how == replacementBySuccessor && !auditHas(entries, "successor-ready", "is Ready with its accelerators; two nodes run until n1 is removed") {
				t.Fatalf("successor-ready, want the accelerators its agent reported%s", dump(entries))
			}
			if tc.blind && (len(inc.Accelerators) != 0 || !auditHasParam(entries, replacementPoolAction, "capacity", "amd.com/gpu=8")) {
				t.Fatalf("accelerators %+v; want none recorded on the incident and the old node's capacity with the pool%s", inc.Accelerators, dump(entries))
			}
			if !notifier.hasMessage(tc.sentence) {
				t.Fatalf("no RESOLVED notification names the new node: %q", tc.sentence)
			}
			if ready := types.VerifiedInfrastructureTime(entries); ready == nil || !ready.Equal(resolved.Time) {
				t.Fatalf("verified infrastructure time = %v, want the resolution's %v", ready, resolved.Time)
			}
			report, err := c.RecoveryReport(context.Background(), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if report.Recovered != 1 {
				t.Fatalf("recovered = %d, want the replacement counted", report.Recovered)
			}
			if inc.ResolvedAt == nil || inc.ResolvedAt.Sub(resolved.Time).Abs() > time.Second {
				t.Fatalf("resolved at %v, want the degraded time to end at the confirmation %v", inc.ResolvedAt, resolved.Time)
			}
		})
	}
}

// Without evidence from the new node by the deadline, the incident goes to a
// person with what is missing, is not counted recovered, and no second
// replacement is started.
func TestReplacementWithoutEvidenceGoesToAPerson(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy recoverymode.Strategy
		setup    func(*poolPlatform)
		report   types.AcceleratorReadiness
		want     string
	}{
		{"no report", "", nil, "", "No fresh accelerator report from new node n9."},
		{"no devices", "", nil, types.AcceleratorReadinessNotReady, "New node n9 reports no accelerator devices."},
		{"not ready", recoverymode.RemoveFirst, func(p *poolPlatform) { p.nodeReady = false }, types.AcceleratorReadinessReady, "New node n10 is not Ready."},
		{"no new pool node", recoverymode.RemoveFirst, func(p *poolPlatform) { p.extra = -1 }, "", "No node joined pool train after the removal."},
		{"several new pool nodes", recoverymode.RemoveFirst, func(p *poolPlatform) { p.extra = 1 }, types.AcceleratorReadinessReady,
			"Several nodes joined pool train after the removal. KubeNeuron could not determine which one replaced the node."},
		{"too few devices", recoverymode.RemoveFirst, func(p *poolPlatform) { p.successorAllocatable = 4 }, types.AcceleratorReadinessReady,
			"New node n10 has 4 allocatable nvidia.com/gpu, fewer than the 8 the incident covered."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &poolPlatform{successorPlatform: newSuccessorPlatform()}
			if tc.setup != nil {
				tc.setup(p)
			}
			c, st, _ := successorFixture(t, recoverymode.Automatic, p.successorPlatform, tc.strategy)
			c.platform = p
			registerSuccessorAgent(t, st, 8)
			registerOldNode(t, st, 8, types.AcceleratorVendorNVIDIA)
			id := openVendorIncident(t, c, "n1", types.AcceleratorVendorNVIDIA)
			inc := untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
			if inc.State != types.StateVerifying {
				t.Fatalf("state = %s, want VERIFYING%s", inc.State, dump(trail(t, st, id)))
			}
			node := successorNodeName
			if tc.strategy == recoverymode.RemoveFirst {
				node = poolNodeName
				registerAgentAs(t, st, poolNodeName, "uid-"+poolNodeName)
			}
			if tc.report != "" {
				reportFrom(t, st, node, "uid-"+node, types.AcceleratorVendorNVIDIA, tc.report)
			}
			ctx := context.Background()
			if ok, reason, _ := c.verifyRecovery(ctx, inc); ok || reason != tc.want {
				t.Fatalf("verify = %v %q, want held on %q", ok, reason, tc.want)
			}
			// Past the deadline.
			inc.StateChangedAt = time.Now().Add(-time.Hour)
			inc.UpdatedAt = inc.StateChangedAt
			if err := st.UpdateIncident(ctx, inc); err != nil {
				t.Fatal(err)
			}
			if err := c.advanceVerifying(ctx, mustIncident(t, st, id)); err != nil {
				t.Fatal(err)
			}
			inc = mustIncident(t, st, id)
			entries := trail(t, st, id)
			if inc.State != types.StateNeedsHuman || !auditHas(entries, "verify-evidence", tc.want) {
				t.Fatalf("state = %s, want NEEDS_HUMAN naming %q%s", inc.State, tc.want, dump(entries))
			}
			if _, _, replaced, plain, _ := p.counts(); replaced+plain != 1 {
				t.Fatalf("%d replacements, want the one", replaced+plain)
			}
			report, err := c.RecoveryReport(ctx, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if report.Recovered != 0 {
				t.Fatalf("recovered = %d, want an unconfirmed replacement not counted", report.Recovered)
			}
		})
	}
}

// registerAgentAs has a node's agent register with the given UID.
func registerAgentAs(t *testing.T, st *sqlite.Store, node, uid string) {
	t.Helper()
	n := &types.Node{Name: node, UID: uid, AgentLastSeen: time.Now(),
		GPUs: []types.GPUInfo{{UUID: "GPU-new-0"}}}
	if err := st.UpsertAgentRegistration(context.Background(), n); err != nil {
		t.Fatal(err)
	}
}

// registerOldNode gives the target n1 its agent's accelerators of vendor, so
// the incident records what it covered.
func registerOldNode(t *testing.T, st *sqlite.Store, gpus int, vendor types.AcceleratorVendor) {
	t.Helper()
	n := &types.Node{Name: "n1", UID: "uid-n1", AgentLastSeen: time.Now(), Labels: map[string]string{"pool": "train"}}
	for i := 0; i < gpus; i++ {
		gpu := types.GPUInfo{UUID: fmt.Sprintf("GPU-n1-%d", i), Index: i}
		if vendor == types.AcceleratorVendorAMD {
			gpu.UUID, gpu.Model = fmt.Sprintf("amd-n1-%d", i), "AMD Instinct MI300X"
		}
		n.GPUs = append(n.GPUs, gpu)
	}
	if err := st.UpsertAgentRegistration(context.Background(), n); err != nil {
		t.Fatal(err)
	}
}

// An incident that recorded no accelerators (the old node's agent saw none)
// compares the new pool node with what the old node advertised, and says why
// it waits before the deadline; with nothing recorded it still cannot.
func TestRemoveFirstComparesWithTheReplacedNodeCapacity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*poolPlatform)
		want  string
	}{
		{"fewer than the old node", func(p *poolPlatform) { p.successorAllocatable = 4 },
			"New node n10 has 4 allocatable amd.com/gpu, fewer than the 8 the replaced node advertised."},
		{"old node advertised nothing", func(p *poolPlatform) { p.oldNoResource = true },
			"The incident recorded no accelerators to compare new node n10 against."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &poolPlatform{successorPlatform: newSuccessorPlatform(), resource: "amd.com/gpu"}
			tc.setup(p)
			c, st, _ := successorFixture(t, recoverymode.Automatic, p.successorPlatform, recoverymode.RemoveFirst)
			c.platform = p
			registerOldNode(t, st, 0, types.AcceleratorVendorAMD)
			id := openVendorIncident(t, c, "n1", types.AcceleratorVendorAMD)
			inc := untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
			if inc.State != types.StateVerifying {
				t.Fatalf("state = %s, want VERIFYING%s", inc.State, dump(trail(t, st, id)))
			}
			registerAgentAs(t, st, poolNodeName, "uid-"+poolNodeName)
			ctx := context.Background()
			if ok, reason, _ := c.verifyRecovery(ctx, inc); ok || reason != tc.want {
				t.Fatalf("verify = %v %q, want held on %q", ok, reason, tc.want)
			}
			// Inside the deadline: the reason is written once, not per reconcile.
			for range 3 {
				if err := c.advanceVerifying(ctx, mustIncident(t, st, id)); err != nil {
					t.Fatal(err)
				}
			}
			entries := trail(t, st, id)
			waits := 0
			for _, e := range entries {
				if e.Action == verifyWaitAction {
					waits++
					if e.Result != tc.want {
						t.Fatalf("waiting on %q, want %q", e.Result, tc.want)
					}
				}
			}
			if waits != 1 || mustIncident(t, st, id).State != types.StateVerifying {
				t.Fatalf("%d waiting rows, want one while VERIFYING%s", waits, dump(entries))
			}
		})
	}
}

func auditHasParam(entries []*types.AuditEntry, action, key, value string) bool {
	for _, e := range entries {
		if e.Action == action && e.Params[key] == value {
			return true
		}
	}
	return false
}

// A replacement plan that closes, or an operator who acknowledges, while the
// incident verifies is not a recurrence: on GKE the pool's resize settles
// after the incident has entered VERIFYING. A signal in that window still is.
func TestVerifyingWritesThatAreNotSignals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy recoverymode.Strategy
		write    func(*testing.T, *Controller, *types.Incident)
		recurs   bool
	}{
		{"remove-first plan closes", recoverymode.RemoveFirst, func(t *testing.T, c *Controller, inc *types.Incident) {
			rec := &types.RemoveFirstRecord{Outcome: types.RemoveFirstRestored}
			rec.Operation, rec.Pool = "resize-1", "train"
			if err := c.saveRemoveFirst(context.Background(), inc, rec, "remove-first-restored", "ended restored"); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"successor record written", "", func(t *testing.T, c *Controller, inc *types.Incident) {
			if err := c.saveSuccessor(context.Background(), inc, successorOrNew(inc), "successor-noted", "noted while verifying"); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"operator acknowledges", recoverymode.RemoveFirst, func(t *testing.T, c *Controller, inc *types.Incident) {
			if _, _, err := c.AcknowledgeIncident(context.Background(), inc.ID, "operator", "looking", 0, "ack-1"); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"the signal recurs", recoverymode.RemoveFirst, func(t *testing.T, c *Controller, inc *types.Incident) {
			if err := c.ingest(context.Background(), signal(types.ClassFellOffBus, "n1", "")); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &poolPlatform{successorPlatform: newSuccessorPlatform()}
			c, st, _ := successorFixture(t, recoverymode.Automatic, p.successorPlatform, tc.strategy)
			c.platform = p
			registerSuccessorAgent(t, st, 8)
			registerOldNode(t, st, 8, types.AcceleratorVendorNVIDIA)
			id := openVendorIncident(t, c, "n1", types.AcceleratorVendorNVIDIA)
			inc := untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
			if inc.State != types.StateVerifying {
				t.Fatalf("state = %s, want VERIFYING%s", inc.State, dump(trail(t, st, id)))
			}
			tc.write(t, c, inc)
			node := successorNodeName
			if tc.strategy == recoverymode.RemoveFirst {
				node = poolNodeName
				registerAgentAs(t, st, node, "uid-"+node)
			}
			reportFrom(t, st, node, "uid-"+node, types.AcceleratorVendorNVIDIA, types.AcceleratorReadinessReady)
			inc = untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State != types.StateVerifying })
			entries := trail(t, st, id)
			recurred := false
			for _, e := range entries {
				recurred = recurred || strings.Contains(e.Result, "signal recurred")
			}
			if tc.recurs != recurred || tc.recurs == (inc.State == types.StateResolved) {
				t.Fatalf("state = %s, recurred = %v, want recurred = %v%s", inc.State, recurred, tc.recurs, dump(entries))
			}
		})
	}
}

func successorOrNew(inc *types.Incident) *types.SuccessorRecord {
	if inc.Successor != nil {
		return inc.Successor.Clone()
	}
	rec := &types.SuccessorRecord{}
	rec.Operation = "add-1"
	return rec
}
