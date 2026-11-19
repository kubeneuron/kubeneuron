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
func (c *Controller) notifyAddFirstHold(ctx context.Context, inc *types.Incident, step, reason, action string) error {
	// A concurrent tick can retry once this incident's dispatch has finished.
	if _, busy := c.holdDispatches.LoadOrStore(inc.ID, struct{}{}); busy {
		return nil
	}
	defer c.holdDispatches.Delete(inc.ID)
	result := step + ": " + reason
	c.policyAssessedMu.Lock()
	newHold := false
	send := false
	err := c.store.WithTx(ctx, func(tx store.Tx) error {
		trail, err := tx.AuditTrail(ctx, inc.ID)
		if err != nil {
			return err
		}

		appendRow := func(action, message string) error {
			return tx.AppendAudit(ctx, &types.AuditEntry{IncidentID: inc.ID, Time: time.Now(), Actor: "system", Action: action, Result: message, FromState: inc.State, ToState: inc.State, DryRun: inc.DryRun})
		}
		start := len(trail)
		for start > 0 {
			e := trail[start-1]
			if e.Action == "recovery-unavailable" || e.Action == "recovery-available" || e.Action == "recovery-available-again" {
				break
			}
			start--
		}
		newHold = start == 0 || trail[start-1].Action != "recovery-unavailable" || trail[start-1].Result != result
		if newHold {
			if err := appendRow("recovery-unavailable", result); err != nil {
				return err
			}
		} else {
			for i := len(trail) - 1; i >= start; i-- {
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
		}
		send = true
		return appendRow("notify-pending", reason)
	})
	if err == nil {
		delete(c.policyAssessed, inc.ID)
	}
	c.policyAssessedMu.Unlock()
	if err != nil {
		return err
	}
	if newHold {
		c.log.Warn("step held: recovery unavailable", "incident", inc.ID, "step", step, "reason", reason)
	}
	if !send {
		return nil
	}
	msg := reason + ". " + action
	if err := c.notify(ctx, notify.NotifyEvent{Incident: inc, Kind: notify.EventNeedsHuman, Message: msg}); err != nil {
		return err
	}
	return c.appendAudit(ctx, inc, "system", "notified", reason)
}
