ALTER TABLE monitors
    ADD COLUMN next_check_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP;

ALTER TABLE monitors
    ADD CONSTRAINT monitors_tenant_environment_id_key
    UNIQUE (organization_id, environment_id, id);

CREATE INDEX monitors_due_schedule_idx
    ON monitors (next_check_at, id);

CREATE TABLE check_jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    monitor_id uuid NOT NULL,
    job_type text NOT NULL DEFAULT 'scheduled'
        CHECK (job_type IN ('scheduled', 'manual_test')),
    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'running', 'completed', 'cancelled', 'dead')),
    scheduled_at timestamptz NOT NULL,
    started_at timestamptz,
    completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (monitor_id, scheduled_at),
    FOREIGN KEY (organization_id, environment_id, monitor_id)
        REFERENCES monitors (organization_id, environment_id, id)
);

CREATE INDEX check_jobs_pending_schedule_idx
    ON check_jobs (scheduled_at, id)
    WHERE state = 'pending';

CREATE INDEX check_jobs_monitor_history_idx
    ON check_jobs (organization_id, environment_id, monitor_id, scheduled_at DESC, id);

CREATE UNIQUE INDEX check_jobs_one_outstanding_scheduled_idx
    ON check_jobs (monitor_id)
    WHERE job_type = 'scheduled' AND state IN ('pending', 'running');
