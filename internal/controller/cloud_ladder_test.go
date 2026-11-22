// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"context"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"testing"
)

func TestCloudLadderIncludesEscalationAndTerminatesCycles(t *testing.T) {
	c, _ := stackTestController(t, &janitorPlatform{})
	first := &playbook.Playbook{Name: "first"}
	second := &playbook.Playbook{Name: "second"}
	first.OnFailure.EscalateTo = "second"
	second.OnFailure.EscalateTo = "first"
	books := map[string]*playbook.Playbook{"first": first, "second": second}
	engine, err := playbook.NewEngine(books, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)
	if c.cloudRecoveryLadder(context.Background(), "first") {
		t.Fatal("non-cloud cycle matched")
	}
	second.Steps = []playbook.Step{{Action: "platform.replace_node"}}
	if !c.cloudRecoveryLadder(context.Background(), "first") {
		t.Fatal("cloud escalation not detected")
	}
	if c.cloudRecoveryLadder(context.Background(), "missing") {
		t.Fatal("missing book matched")
	}
}
