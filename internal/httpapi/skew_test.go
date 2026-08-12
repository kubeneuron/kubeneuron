package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// TestResultBodyStaysDecodableByAnOlderController is the rolling-upgrade
// contract. The result route strict-decodes, so ANY new field on this body is
// a 400 from every controller that predates it — on rollback, and whenever
// agents are upgraded before controllers. The refusal code therefore travels
// in a header; this test fails the build if it ever creeps back into the body.
func TestResultBodyStaysDecodableByAnOlderController(t *testing.T) {
	body, err := json.Marshal(types.ActionResult{
		ActionID: "a1", OK: false, Error: "busy", Refusal: types.RefusalNotIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the shape a pre-round-13 controller decodes into.
	type oldResult struct {
		ActionID   string `json:"action_id"`
		OK         bool   `json:"ok"`
		Output     string `json:"output,omitempty"`
		Error      string `json:"error,omitempty"`
		StartedAt  string `json:"started_at"`
		FinishedAt string `json:"finished_at"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	var old oldResult
	if err := dec.Decode(&old); err != nil {
		t.Fatalf("an older controller cannot decode this result body: %v\n"+
			"the result route strict-decodes, so a new field here breaks every "+
			"rollback and any upgrade that reaches agents first", err)
	}
}
