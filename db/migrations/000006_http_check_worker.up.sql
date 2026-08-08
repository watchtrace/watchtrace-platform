ALTER TABLE check_jobs
    ADD COLUMN attempt_count smallint NOT NULL DEFAULT 0
        CHECK (attempt_count BETWEEN 0 AND 3),
    ADD COLUMN max_attempts smallint NOT NULL DEFAULT 3
        CHECK (max_attempts = 3),
    ADD COLUMN lease_owner text
        CHECK (lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 128)),
    ADD COLUMN lease_token uuid,
    ADD COLUMN lease_expires_at timestamptz,
    ADD CONSTRAINT check_jobs_attempt_limit_check
        CHECK (attempt_count <= max_attempts),
    ADD CONSTRAINT check_jobs_lease_consistency_check
        CHECK (
            (state = 'running'
                AND lease_owner IS NOT NULL
                AND lease_token IS NOT NULL
                AND lease_expires_at IS NOT NULL
                AND started_at IS NOT NULL
                AND completed_at IS NULL)
            OR
            (state <> 'running'
                AND lease_owner IS NULL
                AND lease_token IS NULL
                AND lease_expires_at IS NULL)
        );

CREATE INDEX check_jobs_claim_idx
    ON check_jobs (job_type, scheduled_at, id)
    WHERE state = 'pending';

CREATE TABLE health_checks (
    job_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    monitor_id uuid NOT NULL,
    job_type text NOT NULL
        CHECK (job_type IN ('scheduled', 'manual_test')),
    scheduled_at timestamptz NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz NOT NULL,
    succeeded boolean NOT NULL,
    status_code smallint
        CHECK (status_code BETWEEN 100 AND 599),
    error_category text
        CHECK (error_category IN (
            'invalid_target',
            'unsafe_target',
            'dns',
            'connection',
            'tls',
            'timeout',
            'http_protocol',
            'response_body',
            'unexpected_status'
        )),
    total_duration_microseconds bigint NOT NULL
        CHECK (total_duration_microseconds >= 0),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (organization_id, environment_id, monitor_id)
        REFERENCES monitors (organization_id, environment_id, id),
    CHECK (completed_at >= started_at),
    CHECK (
        (succeeded AND status_code IS NOT NULL AND error_category IS NULL)
        OR
        (NOT succeeded AND error_category IS NOT NULL)
    )
);

CREATE INDEX health_checks_monitor_history_idx
    ON health_checks (
        organization_id,
        environment_id,
        monitor_id,
        scheduled_at DESC,
        job_id
    );
