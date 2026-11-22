// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestAuthorisationsNamePolicyOrPerson(t *testing.T) {
	trail := []*types.AuditEntry{
		{FromState: types.StateEvaluating, ToState: types.StateExecuting, Actor: "system", Action: "observe"},
		{FromState: types.StateEvaluating, ToState: types.StateExecuting, Actor: "policy:auto-dbe", Action: "drain"},
		{FromState: types.StateAwaitingApproval, ToState: types.StateExecuting, Actor: "token:alice", Action: "replace"},
	}
	got := strings.Join(authorisations(trail), "\n")
	if strings.Contains(got, "observe") {
		t.Fatalf("a step that needs no approval is listed:\n%s", got)
	}
	if !strings.Contains(got, "drain        authorised by policy auto-dbe") || !strings.Contains(got, "replace      approved by token:alice") {
		t.Fatalf("authorisations:\n%s", got)
	}
}
