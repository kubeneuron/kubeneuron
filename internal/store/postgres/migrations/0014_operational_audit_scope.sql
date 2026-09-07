-- 0014 — preserve tenant/cluster scope in the global v0.4.0 audit explorer.

ALTER TABLE operational_audit ADD COLUMN tenant TEXT NOT NULL DEFAULT '';
ALTER TABLE operational_audit ADD COLUMN cluster TEXT NOT NULL DEFAULT '';

UPDATE operational_audit AS event
SET tenant = resource.tenant,
    cluster = resource.cluster
FROM operational_resources AS resource
WHERE resource.kind = event.kind
  AND resource.id = event.resource_id
  AND event.tenant = ''
  AND event.cluster = '';

CREATE INDEX IF NOT EXISTS idx_operational_audit_scope
    ON operational_audit (tenant, cluster, id);
