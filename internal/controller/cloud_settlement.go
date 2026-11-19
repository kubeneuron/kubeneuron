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
		return fmt.Errorf("no open cloud operation for incident %s; nothing to settle", inc.ID)
	}
	return nil
}
