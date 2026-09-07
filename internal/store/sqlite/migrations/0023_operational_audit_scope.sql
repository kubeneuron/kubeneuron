-- 0023 — preserve tenant/cluster scope in the global v0.4.0 audit explorer.
--
-- A hash-chain event can outlive the resource summary it describes, so scope
-- cannot be recovered reliably from a later payload decode. Persist it beside
-- the immutable event and backfill the v0.4.0 rows that already have an
-- operational resource envelope.

ALTER TABLE operational_audit ADD COLUMN tenant TEXT NOT NULL DEFAULT '';
ALTER TABLE operational_audit ADD COLUMN cluster TEXT NOT NULL DEFAULT '';

UPDATE operational_audit AS event
SET tenant = COALESCE((
        SELECT resource.tenant
        FROM operational_resources AS resource
        WHERE resource.kind = event.kind AND resource.id = event.resource_id
    ), ''),
    cluster = COALESCE((
        SELECT resource.cluster
        FROM operational_resources AS resource
        WHERE resource.kind = event.kind AND resource.id = event.resource_id
    ), '')
WHERE event.tenant = '' AND event.cluster = '';

CREATE INDEX IF NOT EXISTS idx_operational_audit_scope
    ON operational_audit (tenant, cluster, id);
