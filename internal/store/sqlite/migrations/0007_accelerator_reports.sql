-- 0007 — latest agent-reported accelerator runtime profile per node/vendor.
--
-- Reports are intentionally separate from the legacy nodes.gpus inventory:
-- a node can expose more than one accelerator vendor/runtime, and the agent
-- owns the detailed topology and declared semantic capabilities. The primary
-- key makes this a latest-value store, while observed_at_ns is used by the
-- write path to reject an out-of-order observation rather than silently
-- rolling the controller back to an older safety profile.

CREATE TABLE IF NOT EXISTS accelerator_reports (
    node              TEXT NOT NULL,
    vendor            TEXT NOT NULL,
    observed_at_ns    INTEGER NOT NULL,
    profile_digest    TEXT NOT NULL DEFAULT '',
    readiness         TEXT NOT NULL,
    reasons_json      TEXT NOT NULL DEFAULT '[]',
    devices_json      TEXT NOT NULL DEFAULT '[]',
    driver_version    TEXT NOT NULL DEFAULT '',
    runtime_version   TEXT NOT NULL DEFAULT '',
    topology_safety   TEXT NOT NULL,
    capabilities_json TEXT NOT NULL DEFAULT '[]',
    PRIMARY KEY (node, vendor)
);

CREATE INDEX IF NOT EXISTS idx_accelerator_reports_node
    ON accelerator_reports (node, vendor);
