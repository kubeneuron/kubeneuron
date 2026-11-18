-- SPDX-License-Identifier: Apache-2.0
CREATE TABLE training_progress (
    incident_id TEXT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    job_uid TEXT NOT NULL,
    pod_uid TEXT NOT NULL,
    step BIGINT NOT NULL CHECK (step >= 0),
    received_at TEXT NOT NULL,
    PRIMARY KEY (incident_id, job_uid, pod_uid)
);
