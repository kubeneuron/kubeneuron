// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// notifyAddFirstHold records the intent before dispatch. A crash after Notify
// but before its acknowledgement can repeat the notification on the next tick.
func (c *Controller) notifyAddFirstHold(ctx context.Context, inc *types.Incident, step, reason string) error {
	result := step + ": " + reason
	c.policyAssessedMu.Lock()
	defer c.policyAssessedMu.Unlock()
	send := false
	err := c.store.WithTx(ctx, func(tx store.Tx) error {
		trail, err := tx.AuditTrail(ctx, inc.ID)
		if err != nil {
			return err
		}
		lastAssessment := types.RecoveryAvailabilityFromAudit(trail)
		appendRow := func(action, message string) error {
			return tx.AppendAudit(ctx, &types.AuditEntry{IncidentID: inc.ID, Time: time.Now(), Actor: "system", Action: action, Result: message, FromState: inc.State, ToState: inc.State, DryRun: inc.DryRun})
		}
		if lastAssessment == nil || lastAssessment.Source != "policy" || lastAssessment.Status != "unavailable" || lastAssessment.Reason != result {
			if err := appendRow("recovery-unavailable", result); err != nil {
				return err
			}
		}
		for i := len(trail) - 1; i >= 0; i-- {
			e := trail[i]
			if e.Result != reason {
				continue
			}
			switch e.Action {
			case "notified":
				return nil
			case "notify-pending":
				send = true
				return nil
			}
		}
		send = true
		return appendRow("notify-pending", reason)
	})
	if err != nil {
		return err
	}
	delete(c.policyAssessed, inc.ID)
	if !send {
		return nil
	}
	msg := reason + ". Make this pool eligible for add-first or explicitly select remove-first in its replacement policy."
	if err := c.notify(ctx, notify.NotifyEvent{Incident: inc, Kind: notify.EventNeedsHuman, Message: msg}); err != nil {
		return err
	}
	return c.appendAudit(ctx, inc, "system", "notified", reason)
}
