// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"context"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// repinAfterRecycle commits the new pin and its evidence in one transaction.
// A crash before commit leaves the old pin intact; we never infer our action
// from a later boot change.
func (c *Controller) repinAfterRecycle(ctx context.Context, inc *types.Incident, result cloud.RecycleResult) error {
	if inc.CloudIdentity == nil {
		return store.ErrConflict
	}
	if err := result.Validate(*inc.CloudIdentity); err != nil {
		return err
	}
	var saved *types.Incident
	err := c.store.WithTx(ctx, func(tx store.Tx) error {
		fresh, err := tx.GetIncident(ctx, inc.ID)
		if err != nil {
			return err
		}
		if fresh.State != inc.State || !fresh.StateChangedAt.Equal(inc.StateChangedAt) || fresh.CloudIdentity == nil || *fresh.CloudIdentity != result.Before {
			return store.ErrConflict
		}
		next := result.After
		fresh.CloudIdentity = &next
		fresh.UpdatedAt = result.CompletedAt
		fresh.StateChangedAt = result.CompletedAt
		if err := tx.UpdateIncident(ctx, fresh); err != nil {
			return err
		}
		if err := tx.AppendAudit(ctx, &types.AuditEntry{
			IncidentID: inc.ID, Time: result.CompletedAt, FromState: fresh.State, ToState: fresh.State,
			Actor: "system", Action: "cloud-pin-updated", Result: "pin updated after verified recycle; recovery remains unverified",
			Params: map[string]string{"previous_boot_id": result.Before.BootID, "boot_id": result.After.BootID, "instance_id": result.After.InstanceID},
		}); err != nil {
			return err
		}
		saved = fresh
		return nil
	})
	if err == nil {
		*inc = *saved
	}
	return err
}
