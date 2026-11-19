// SPDX-License-Identifier: Apache-2.0

package controller

import (
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
// label, and whose pool gains a node once the target is removed.
type poolPlatform struct {
	*successorPlatform
	// members are the pool's nodes besides the target; added on removal.
	members []platform.SuccessorNode
	// extra is how many more new nodes the pool gains on removal.
	extra int
}

func (p *poolPlatform) CloudNode(ctx context.Context, node string) (cloud.NodeRef, error) {
	ref, err := p.successorPlatform.CloudNode(ctx, node)
	ref.Labels = map[string]string{"eks.amazonaws.com/nodegroup": "train"}
	return ref, err
}

func (p *poolPlatform) ReplacePinnedNode(ctx context.Context, pin cloud.NodeIdentity) error {
	if err := p.successorPlatform.ReplacePinnedNode(ctx, pin); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i <= p.extra; i++ {
		name := poolNodeName
		if i > 0 {
			name += strings.Repeat("x", i)
		}
		p.members = append(p.members, platform.SuccessorNode{Found: true, Name: name, UID: "uid-" + name,
			CreatedAt: time.Now().Add(time.Millisecond), Ready: p.nodeReady,
			Allocatable: map[string]int64{topology.ResourceWholeGPU: p.successorAllocatable, "amd.com/gpu": p.successorAllocatable}})
	}
	return nil
}

func (p *poolPlatform) PoolNodes(_ context.Context, label, pool string) ([]platform.SuccessorNode, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if label != "eks.amazonaws.com/nodegroup" || pool != "train" {
		return nil, nil
	}
	old := platform.SuccessorNode{Found: true, Name: "n2", UID: "uid-n2", CreatedAt: time.Now().Add(-time.Hour), Ready: true}
	return append([]platform.SuccessorNode{old}, p.members...), nil
}

// reportFrom has the agent on node post its accelerator report now.
func reportFrom(t *testing.T, st *sqlite.Store, node string, vendor types.AcceleratorVendor, readiness types.AcceleratorReadiness) {
	t.Helper()
	report := readyNVIDIAResetReport(time.Now(), "profile")
	report.Node, report.NodeUID, report.Vendor, report.Readiness = node, "uid-"+node, vendor, readiness
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
	sig.Evidence["vendor"] = string(vendor)
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
// successor the provider named (add-first), or the one node of the target's
// pool created after the removal (remove-first). The resolution names the
// node, how it was tied to the replacement and what was confirmed, at the
// depth the vendor's report allows, and the report counts it recovered.
func TestReplacementResolvesOnTheNewNode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      recoverymode.Mode
		strategy  recoverymode.Strategy
		vendor    types.AcceleratorVendor
		node, how string
		depth     string
	}{
		{"add-first nvidia", recoverymode.Automatic, "", types.AcceleratorVendorNVIDIA, successorNodeName, replacementBySuccessor, "driver health observed"},
		{"add-first aws approval", recoverymode.Approval, "", types.AcceleratorVendorAWS, successorNodeName, replacementBySuccessor, "device presence observed; runtime not attested"},
		{"remove-first nvidia approval", recoverymode.Approval, recoverymode.RemoveFirst, types.AcceleratorVendorNVIDIA, poolNodeName, replacementByPool, "driver health observed"},
		{"remove-first aws", recoverymode.Automatic, recoverymode.RemoveFirst, types.AcceleratorVendorAWS, poolNodeName, replacementByPool, "device presence observed; runtime not attested"},
		{"remove-first amd", recoverymode.Automatic, recoverymode.RemoveFirst, types.AcceleratorVendorAMD, poolNodeName, replacementByPool, "this build reads no amd accelerator report, so its devices were not checked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &poolPlatform{successorPlatform: newSuccessorPlatform()}
			c, st, _ := successorFixture(t, tc.mode, p.successorPlatform, tc.strategy)
			c.platform = p
			registerSuccessorAgent(t, st, 8)
			registerOldNode(t, st, 8)
			id := openVendorIncident(t, c, "n1", tc.vendor)
			inc := untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
			if inc.State != types.StateVerifying {
				t.Fatalf("state = %s, want VERIFYING after the replacement%s", inc.State, dump(trail(t, st, id)))
			}
			if tc.node == poolNodeName {
				registerAgent(t, st, poolNodeName)
			}
			readiness := types.AcceleratorReadinessReady
			if tc.vendor == types.AcceleratorVendorAWS {
				readiness = types.AcceleratorReadinessObserved
			}
			if tc.vendor != types.AcceleratorVendorAMD {
				reportFrom(t, st, tc.node, tc.vendor, readiness)
			}
			inc = untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State != types.StateVerifying })
			entries := trail(t, st, id)
			if inc.State != types.StateResolved {
				t.Fatalf("state = %s, want RESOLVED%s", inc.State, dump(entries))
			}
			resolved := resolveEntry(entries)
			if resolved == nil || resolved.Params["infrastructure_evidence"] != replacementInfrastructureEvidence ||
				resolved.Params["replacement_node"] != tc.node || resolved.Params["replacement_node_uid"] != "uid-"+tc.node ||
				resolved.Params["replacement_correlation"] != tc.how {
				t.Fatalf("resolve entry = %+v", resolved)
			}
			if !strings.Contains(resolved.Result, "replaced by node "+tc.node) || !strings.Contains(resolved.Result, tc.depth) {
				t.Fatalf("resolution %q does not name the node and %q", resolved.Result, tc.depth)
			}
			if inc.Target.Node != "n1" {
				t.Fatalf("target = %+v, want the incident's original target", inc.Target)
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
		})
	}
}

// Without evidence from the new node by the deadline, the incident goes to a
// person with what is missing, and no second replacement is started.
func TestReplacementWithoutEvidenceGoesToAPerson(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy recoverymode.Strategy
		setup    func(*poolPlatform)
		report   types.AcceleratorReadiness
		want     string
	}{
		{"no report", "", nil, "", "no fresh nvidia accelerator report from replacement node n9"},
		{"no devices", "", nil, types.AcceleratorReadinessNotReady, "shows no ready devices"},
		{"not ready", recoverymode.RemoveFirst, func(p *poolPlatform) { p.nodeReady = false }, types.AcceleratorReadinessReady, "replacement node n10 is not Ready"},
		{"no new pool node", recoverymode.RemoveFirst, func(p *poolPlatform) { p.extra = -1 }, "", "no node of pool train was created after the removal"},
		{"ambiguous pool", recoverymode.RemoveFirst, func(p *poolPlatform) { p.extra = 1 }, types.AcceleratorReadinessReady, "ambiguous: 2 nodes of pool train were created after the removal"},
		{"too few devices", recoverymode.RemoveFirst, func(p *poolPlatform) { p.successorAllocatable = 4 }, types.AcceleratorReadinessReady, "has 4 allocatable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &poolPlatform{successorPlatform: newSuccessorPlatform()}
			if tc.setup != nil {
				tc.setup(p)
			}
			c, st, _ := successorFixture(t, recoverymode.Automatic, p.successorPlatform, tc.strategy)
			c.platform = p
			registerSuccessorAgent(t, st, 8)
			registerOldNode(t, st, 8)
			id := openVendorIncident(t, c, "n1", types.AcceleratorVendorNVIDIA)
			inc := untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
			if inc.State != types.StateVerifying {
				t.Fatalf("state = %s, want VERIFYING%s", inc.State, dump(trail(t, st, id)))
			}
			node := successorNodeName
			if tc.strategy == recoverymode.RemoveFirst {
				node = poolNodeName
				registerAgent(t, st, poolNodeName)
			}
			if tc.report != "" {
				reportFrom(t, st, node, types.AcceleratorVendorNVIDIA, tc.report)
			}
			ctx := context.Background()
			if ok, reason, _ := c.verifyRecovery(ctx, inc); ok || !strings.Contains(reason, tc.want) {
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
		})
	}
}

// registerAgent has a node's agent register with its UID.
func registerAgent(t *testing.T, st *sqlite.Store, node string) {
	t.Helper()
	n := &types.Node{Name: node, UID: "uid-" + node, AgentLastSeen: time.Now(),
		GPUs: []types.GPUInfo{{UUID: "GPU-new-0"}}}
	if err := st.UpsertAgentRegistration(context.Background(), n); err != nil {
		t.Fatal(err)
	}
}

// registerOldNode gives the target n1 its agent's accelerators, so the
// incident records what it covered.
func registerOldNode(t *testing.T, st *sqlite.Store, gpus int) {
	t.Helper()
	n := &types.Node{Name: "n1", UID: "uid-n1", AgentLastSeen: time.Now(), Labels: map[string]string{"pool": "train"}}
	for i := 0; i < gpus; i++ {
		n.GPUs = append(n.GPUs, types.GPUInfo{UUID: fmt.Sprintf("GPU-n1-%d", i), Index: i})
	}
	if err := st.UpsertAgentRegistration(context.Background(), n); err != nil {
		t.Fatal(err)
	}
}
