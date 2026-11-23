// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// liveNodePlatform is a cluster read one node object at a time.
type liveNodePlatform struct {
	platform.Platform
	nodes  map[string]types.Node
	uidErr error
}

func (p *liveNodePlatform) Name() string { return "live-node-test" }

func (p *liveNodePlatform) NodeLabels(_ context.Context, node string) (map[string]string, bool, error) {
	n, ok := p.nodes[node]
	return n.Labels, ok, nil
}

func (p *liveNodePlatform) NodeUID(_ context.Context, node string) (string, bool, error) {
	if p.uidErr != nil {
		return "", false, p.uidErr
	}
	n, ok := p.nodes[node]
	return n.UID, ok, nil
}

var (
	h100Labels = map[string]string{"accelerator": "nvidia-h100"}
	a100Labels = map[string]string{"accelerator": "nvidia-a100"}
)

// profileCluster has a profile selecting h100Labels, an agent that
// registered n1 as stored, and n1 as live.
func profileCluster(t *testing.T, stored, live types.Node) (*Controller, *liveNodePlatform) {
	t.Helper()
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertNode(context.Background(), &stored); err != nil {
		t.Fatal(err)
	}
	p := &liveNodePlatform{nodes: map[string]types.Node{live.Name: live}}
	c := New(st, nil, nil, nil, nil, p, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{testNVIDIAResetProfile()}); err != nil {
		t.Fatal(err)
	}
	return c, p
}

// The observation profile is chosen for the node object the agent
// authenticated on, by the labels it carries now. A node recreated with the
// same name is another node: an agent of the earlier one, or a cache still
// holding it, gets no profile.
func TestObservationProfileIsForTheNodeObject(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		stored   types.Node
		live     types.Node
		uid      string
		selected bool
		err      string
	}{
		{name: "the node, labels unchanged", uid: "uid-1", selected: true,
			stored: types.Node{Name: "n1", UID: "uid-1", Labels: h100Labels},
			live:   types.Node{Name: "n1", UID: "uid-1", Labels: h100Labels}},
		{name: "labelled into the profile after the agent registered", uid: "uid-1", selected: true,
			stored: types.Node{Name: "n1", UID: "uid-1", Labels: a100Labels},
			live:   types.Node{Name: "n1", UID: "uid-1", Labels: h100Labels}},
		{name: "labelled out of the profile after the agent registered", uid: "uid-1",
			stored: types.Node{Name: "n1", UID: "uid-1", Labels: h100Labels},
			live:   types.Node{Name: "n1", UID: "uid-1", Labels: a100Labels}},
		{name: "same name, new UID, asked for the earlier node", uid: "uid-1", err: "another node object",
			stored: types.Node{Name: "n1", UID: "uid-1", Labels: h100Labels},
			live:   types.Node{Name: "n1", UID: "uid-2", Labels: h100Labels}},
		{name: "same name, new UID, asked for the new node", uid: "uid-2",
			stored: types.Node{Name: "n1", UID: "uid-1", Labels: h100Labels},
			live:   types.Node{Name: "n1", UID: "uid-2", Labels: a100Labels}},
		{name: "node gone", uid: "uid-1", err: "does not exist",
			stored: types.Node{Name: "n1", UID: "uid-1", Labels: h100Labels},
			live:   types.Node{Name: "n2", UID: "uid-2", Labels: h100Labels}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := profileCluster(t, tc.stored, tc.live)
			profile, err := c.AcceleratorObservationProfile(ctx, "n1", tc.uid, types.AcceleratorVendorNVIDIA)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("profile = %+v, %v; want an error naming %q", profile, err, tc.err)
				}
				return
			}
			if err != nil || (profile != nil) != tc.selected {
				t.Fatalf("profile = %+v, %v; want selected=%v", profile, err, tc.selected)
			}
		})
	}
}

func TestObservationProfileHoldsWhenTheUIDCannotBeRead(t *testing.T) {
	n := types.Node{Name: "n1", UID: "uid-1", Labels: h100Labels}
	c, p := profileCluster(t, n, n)
	p.uidErr = errors.New("apiserver unavailable")
	if profile, err := c.AcceleratorObservationProfile(context.Background(), "n1", "uid-1", types.AcceleratorVendorNVIDIA); err == nil {
		t.Fatalf("profile = %+v with the UID unreadable, want an error", profile)
	}
}

// Verification without a report asks what the agent of the incident's node
// was told. When the node is now another object of the same name, nobody
// can say: it is not verified on the heartbeat by the new node's labels.
func TestVerificationWithoutReportIsForTheIncidentsNode(t *testing.T) {
	c, _ := profileCluster(t,
		types.Node{Name: "n1", UID: "uid-1", Labels: h100Labels},
		types.Node{Name: "n1", UID: "uid-2", Labels: a100Labels})
	inc := &types.Incident{ID: "i1", Target: types.Target{Node: "n1", GPUIndex: 0}, RepairNodeUID: "uid-1"}
	if ok, reason := c.verifyWithoutCurrentReport(context.Background(), inc, types.AcceleratorVendorNVIDIA, "no report"); ok ||
		!strings.Contains(reason, "cannot tell whether a runtime profile selects the node") {
		t.Fatalf("verified = %v, %q; want held, the profile undecidable", ok, reason)
	}
	inc.RepairNodeUID = "uid-2"
	if ok, reason := c.verifyWithoutCurrentReport(context.Background(), inc, types.AcceleratorVendorNVIDIA, "no report"); !ok {
		t.Fatalf("the node itself: verified = %v, %q", ok, reason)
	}
}

// The GPUs an agent reported make a node an accelerator node for the journal
// label only while it is the node they were reported on.
func TestJournalLabelCountsReportedGPUsOfTheNodeObject(t *testing.T) {
	cfg := journalConfig()
	cfg.AgentNodeSelector = map[string]string{"pool": "gpu"}
	gpu := map[string]string{"pool": "gpu"}
	for _, tc := range []struct {
		name      string
		storedUID string
		liveUID   string
		want      string
	}{
		{name: "the node that reported them", storedUID: "uid-1", liveUID: "uid-1", want: "var"},
		{name: "same name, new UID", storedUID: "uid-1", liveUID: "uid-2", want: ""},
		{name: "a platform without UIDs", want: "var"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, st, p := journalFixture(t, cfg,
				platform.JournalNode{Name: "n1", UID: tc.liveUID, OSImage: "Amazon Linux 2023.12.20261112", Labels: gpu})
			if err := st.UpsertNode(context.Background(), &types.Node{Name: "n1", UID: tc.storedUID, GPUs: []types.GPUInfo{{Index: 0}}}); err != nil {
				t.Fatal(err)
			}
			c.reconcileJournalLabels(context.Background())
			if got := p.dir("n1"); got != tc.want {
				t.Fatalf("label %q, want %q", got, tc.want)
			}
		})
	}
}

// The journal label follows the labels the node carries now, not those its
// agent registered with.
func TestJournalLabelFollowsTheLabelsOfTheNodeNow(t *testing.T) {
	cfg := journalConfig()
	cfg.AgentNodeSelector = map[string]string{"pool": "gpu"}
	c, st, p := journalFixture(t, cfg,
		platform.JournalNode{Name: "in", UID: "uid-in", OSImage: "Amazon Linux 2023.12.20261112", Labels: map[string]string{"pool": "gpu"}, Accelerator: true},
		platform.JournalNode{Name: "out", UID: "uid-out", OSImage: "Amazon Linux 2023.12.20261112", Labels: map[string]string{"pool": "cpu"}, Accelerator: true, Dir: "var"},
	)
	for name, labels := range map[string]map[string]string{"in": {"pool": "cpu"}, "out": {"pool": "gpu"}} {
		if err := st.UpsertNode(context.Background(), &types.Node{Name: name, UID: "uid-" + name, Labels: labels}); err != nil {
			t.Fatal(err)
		}
	}
	c.reconcileJournalLabels(context.Background())
	// Outside the agent's selector a node keeps its label: it may be another
	// installation's.
	if p.dir("in") != "var" || p.dir("out") != "var" {
		t.Fatalf("labels in=%q out=%q, want var and left alone", p.dir("in"), p.dir("out"))
	}
	p.nodes["out"].Labels = map[string]string{"pool": "gpu"}
	p.nodes["out"].OSImage = "Debian GNU/Linux 12 (bookworm)"
	c.reconcileJournalLabels(context.Background())
	if p.dir("out") != "" {
		t.Fatalf("relabelled into the selector: label %q, want it taken off for the unknown image", p.dir("out"))
	}
}

// A journal mount failure is the node object's: the node recreated with the
// same name is labelled again at once.
func TestJournalMountFailureIsTheNodeObjects(t *testing.T) {
	c, _, p := journalFixture(t, journalConfig(),
		platform.JournalNode{Name: "a", UID: "uid-1", OSImage: "Amazon Linux 2023", Accelerator: true, Dir: "var",
			WaitingSince: time.Now().Add(-journalMountWait - time.Second)},
	)
	c.reconcileJournalLabels(context.Background())
	if p.dir("a") != "" || c.JournalReason("a") != JournalReasonMountFailed {
		t.Fatalf("after the failure: label %q reason %q", p.dir("a"), c.JournalReason("a"))
	}
	p.nodes["a"].UID = "uid-2"
	c.reconcileJournalLabels(context.Background())
	if p.dir("a") != "var" || c.JournalReason("a") != "" {
		t.Fatalf("the new node: label %q reason %q", p.dir("a"), c.JournalReason("a"))
	}
}

// Only a cloud recovery step recreates a node object, and an incident that
// ran one is verified by its own acceptance of the node, never on the
// heartbeat path that asks about the incident's pinned node. Without such a
// step, a node of the same name with another UID holds there.
func TestRecreatingRecoveryIsNotVerifiedOnTheHeartbeatPath(t *testing.T) {
	const undecidable = "cannot tell whether a runtime profile selects the node"
	for _, wire := range []string{"", "agent.reboot", "platform.recycle_node", "platform.replace_node"} {
		t.Run("step "+wire, func(t *testing.T) {
			c, _ := profileCluster(t,
				types.Node{Name: "n1", UID: "uid-2", Labels: a100Labels, AgentLastSeen: time.Now()},
				types.Node{Name: "n1", UID: "uid-2", Labels: a100Labels})
			now := time.Now()
			inc := &types.Incident{ID: "i1", Class: types.ClassDriverHang, Target: types.Target{Node: "n1", GPUUUID: "GPU-1"},
				RepairNodeUID: "uid-1", Vendor: types.AcceleratorVendorNVIDIA, State: types.StateVerifying,
				OpenedAt: now, UpdatedAt: now, StateChangedAt: now}
			if err := c.store.CreateIncident(context.Background(), inc); err != nil {
				t.Fatal(err)
			}
			if wire != "" {
				if err := c.store.AppendAudit(context.Background(), &types.AuditEntry{IncidentID: inc.ID, Time: time.Now(),
					Actor: "system", Action: "step", Result: auditStepStartResult(wire)}); err != nil {
					t.Fatal(err)
				}
			}
			ok, reason, _ := c.verifyRecovery(context.Background(), inc)
			cloudStep := wire == "platform.recycle_node" || wire == "platform.replace_node"
			if ok || strings.Contains(reason, undecidable) == cloudStep {
				t.Fatalf("verified = %v, %q; want held, on the heartbeat path=%v", ok, reason, !cloudStep)
			}
		})
	}
}
