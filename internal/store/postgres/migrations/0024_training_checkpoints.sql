-- SPDX-License-Identifier: Apache-2.0
CREATE TABLE training_checkpoint_facts (
    incident_id TEXT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    job_uid TEXT NOT NULL,
    pod_uid TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('checkpoint', 'resume')),
    checkpoint_id TEXT NOT NULL,
    step BIGINT CHECK (step >= 0),
    received_at TEXT NOT NULL,
    PRIMARY KEY (incident_id, job_uid, pod_uid, kind, checkpoint_id)
);
