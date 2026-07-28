-- 0010 — durable safety-gate state. Cooldowns and flap history previously
-- lived only in controller memory, so a restart (crash, deploy, TLS
-- rotation) reset exactly the protections most needed right after a crash.
-- Snapshots are small JSON documents keyed by kind ('cooldowns' | 'flap').

CREATE TABLE IF NOT EXISTS safety_state (
    kind       TEXT PRIMARY KEY,
    payload    TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
