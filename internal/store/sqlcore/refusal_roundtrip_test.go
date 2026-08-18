package sqlcore

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// TestRefusalSurvivesTheStore pins the field that the durable action queue
// used to destroy.
//
// The agent sets ActionResult.Refusal, the API accepts it from
// AgentActionRefusalHeader, and the controller then reads the result back out
// of the store — agentrpc.Execute polls GetAction, it never sees the agent's
// HTTP response. Because Refusal is `json:"-"` for wire-compatibility reasons,
// marshalling the result straight into the actions table dropped it, so the
// controller's idleGuardRefused could never be true and the
// destructive_steps_deferred counter was pinned at zero.
//
// The covering test for that feature used an in-memory fake actuator that
// returned the struct directly, so it passed on a path production never takes.
// This one goes through the encoding the store actually uses.
func TestRefusalSurvivesTheStore(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	res := types.ActionResult{
		ActionID:   "act-1",
		OK:         false,
		Output:     "GPU 0 is still held by nvidia-device-plugin(11621)",
		StartedAt:  now,
		FinishedAt: now,
		Refusal:    types.RefusalNotIdle,
	}

	blob, err := marshalActionResult(res)
	if err != nil {
		t.Fatal(err)
	}
	var got types.ActionResult
	if err := unmarshalActionResult(string(blob), &got); err != nil {
		t.Fatal(err)
	}
	if got.Refusal != types.RefusalNotIdle {
		t.Fatalf("Refusal after a store round-trip = %q, want %q — "+
			"the controller reads results from the store, so an empty code here means "+
			"the idle-guard refusal metric can never fire", got.Refusal, types.RefusalNotIdle)
	}
	if got.Output != res.Output || got.OK != res.OK || !got.StartedAt.Equal(res.StartedAt) {
		t.Fatalf("the rest of the result did not survive: %+v", got)
	}
}

// TestStoredResultKeepsTheWireShape guards the reason Refusal is `json:"-"` in
// the first place. The stored blob may carry the extra key; the type that
// crosses the network must not, or a newer agent posting to an older
// controller gets a 400 from a strict decoder on every result it sends.
func TestStoredResultKeepsTheWireShape(t *testing.T) {
	wire, err := json.Marshal(types.ActionResult{ActionID: "a", Refusal: types.RefusalNotIdle})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["refusal"]; present {
		t.Fatal("types.ActionResult now serialises a refusal key on the wire; " +
			"the result route strict-decodes, so this 400s every result a newer agent posts to an older controller")
	}
}

// TestOlderBlobsStillDecode covers the rows already on disk: results written
// before the refusal key existed must read back cleanly with an empty code.
func TestOlderBlobsStillDecode(t *testing.T) {
	legacy := `{"action_id":"act-0","ok":true,"output":"done"}`
	var got types.ActionResult
	if err := unmarshalActionResult(legacy, &got); err != nil {
		t.Fatalf("a result written by an earlier build no longer decodes: %v", err)
	}
	if got.ActionID != "act-0" || !got.OK || got.Refusal != "" {
		t.Fatalf("legacy result decoded wrong: %+v", got)
	}
}
