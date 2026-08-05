package agentrpc

import (
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// An action the agent can execute but the actuator does not advertise is
// undispatchable: the controller refuses it with "no actuator supports ...".
// That is how the accelerator-host quiesce failed on real hardware after it was
// implemented — the executor handled it, nothing declared it. Every action the
// agent executes belongs in this list.
func TestCapabilitiesCoverEveryAgentExecutedAction(t *testing.T) {
	declared := map[types.ActionType]bool{}
	for _, a := range (&Actuator{}).Capabilities() {
		declared[a] = true
	}
	for _, want := range []types.ActionType{
		types.ActionIdleCheck,
		types.ActionWaitIdle,
		types.ActionGPUReset,
		types.ActionRunDiag,
		types.ActionCollectBundle,
		types.ActionReboot,
		types.ActionDriverReload,
		types.ActionDriverReinstall,
		types.ActionRunScript,
		types.ActionQuiesceAcceleratorHost,
		types.ActionRestoreAcceleratorHost,
	} {
		if !declared[want] {
			t.Errorf("action %q is executed by the agent but not advertised; the controller cannot dispatch it", want)
		}
	}
}
