// SPDX-License-Identifier: Apache-2.0

package sqlcore

import "context"

// ClaimNodeRemediation records the first owner or links the incident to the
// existing owner. The caller's transaction also records any change of step.
func (q *Queries) ClaimNodeRemediation(ctx context.Context, node, uid, incidentID string) (string, error) {
	if _, err := q.db.ExecContext(ctx, `INSERT INTO node_remediation_owners (node, node_uid, incident_id)
 VALUES (?, ?, ?) ON CONFLICT (node, node_uid) DO NOTHING`, node, uid, incidentID); err != nil {
		return "", err
	}
	var owner string
	if err := q.db.QueryRowContext(ctx, `SELECT incident_id FROM node_remediation_owners WHERE node=? AND node_uid=?`, node, uid).Scan(&owner); err != nil {
		return "", err
	}
	if owner != incidentID {
		_, err := q.db.ExecContext(ctx, `INSERT INTO node_remediation_followers (incident_id, owner_id)
 VALUES (?, ?) ON CONFLICT (incident_id) DO UPDATE SET owner_id=excluded.owner_id`, incidentID, owner)
		if err != nil {
			return "", err
		}
	}
	return owner, nil
}
