-- 0013 — serialize each v0.4.0 audit hash chain across concurrent writers.

CREATE TABLE IF NOT EXISTS operational_audit_heads (
    kind        TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    last_hash   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (kind, resource_id)
);

INSERT INTO operational_audit_heads (kind, resource_id, last_hash)
SELECT latest.kind, latest.resource_id, latest.hash
FROM operational_audit AS latest
JOIN (
    SELECT kind, resource_id, MAX(id) AS max_id
    FROM operational_audit
    GROUP BY kind, resource_id
) AS grouped
  ON grouped.kind = latest.kind
 AND grouped.resource_id = latest.resource_id
 AND grouped.max_id = latest.id
ON CONFLICT (kind, resource_id) DO NOTHING;
