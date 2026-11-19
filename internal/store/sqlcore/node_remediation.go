// SPDX-License-Identifier: Apache-2.0

package sqlcore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// ClaimNodeRemediation records the first owner or links the incident to the
// existing owner. The caller's transaction also records any change of step.
func (q *Queries) ClaimNodeRemediation(ctx context.Context, node, uid, incidentID string) (string, error) {
	// A follower must verify its original owner's outcome, even if the node
	// becomes available between reconciles or during a leadership change.
	var owner string
	err := q.db.QueryRowContext(ctx, "SELECT owner_id FROM node_remediation_followers WHERE incident_id=?", incidentID).Scan(&owner)
	if err == nil {
		return owner, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if _, err := q.db.ExecContext(ctx, `INSERT INTO node_remediation_owners (node, node_uid, incident_id)
 VALUES (?, ?, ?) ON CONFLICT (node, node_uid) DO NOTHING`, node, uid, incidentID); err != nil {
		return "", err
	}
	if err := q.db.QueryRowContext(ctx, `SELECT incident_id FROM node_remediation_owners WHERE node=? AND node_uid=?`, node, uid).Scan(&owner); err != nil {
		return "", err
	}
	if owner != incidentID {
		res, err := q.db.ExecContext(ctx, `INSERT INTO node_remediation_followers (incident_id, owner_id)
 VALUES (?, ?) ON CONFLICT (incident_id) DO NOTHING`, incidentID, owner)
		if err != nil {
			return "", err
		}
		inserted, err := res.RowsAffected()
		if err != nil {
			return "", err
		}
		if inserted == 0 {
			err := q.db.QueryRowContext(ctx, "SELECT owner_id FROM node_remediation_followers WHERE incident_id=?", incidentID).Scan(&owner)
			return owner, err
		}
		inc, err := q.GetIncident(ctx, incidentID)
		if err != nil {
			return "", err
		}
		err = q.AppendAudit(ctx, &types.AuditEntry{
			IncidentID: incidentID, Time: time.Now(), FromState: inc.State, ToState: inc.State,
			Actor: "system", Action: "wait-for-node-repair", Params: map[string]string{"owner_id": owner},
			Result: "node repair is owned by incident " + owner,
		})
		if err != nil {
			return "", err
		}
	}
	return owner, nil
}

// ReleaseNodeRemediation drops only the active owner's claim. Followers keep
// their persisted link so they can verify the outcome independently.
func (q *Queries) ReleaseNodeRemediation(ctx context.Context, incidentID string) error {
	_, err := q.db.ExecContext(ctx, "DELETE FROM node_remediation_owners WHERE incident_id=?", incidentID)
	return err
}

// ReleaseRepairFollowers lets each waiting incident seek a new authorization
// when approval ended before the owner executed a step.
func (q *Queries) ReleaseRepairFollowers(ctx context.Context, ownerID string, at time.Time, reason string) error {
	rows, err := q.db.QueryContext(ctx, "DELETE FROM node_remediation_followers WHERE owner_id=? RETURNING incident_id", ownerID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		inc, err := q.GetIncident(ctx, id)
		if err != nil {
			return err
		}
		if err := q.AppendAudit(ctx, &types.AuditEntry{IncidentID: id, Time: at, Actor: "system", Action: "node-repair-released", FromState: inc.State, ToState: inc.State, Params: map[string]string{"owner_id": ownerID}, Result: "previous owner released repair before execution: " + reason}); err != nil {
			return err
		}
	}
	return nil
}
