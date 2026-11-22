// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"strings"
	"time"
)

// CloudSettlementHint explains why a resolved owner still holds its node.
func (inc *Incident) CloudSettlementHint() string {
	if inc.State != StateResolved {
		return ""
	}
	var kind, operation, pool string
	var started time.Time
	switch {
	case inc.Successor != nil && (inc.Successor.Open() || inc.Successor.Outcome == SuccessorHandedOver):
		s := inc.Successor
		kind, operation, pool, started = "replace", s.Operation, s.Pool, s.RequestedAt
	case inc.RemoveFirst != nil && (inc.RemoveFirst.Open() || inc.RemoveFirst.Outcome == RemoveFirstNeedsOperator):
		r := inc.RemoveFirst
		kind, operation, pool, started = "remove-first", r.Operation, r.Pool, r.PlannedAt
	default:
		return ""
	}
	detail := ""
	if !started.IsZero() {
		detail = " started at " + started.UTC().Format(time.RFC3339)
	}
	var identifiers []string
	if operation != "" {
		identifiers = append(identifiers, "operation "+operation)
	}
	if pool != "" {
		identifiers = append(identifiers, "pool "+pool)
	}
	if len(identifiers) > 0 {
		detail += " (" + strings.Join(identifiers, ", ") + ")"
	}
	provider := "your cloud provider"
	if inc.CloudIdentity != nil {
		switch {
		case strings.HasPrefix(inc.CloudIdentity.ProviderID, "aws://"):
			provider = "AWS"
		case strings.HasPrefix(inc.CloudIdentity.ProviderID, "gce://"):
			provider = "Google Cloud"
		case strings.HasPrefix(inc.CloudIdentity.ProviderID, "azure://"):
			provider = "Azure"
		}
	}
	return fmt.Sprintf("Repair of node %s is still blocked: the cloud %s operation%s has no confirmed outcome.\nCheck the operation in %s. Once you have verified that it completed or was canceled, run:\n  kubeneuronctl resolve %s --cloud-operation-settled --reason \"<what you verified>\"", inc.Target.Node, kind, detail, provider, inc.ID)
}
