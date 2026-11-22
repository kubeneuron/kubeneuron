// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const waitingForCloudIdentity = "KubeNeuron could not match this node to a cloud instance yet, so it cannot be replaced — the cloud instance could not be identified when the incident opened; KubeNeuron checks again every minute"

func recreatedCloudNodeReason(incidentID string) string {
	return fmt.Sprintf("KubeNeuron could not match this node to a cloud instance, so it cannot be replaced — the node was re-created under the same name after the incident opened, so it is no longer the node the fault was seen on; resolve this incident with kubeneuronctl resolve %s --actor <who>", incidentID)
}

func canMatchCloudIdentity(inc *types.Incident) bool {
	return inc.CloudIdentity == nil && inc.RepairNodeUID != "" && inc.State == types.StateEvaluating &&
		inc.ApprovalEpoch == 0 && !inc.RemediationSlotHeld && inc.Successor == nil && inc.RemoveFirst == nil
}

// Called only by the leader's paced pre-authorization assessment.
func (c *Controller) backfillCloudIdentity(ctx context.Context, inc *types.Incident) (string, error) {
	p, ok := c.platform.(interface {
		CloudRecyclingConfigured() bool
		MatchCloudNodeUID(context.Context, string, string) (cloud.NodeIdentity, error)
	})
	if !ok || !p.CloudRecyclingConfigured() || !canMatchCloudIdentity(inc) {
		return "", nil
	}
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil {
		return "", err
	}
	for _, event := range trail {
		if event.Action == "provider-recovery-unavailable" && event.Result == recreatedCloudNodeReason(inc.ID) {
			return event.Result, nil
		}
		if event.ToState == types.StateExecuting || event.ToState == types.StateAwaitingApproval || event.Action == "policy-authorised" {
			return "", nil
		}
	}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	pin, err := p.MatchCloudNodeUID(readCtx, inc.Target.Node, inc.RepairNodeUID)
	if errors.Is(err, cloud.ErrIdentityChanged) {
		return recreatedCloudNodeReason(inc.ID), nil
	}
	if err != nil || pin.Validate() != nil || pin.Node != inc.Target.Node || pin.NodeUID != inc.RepairNodeUID {
		return waitingForCloudIdentity, nil
	}
	return "", c.recordLateCloudIdentity(ctx, inc, pin)
}

func (c *Controller) recordLateCloudIdentity(ctx context.Context, inc *types.Incident, pin cloud.NodeIdentity) error {
	// The version covers the state, step, approval epoch and plans observed
	// before the GET. A racing leader or a manual resolve forces a fresh tick.
	next := *inc
	next.CloudIdentity = &pin
	next.UpdatedAt = time.Now()
	err := c.store.WithTx(ctx, func(tx store.Tx) error {
		if err := tx.UpdateIncident(ctx, &next); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, &types.AuditEntry{
			IncidentID: next.ID, Time: next.UpdatedAt, FromState: next.State, ToState: next.State,
			Actor: "system", Action: "cloud-instance-matched", DryRun: next.DryRun,
			Result: "cloud instance matched after the incident opened: the Kubernetes node is the same object (same UID) as when the incident opened; boot ID recorded at match time",
		})
	})
	if err == nil {
		*inc = next
	}
	return err
}
