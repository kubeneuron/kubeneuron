// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/kmsg"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/impact"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Kernel log lines the amdgpu driver prints, as the agent reads them from
// /dev/kmsg. Each names the device by its PCI address only.
const (
	amdRingTimeout = `3,2201,4053054882,-;amdgpu 0000:c3:00.0: amdgpu: ring gfx_0.0.0 timeout, signaled seq=1148, emitted seq=1150`
	amdPCIeError   = `2,2299,4055554882,-;amdgpu 0000:c3:00.0: amdgpu: PCI error: detected callback, state(1)!!`
	amdECC         = `3,2210,4053154882,-;amdgpu 0000:c3:00.0: amdgpu: 2 uncorrectable hardware errors detected in umc block`
	amdBadPage     = `amdgpu 0000:c3:00.0: amdgpu: bad page 0x00012340 reserved successfully`
)

// amdEvent is the event the agent sends for line: a fault of another vendor
// carries no GPU index and no UUID, only the PCI address.
func amdEvent(t *testing.T, id, line string) types.AgentEvent {
	t.Helper()
	ev, ok := kmsg.ParseLine(line)
	if !ok || ev.Fault == nil || ev.Fault.Vendor != "amd" {
		t.Fatalf("line %q does not parse as an amd fault: %+v", line, ev)
	}
	return types.AgentEvent{EventID: id, Node: "n1", GPUIndex: -1, PCIAddr: ev.PCIAddr, Fault: ev.Fault, Raw: ev.Raw, Timestamp: time.Now()}
}

// shippedPolicies are configs/policies.yaml in mode, as one node with a live
// safety gate runs them.
func shippedPolicies(t *testing.T, mode recoverymode.Mode) (map[string]*playbook.Playbook, []playbook.Policy) {
	t.Helper()
	books, err := playbook.LoadDir("../../configs/playbooks")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("../../configs/policies.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var policies []playbook.Policy
	for _, p := range cfg.Policies {
		policies = append(policies, playbook.Policy{Name: "shipped-" + string(p.Match.Class), Class: p.Match.Class,
			Playbook: p.Playbook, Params: p.Params, Mode: mode})
	}
	return books, policies
}

// amdSingleNode is a cluster of one node, the node the controller runs on,
// with the shipped policies in mode.
func amdSingleNode(t *testing.T, mode recoverymode.Mode) (*Controller, *sqlite.Store, *successorPlatform) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertNode(context.Background(), &types.Node{Name: "n1", Labels: map[string]string{"pool": "train"}}); err != nil {
		t.Fatal(err)
	}
	engine, err := playbook.NewEngine(shippedPolicies(t, mode))
	if err != nil {
		t.Fatal(err)
	}
	live := safety.Limits{MaxConcurrentRemediations: 8, MaxConcurrentReboots: 8}
	p := newSuccessorPlatform()
	c := New(st, st, engine, safety.NewGate(live), safety.NewFlapDetector(100, time.Hour), p, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &live}); err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)
	return c, st, p
}

func onlyIncident(t *testing.T, st *sqlite.Store) *types.Incident {
	t.Helper()
	incidents := incidentsOf(t, st)
	if len(incidents) != 1 {
		t.Fatalf("incidents = %+v, want one", incidents)
	}
	return incidents[0]
}

func reconcileRounds(c *Controller, rounds int) {
	for range rounds {
		c.reconcile(context.Background())
	}
}

// Under the shipped policies an amdgpu fault opens an incident of its class
// on the node, with the PCI address as its only device identity. The reset
// playbooks cannot act on a device without a UUID, so the incident goes to a
// person before the first step, in every mode: nothing is cordoned, drained
// or reset, also when the node is the only one and runs the controller.
func TestAMDKernelFaultUnderShippedPolicies(t *testing.T) {
	const unattributed = "is unattributed (no GPU UUID); a per-device reset is impossible: " +
		"physical NVIDIA reset requires a GPU UUID target, but this incident is unattributed (empty GPU UUID)"
	for _, tc := range []struct {
		line     string
		class    types.ProblemClass
		playbook string
		state    types.IncidentState
	}{
		{line: amdRingTimeout, class: types.ClassDriverHang, playbook: "gpu-reset", state: types.StateNeedsHuman},
		{line: amdECC, class: types.ClassECCDBE, playbook: "drain-and-reset", state: types.StateNeedsHuman},
		{line: amdBadPage, class: types.ClassRowRemapOK, playbook: "reset-when-idle", state: types.StateNeedsHuman},
		{line: amdPCIeError, class: types.ClassPCIe, playbook: "observe-suspect", state: types.StateObserving},
	} {
		for _, mode := range []recoverymode.Mode{recoverymode.DryRun, recoverymode.Approval} {
			t.Run(string(tc.class)+"/"+string(mode), func(t *testing.T) {
				c, st, p := amdSingleNode(t, mode)
				sendAgentEvent(t, c, amdEvent(t, "amd-1", tc.line))
				reconcileRounds(c, 40)
				inc := onlyIncident(t, st)
				entries := trail(t, st, inc.ID)
				want := types.Target{Node: "n1", GPUIndex: -1, PCIAddr: "0000:c3:00"}
				if inc.Class != tc.class || inc.Vendor != types.AcceleratorVendorAMD || inc.Target != want || inc.Playbook != tc.playbook {
					t.Fatalf("incident %s %s %+v %s, want %s amd %+v %s", inc.Class, inc.Vendor, inc.Target, inc.Playbook, tc.class, want, tc.playbook)
				}
				if inc.State != tc.state || inc.StepIndex != 0 {
					t.Fatalf("state %s step %d, want %s step 0%s", inc.State, inc.StepIndex, tc.state, dump(entries))
				}
				if requests, _, _, _, drained := p.counts(); requests != 0 || drained != 0 || len(p.cordoned) != 0 {
					t.Fatalf("the platform was asked to act: requests %d drained %d cordoned %v", requests, drained, p.cordoned)
				}
				if tc.state == types.StateNeedsHuman && !auditHas(entries, "quarantine", `playbook "`+tc.playbook+`" resets a GPU but incident `+inc.ID+" "+unattributed) {
					t.Fatalf("no quarantine saying why%s", dump(entries))
				}
			})
		}
	}
}

// Automatic mode refuses the shipped reset playbooks when the configuration
// loads: they run node-level agent actions.
func TestAMDKernelFaultShippedPlaybooksRefuseAutomatic(t *testing.T) {
	for class, want := range map[types.ProblemClass]string{
		types.ClassDriverHang: `policy for class "driver-hang": policy in Automatic mode cannot use node-level action "agent.idle_check" in playbook "gpu-reset"; use Approval mode`,
		types.ClassECCDBE:     `policy for class "ecc-dbe": policy in Automatic mode cannot use node-level action "agent.gpu_reset" in playbook "drain-and-reset"; use Approval mode`,
		types.ClassRowRemapOK: `policy for class "row-remap-ok": policy in Automatic mode cannot use node-level action "agent.wait_idle" in playbook "reset-when-idle"; use Approval mode`,
	} {
		books, policies := shippedPolicies(t, recoverymode.Approval)
		for i := range policies {
			if policies[i].Class == class {
				policies[i].Mode, policies[i].Automatic = recoverymode.Automatic, autoSettings()
			}
		}
		if _, err := playbook.NewEngine(books, policies); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", class, err, want)
		}
	}
}

// A node playbook (cordon, drain, replace) for the driver hang parks for a
// person in every mode before the node is disrupted. Automatic mode stops at
// the cordon: no runtime profile qualifies a node for amd. The preview at the
// park says the devices are unknown.
func TestAMDKernelFaultNodePlaybookParksForAPerson(t *testing.T) {
	const noProfile = "automatic policy " + autoPolicy + " did not authorise this step: automatic replacement is not allowed on this node — " +
		"no AcceleratorRuntimeProfile selects this node's labels for vendor amd"
	for _, tc := range []struct {
		mode recoverymode.Mode
		// step is the index of the step the incident parks at: 0 the
		// cordon, 1 the drain.
		step int
		why  string
	}{
		{mode: recoverymode.DryRun, step: 1, why: "step requires human approval"},
		{mode: recoverymode.Approval, step: 0},
		{mode: recoverymode.Automatic, step: 0, why: noProfile},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			p := newSuccessorPlatform()
			c, st, _ := successorFixture(t, tc.mode, p, "")
			book := &playbook.Playbook{Name: "cloud-replace", Target: "node", Steps: []playbook.Step{
				{Name: "cordon", Action: "platform.cordon"},
				{Name: "drain", Action: "platform.drain", Approval: "required"},
				{Name: "replace", Action: "platform.replace_node", Approval: "required"},
			}}
			pol := playbook.Policy{Name: autoPolicy, Class: types.ClassDriverHang, Playbook: book.Name, Mode: tc.mode}
			if tc.mode == recoverymode.Automatic {
				pol.Automatic = autoSettings()
			}
			engine, err := playbook.NewEngine(map[string]*playbook.Playbook{book.Name: book}, []playbook.Policy{pol})
			if err != nil {
				t.Fatal(err)
			}
			c.SetEngine(engine)
			c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.Impact = impactPolicy(false) })
			sendAgentEvent(t, c, amdEvent(t, "amd-1", amdRingTimeout))
			id := onlyIncident(t, st).ID
			inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.State == types.StateAwaitingApproval || inc.State.Halted() })
			entries := trail(t, st, id)
			if inc.State != types.StateAwaitingApproval || inc.StepIndex != tc.step {
				t.Fatalf("state %s step %d, want AWAITING_APPROVAL at step %d%s", inc.State, inc.StepIndex, tc.step, dump(entries))
			}
			if tc.why != "" && !slices.ContainsFunc(entries, func(e *types.AuditEntry) bool {
				return e.ToState == types.StateAwaitingApproval && strings.Contains(e.Result, tc.why)
			}) {
				t.Fatalf("the park does not say %q%s", tc.why, dump(entries))
			}
			if _, _, _, _, drained := p.counts(); drained != 0 || len(p.cordoned) != 0 {
				t.Fatalf("the node was disrupted before a person decided: drained %d cordoned %v", drained, p.cordoned)
			}
			pv, err := c.ImpactPreview(context.Background(), id, 0)
			if err != nil || pv == nil || pv.UnavailableReason != "" {
				t.Fatalf("preview %+v, %v", pv, err)
			}
			var payload struct {
				Warnings []string `json:"warnings"`
			}
			if err := json.Unmarshal(pv.Payload, &payload); err != nil || !slices.Contains(payload.Warnings, impact.WarningTopologyRefused) {
				t.Fatalf("preview warnings %v (%v), want %s: %s", payload.Warnings, err, impact.WarningTopologyRefused, pv.Payload)
			}
		})
	}
}

// A resolved amdgpu incident stays closed: reconciling does not reopen it,
// and the agent sending the same event again does not open another one. A
// new fault opens a new incident.
func TestAMDKernelFaultIncidentStaysClosed(t *testing.T) {
	c, st, _ := amdSingleNode(t, recoverymode.Approval)
	sendAgentEvent(t, c, amdEvent(t, "amd-1", amdRingTimeout))
	reconcileRounds(c, 20)
	inc := onlyIncident(t, st)
	if inc.State != types.StateNeedsHuman {
		t.Fatalf("state %s, want NEEDS_HUMAN", inc.State)
	}
	if err := c.ResolveIncident(context.Background(), inc.ID, "andrew", "node checked"); err != nil {
		t.Fatal(err)
	}
	reconcileRounds(c, 40)
	sendAgentEvent(t, c, amdEvent(t, "amd-1", amdRingTimeout))
	reconcileRounds(c, 20)
	if got := onlyIncident(t, st); got.ID != inc.ID || got.State != types.StateResolved {
		t.Fatalf("incident %s %s, want %s RESOLVED%s", got.ID, got.State, inc.ID, dump(trail(t, st, got.ID)))
	}

	sendAgentEvent(t, c, amdEvent(t, "amd-2", amdRingTimeout))
	reconcileRounds(c, 20)
	if incidents := incidentsOf(t, st); len(incidents) != 2 {
		t.Fatalf("a new fault opened %d incidents, want a second one", len(incidents))
	}
}
