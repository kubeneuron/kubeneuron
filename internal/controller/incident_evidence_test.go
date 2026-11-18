// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/evidence"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// evidencePlatform answers the node evidence read on top of the automatic
// fixture's cloud platform.
type evidencePlatform struct {
	autoCloudPlatform
	nodes map[string]*evidence.Node
	err   error
	reads int
}

func (p *evidencePlatform) NodeEvidence(_ context.Context, node string) (*evidence.Node, error) {
	p.reads++
	if p.err != nil {
		return nil, p.err
	}
	return p.nodes[node], nil
}

func evidenceRows(t *testing.T, st *sqlite.Store, id string) []map[string]string {
	t.Helper()
	trail, err := st.AuditTrail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]string
	for _, e := range trail {
		if e.Action == EvidenceAction {
			rows = append(rows, e.Params)
		}
	}
	return rows
}

func firstEvidence(t *testing.T, st *sqlite.Store, id string) types.IncidentEvidence {
	t.Helper()
	rows := evidenceRows(t, st, id)
	if len(rows) == 0 {
		t.Fatalf("no evidence row for %s", id)
	}
	var ev types.IncidentEvidence
	if err := json.Unmarshal([]byte(rows[0]["incident_evidence"]), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func healthyEvidenceNode() *evidence.Node {
	return &evidence.Node{
		Capacity:    map[string]int64{"nvidia.com/gpu": 8},
		Allocatable: map[string]int64{"nvidia.com/gpu": 8},
		Pods:        []evidence.Pod{{Labels: map[string]string{"app": "nvidia-device-plugin-daemonset"}, Running: true, Ready: true}},
	}
}

func TestOpeningRecordsEvidence(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Approval, nil, 4)
	p := &evidencePlatform{nodes: map[string]*evidence.Node{"n1": healthyEvidenceNode()}}
	c.platform = p
	id := openOn(t, c, "n1")
	ev := firstEvidence(t, st, id)
	if ev.Location.Location != types.LocationThisNode || ev.Software.State != types.EvidenceOK || ev.Network.State != types.EvidenceUnknown {
		t.Fatalf("evidence %+v", ev)
	}
	if p.reads != 1 {
		t.Fatalf("node read %d times", p.reads)
	}
}

func TestUnreadableNodeEvidenceIsUnknown(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Approval, nil, 4)
	c.platform = &evidencePlatform{err: errors.New("forbidden")}
	ev := firstEvidence(t, st, openOn(t, c, "n1"))
	if ev.Software.State != types.EvidenceUnknown || ev.Network.State != types.EvidenceUnknown {
		t.Fatalf("an unread node must be unknown, never ok: %+v", ev)
	}
}

func TestSimultaneousFaultsAreGrouped(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Approval, nil, 4)
	c.platform = &evidencePlatform{nodes: map[string]*evidence.Node{"n1": healthyEvidenceNode(), "n2": healthyEvidenceNode()}}
	first := openOn(t, c, "n1")
	second := openOn(t, c, "n2")
	ev := firstEvidence(t, st, second)
	if ev.Location.Location != types.LocationShared || ev.Location.Group != first || len(ev.Location.Members) != 2 {
		t.Fatalf("second incident: %+v", ev.Location)
	}
	// The first incident is regrouped by a location-only row.
	rows := evidenceRows(t, st, first)
	if len(rows) != 2 || rows[1]["fault_location"] == "" || rows[1]["incident_evidence"] != "" {
		t.Fatalf("first incident rows %v", rows)
	}
	var loc types.LocationEvidence
	if err := json.Unmarshal([]byte(rows[1]["fault_location"]), &loc); err != nil || loc.Location != types.LocationShared || loc.Group != first {
		t.Fatalf("regrouped first: %+v %v", loc, err)
	}
}

func TestAutomaticNeedsAQualifiedPair(t *testing.T) {
	for name, tc := range map[string]struct {
		profiles   []string
		unprofiled bool
		want       string
	}{
		"no qualified profile":    {nil, false, "qualifies no node profile"},
		"another profile":         {[]string{"other-profile"}, false, "is not a qualified pair"},
		"node matches no profile": {[]string{autoProfile}, true, "cannot be resolved"},
	} {
		t.Run(name, func(t *testing.T) {
			settings := autoSettings()
			settings.QualifiedProfiles = tc.profiles
			c, st, _ := autoFixture(t, recoverymode.Automatic, settings, 4)
			if tc.unprofiled {
				// A pool node the profile does not select.
				profile := c.runtimeConfig(context.Background()).AcceleratorProfiles[0]
				profile.NodeSelector = map[string]string{"pool": "train", "profiled": "true"}
				c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.AcceleratorProfiles[0] = profile })
			}
			id := openOn(t, c, "n1")
			if inc := step(t, c, st, id, 50); inc.State != types.StateAwaitingApproval || !strings.Contains(parkReason(t, st, id), tc.want) {
				t.Fatalf("state=%s audit=%s, want a park naming %q", inc.State, parkReason(t, st, id), tc.want)
			}
		})
	}
}

// Evidence is shown and never authorises: a qualified pair runs the same with
// every evidence row unknown or missing, and an unqualified one parks the
// same with every row healthy.
func TestEvidenceNeverAuthorises(t *testing.T) {
	broken := &evidence.Node{Capacity: map[string]int64{"nvidia.com/gpu": 8, "vpc.amazonaws.com/efa": 4}, Allocatable: map[string]int64{"nvidia.com/gpu": 0}}
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	c.platform = &evidencePlatform{nodes: map[string]*evidence.Node{"n1": broken}}
	id := openOn(t, c, "n1")
	if ev := firstEvidence(t, st, id); ev.Network.State != types.EvidenceMissing || ev.Software.State != types.EvidenceDegraded {
		t.Fatalf("fixture evidence %+v", ev)
	}
	if inc := step(t, c, st, id, 200); inc.State != types.StateResolved {
		t.Fatalf("qualified pair with bad evidence: %s, want the ladder to run as before", inc.State)
	}

	settings := autoSettings()
	settings.QualifiedProfiles = nil
	c, st, _ = autoFixture(t, recoverymode.Automatic, settings, 4)
	c.platform = &evidencePlatform{nodes: map[string]*evidence.Node{"n1": healthyEvidenceNode()}}
	id = openOn(t, c, "n1")
	if inc := step(t, c, st, id, 50); inc.State != types.StateAwaitingApproval {
		t.Fatalf("unqualified pair with healthy evidence: %s, want a park", inc.State)
	}
}

// A storage timeout, a hung job, a pending pod or a missing network resource
// is not a class any policy can bind: no incident of it opens a ladder.
func TestNonHardwareCausesNeverStartTheLadder(t *testing.T) {
	c, _, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	engine := c.runtimeConfig(context.Background()).Engine
	for _, class := range []types.ProblemClass{"storage-timeout", "hung-job", "pending-pod", "network-resource-missing"} {
		if _, ok := engine.PolicyFor(class, ""); ok {
			t.Errorf("%s selected a policy", class)
		}
	}
}

// Automatic replacement needs the fault located on this node: a shared or
// unknown location waits for a person, whatever the pair.
func TestAutomaticReplacementNeedsTheFaultOnThisNode(t *testing.T) {
	for _, loc := range []types.FaultLocation{types.LocationShared, types.LocationUnknown} {
		t.Run(string(loc), func(t *testing.T) {
			c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
			id := openOn(t, c, "n1")
			inc, err := st.GetIncident(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(types.LocationEvidence{Location: loc, Rule: "test rule"})
			c.appendEvidenceRow(context.Background(), inc, map[string]string{"fault_location": string(data)})
			if inc := step(t, c, st, id, 50); inc.State != types.StateAwaitingApproval || !strings.Contains(parkReason(t, st, id), "the fault location is "+string(loc)) {
				t.Fatalf("state=%s audit=%s, want a park naming the location", inc.State, parkReason(t, st, id))
			}
		})
	}
}
