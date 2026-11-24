// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
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
		return fmt.Errorf("Incident %s has no open cloud operation to mark as settled.", inc.ID) //nolint:staticcheck // Displayed verbatim by the CLI.
	}
	return nil
}

// ResolveSettledCloudOperation records the operator's attestation, including
// when a previous ordinary resolve already closed the incident.
func (c *Controller) ResolveSettledCloudOperation(ctx context.Context, id, actor, reason string, version int, key string) (*types.Incident, bool, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(key) == "" || strings.TrimSpace(reason) == "" {
		return nil, false, fmt.Errorf("actor, idempotency key and a reason describing what you verified are required")
	}
	digest := incidentOperationDigest("cloud-operation-settled", id, actor, reason, version)
	var inc *types.Incident
	var replay, held bool
	err := c.store.WithTx(ctx, func(tx store.Tx) error {
		op, ok := tx.(store.OperationalTx)
		if !ok {
			return fmt.Errorf("cloud settlement requires transactional idempotency")
		}
		record, created, err := op.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{Kind: types.ResourceIncidentOperation, Actor: actor, Key: incidentOperationKey("resolve", key), RequestDigest: digest, ResourceID: id})
		if err != nil {
			return err
		}
		if record.RequestDigest != digest || record.ResourceID != id {
			return store.ErrOperationalConflict
		}
		inc, err = tx.GetIncident(ctx, id)
		if err != nil {
			return err
		}
		if !created {
			replay = true
			return nil
		}
		if version > 0 && version != inc.Version {
			return store.ErrConflict
		}
		now := time.Now()
		from := inc.State
		s := inc.Successor
		if !from.Halted() && s.Open() {
			return fmt.Errorf("the current replacement is still active; check the incident again when it needs an operator")
		}
		if !from.Halted() && s != nil && s.Outcome == types.SuccessorHandedOver {
			if s.Operation != "" && s.Operation != successorOperation(inc) {
				s.Outcome, s.Reason, s.EndedAt = "settled-by-operator", reason, &now
				inc.UpdatedAt = now
				if err := tx.UpdateIncident(ctx, inc); err != nil {
					return err
				}
				return tx.AppendAudit(ctx, &types.AuditEntry{IncidentID: id, Time: now, FromState: from, ToState: from, Actor: actor, Action: "earlier-successor-settled", Result: reason, Params: map[string]string{"operation": s.Operation}})
			}
			// A restart may leave the handover durable before quarantine. Record
			// that transition and the operator's resolution in one transaction.
			// This is the sole direct NEEDS_HUMAN transition: the operator is
			// already settling the record, and no intermediate state is published.
			if err := playbook.Transition(inc, types.StateNeedsHuman); err != nil {
				return err
			}
			if err := tx.AppendAudit(ctx, &types.AuditEntry{IncidentID: id, Time: now, FromState: from, ToState: inc.State, Actor: "system", Action: "successor-handover-confirmed", Params: map[string]string{"operation": s.Operation}}); err != nil {
				return err
			}
			from = inc.State
		}
		if err := settleCloudOperation(inc, reason, now); err != nil {
			return err
		}
		if from != types.StateResolved {
			if err := playbook.Transition(inc, types.StateResolved); err != nil {
				return err
			}
			inc.ResolvedAt = &now
		}
		inc.UpdatedAt, inc.StateChangedAt = now, now
		held, inc.RemediationSlotHeld = inc.RemediationSlotHeld, false
		if err := tx.UpdateIncident(ctx, inc); err != nil {
			return err
		}
		if err := tx.ReleaseRepairFollowers(ctx, id, now, reason); err != nil {
			return err
		}
		if err := tx.ReleaseNodeRemediation(ctx, id); err != nil {
			return err
		}
		if err := tx.ReleaseRepairStep(ctx, id); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, &types.AuditEntry{IncidentID: id, Time: now, FromState: from, ToState: types.StateResolved, Actor: actor, Action: "cloud-operation-settled", Result: reason})
	})
	if err != nil {
		return nil, false, err
	}
	if held {
		c.gate.ReleaseRemediation(inc.Target)
	}
	return inc, replay, nil
}
