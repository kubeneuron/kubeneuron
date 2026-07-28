package actuator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// fake is a scriptable actuator for composition tests.
type fake struct {
	name      string
	caps      []types.ActionType
	healthErr error
	execErr   error
	executed  []string
}

func (f *fake) Name() string                              { return f.name }
func (f *fake) Capabilities() []types.ActionType          { return f.caps }
func (f *fake) Healthy(context.Context, types.Node) error { return f.healthErr }
func (f *fake) Execute(_ context.Context, _ types.Node, a types.Action) (*types.ActionResult, error) {
	f.executed = append(f.executed, a.ID)
	if f.execErr != nil {
		return nil, f.execErr
	}
	return &types.ActionResult{ActionID: a.ID, OK: true, Output: f.name}, nil
}

func TestDryRunNeverExecutesInner(t *testing.T) {
	inner := &fake{name: "agent", caps: []types.ActionType{types.ActionGPUReset}}
	d := &DryRun{Inner: inner}

	res, err := d.Execute(context.Background(), types.Node{Name: "n1"}, types.Action{ID: "a1", Type: types.ActionGPUReset})
	if err != nil {
		t.Fatal(err)
	}
	if len(inner.executed) != 0 {
		t.Fatal("dry-run must not execute the inner actuator")
	}
	if !res.OK || !strings.Contains(res.Output, "DRY-RUN") {
		t.Fatalf("dry-run result = %+v, want OK with DRY-RUN output", res)
	}
	if d.Name() != "dry-run(agent)" {
		t.Fatalf("Name = %q", d.Name())
	}
	if len(d.Capabilities()) != 1 {
		t.Fatal("dry-run must report inner capabilities for planning")
	}
}

func TestChainPrefersFirstHealthyCapable(t *testing.T) {
	agent := &fake{name: "agent", caps: []types.ActionType{types.ActionGPUReset, types.ActionReboot}}
	ssh := &fake{name: "ssh", caps: []types.ActionType{types.ActionReboot}}
	c := &Chain{Actuators: []Actuator{agent, ssh}}

	res, err := c.Execute(context.Background(), types.Node{Name: "n1"}, types.Action{ID: "a1", Type: types.ActionReboot})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "agent" {
		t.Fatalf("executed via %q, want agent first", res.Output)
	}
	if len(ssh.executed) != 0 {
		t.Fatal("fallback must not run when the primary is healthy")
	}
}

func TestChainFallsBackWhenPrimaryUnhealthy(t *testing.T) {
	agent := &fake{name: "agent", caps: []types.ActionType{types.ActionReboot}, healthErr: errors.New("node wedged")}
	ssh := &fake{name: "ssh", caps: []types.ActionType{types.ActionReboot}}
	c := &Chain{Actuators: []Actuator{agent, ssh}}

	res, err := c.Execute(context.Background(), types.Node{Name: "n1"}, types.Action{ID: "a1", Type: types.ActionReboot})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "ssh" {
		t.Fatalf("executed via %q, want ssh fallback", res.Output)
	}
}

func TestChainSkipsIncapableActuators(t *testing.T) {
	agent := &fake{name: "agent", caps: []types.ActionType{types.ActionGPUReset}}
	c := &Chain{Actuators: []Actuator{agent}}

	_, err := c.Execute(context.Background(), types.Node{Name: "n1"}, types.Action{ID: "a1", Type: types.ActionPowerCycle})
	if err == nil || !strings.Contains(err.Error(), "no actuator supports") {
		t.Fatalf("err = %v, want unsupported-action error", err)
	}
	if len(agent.executed) != 0 {
		t.Fatal("incapable actuator must not be executed")
	}
}

func TestChainCapabilitiesAreUnion(t *testing.T) {
	c := &Chain{Actuators: []Actuator{
		&fake{caps: []types.ActionType{types.ActionGPUReset, types.ActionReboot}},
		&fake{caps: []types.ActionType{types.ActionReboot, types.ActionCollectBundle}},
	}}
	if got := len(c.Capabilities()); got != 3 {
		t.Fatalf("union capabilities = %d, want 3 (deduplicated)", got)
	}
}
