// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestSettleCloudOperation(t *testing.T) {
	at := time.Now()
	const reason = "checked group activity"
	for _, outcome := range []string{"", types.SuccessorHandedOver} {
		inc := &types.Incident{ID: "inc", Successor: &types.SuccessorRecord{Outcome: outcome}}
		if err := settleCloudOperation(inc, "  ", at); err == nil || inc.Successor.Outcome != outcome {
			t.Fatal("empty reason accepted or mutated plan")
		}
		if err := settleCloudOperation(inc, reason, at); err != nil {
			t.Fatal(err)
		}
		if s := inc.Successor; s.Outcome != "settled-by-operator" || s.Reason != reason || !s.EndedAt.Equal(at) {
			t.Fatalf("successor = %+v", s)
		}
		if err := settleCloudOperation(inc, reason, at); err == nil || err.Error() != "no open cloud operation for incident inc; nothing to settle" {
			t.Fatalf("settled plan: %v", err)
		}
	}
	for _, outcome := range []string{"", types.RemoveFirstNeedsOperator} {
		inc := &types.Incident{ID: "inc", RemoveFirst: &types.RemoveFirstRecord{Outcome: outcome}}
		if err := settleCloudOperation(inc, reason, at); err != nil {
			t.Fatal(err)
		}
		if r := inc.RemoveFirst; r.Outcome != "settled-by-operator" || r.Reason != reason || !r.EndedAt.Equal(at) {
			t.Fatalf("remove-first = %+v", r)
		}
	}
}
