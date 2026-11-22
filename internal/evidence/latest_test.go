// SPDX-License-Identifier: Apache-2.0

package evidence

import (
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestLatestEvidence(t *testing.T) {
	block := `{"observed_at":"2026-11-18T12:00:00Z","fault_location":{"location":"this-node","rule":"r"},"vendor_software":{"state":"ok"},"network_resource":{"state":"unknown"}}`
	trail := []*types.AuditEntry{
		{Action: "open", Params: map[string]string{"incident_evidence": `{"observed_at":"2026-11-18T11:00:00Z","fault_location":{"location":"this-node"}}`}},
		{Action: "evidence", Params: map[string]string{"incident_evidence": block}},
	}
	// The open row carries the signal's evidence and is never read as ours.
	if ev := Latest(trail[:1]); ev != nil {
		t.Fatalf("open row read: %+v", ev)
	}
	ev := Latest(trail)
	if ev == nil || ev.Location.Location != types.LocationThisNode || ev.Software.State != types.EvidenceOK {
		t.Fatalf("%+v", ev)
	}
	// A later regrouping replaces only the location.
	trail = append(trail, &types.AuditEntry{Action: "evidence", Params: map[string]string{"fault_location": `{"location":"shared","group":"g","members":["g","i"]}`}})
	ev = Latest(trail)
	if ev.Location.Location != types.LocationShared || ev.Location.Group != "g" || ev.Software.State != types.EvidenceOK {
		t.Fatalf("regrouped %+v", ev)
	}
	// A broken later block leaves the earlier one standing.
	trail = append(trail, &types.AuditEntry{Action: "evidence", Params: map[string]string{"incident_evidence": "broken"}})
	if ev := Latest(trail); ev == nil || ev.Software.State != types.EvidenceOK {
		t.Fatalf("broken block: %+v", ev)
	}
	if ev := Latest(nil); ev != nil {
		t.Fatal("no rows produced evidence")
	}
}
