// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// An operation handed to an operator is closed to automatic reconciliation,
// but still needs an explicit account of its outcome before repair can resume.
func cloudOperationUnsettled(inc *types.Incident) bool {
	return inc.Successor.Open() || (inc.Successor != nil && inc.Successor.Outcome == types.SuccessorHandedOver) ||
		inc.RemoveFirst.Open() || (inc.RemoveFirst != nil && inc.RemoveFirst.Outcome == types.RemoveFirstNeedsOperator)
}

func settleCloudOperation(inc *types.Incident, reason string, at time.Time) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("cloud operation settlement requires a reason describing what you checked")
	}
	settled := false
	if s := inc.Successor; s != nil && (s.Open() || s.Outcome == types.SuccessorHandedOver) {
		s.Outcome, s.Reason, s.EndedAt = "settled-by-operator", reason, &at
		settled = true
	}
	if r := inc.RemoveFirst; r != nil && (r.Open() || r.Outcome == types.RemoveFirstNeedsOperator) {
		r.Outcome, r.Reason, r.EndedAt = "settled-by-operator", reason, &at
		settled = true
	}
	if !settled {
		return fmt.Errorf("Incident %s has no open cloud operation to mark as settled.", inc.ID)
	}
	return nil
}
