CREATE TABLE monitor_reliability_states (
    monitor_id uuid PRIMARY KEY REFERENCES monitors (id) ON DELETE CASCADE,
    display_state text NOT NULL DEFAULT 'unknown'
      CHECK (display_state IN ('unknown','healthy','degraded','down')),
    observed_state text NOT NULL DEFAULT 'unknown'
      CHECK (observed_state IN ('unknown','healthy','degraded','down')),
    consecutive_failures smallint NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 3),
    consecutive_successes smallint NOT NULL DEFAULT 0 CHECK (consecutive_successes BETWEEN 0 AND 2),
    last_observed_scheduled_at timestamptz,
    last_observed_job_id uuid,
    newest_expected_scheduled_at timestamptz,
    last_evaluated_scheduled_at timestamptz,
    last_evaluated_job_id uuid,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK ((last_observed_scheduled_at IS NULL) = (last_observed_job_id IS NULL))
);

CREATE TABLE monitor_result_evaluations (
    monitor_id uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    job_id uuid PRIMARY KEY REFERENCES health_checks (job_id) ON DELETE CASCADE,
    scheduled_at timestamptz NOT NULL,
    succeeded boolean NOT NULL,
    observed_state text NOT NULL CHECK (observed_state IN ('healthy','degraded','down')),
    consecutive_failures smallint NOT NULL CHECK (consecutive_failures BETWEEN 0 AND 3),
    consecutive_successes smallint NOT NULL CHECK (consecutive_successes BETWEEN 0 AND 2),
    evaluated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (monitor_id, scheduled_at, job_id)
);
CREATE INDEX monitor_result_evaluations_order_idx
    ON monitor_result_evaluations (monitor_id, scheduled_at, job_id);

CREATE TABLE monitor_state_correction_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    monitor_id uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    accepted_job_id uuid NOT NULL,
    corrected_from timestamptz NOT NULL,
    previous_display_state text NOT NULL CHECK (previous_display_state IN ('unknown','healthy','degraded','down')),
    corrected_display_state text NOT NULL CHECK (corrected_display_state IN ('unknown','healthy','degraded','down')),
    safe_reason text NOT NULL DEFAULT 'late_result_correction'
      CHECK (safe_reason = 'late_result_correction'),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (monitor_id, accepted_job_id)
);

CREATE TABLE monitor_rollup_invalidations (
    monitor_id uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    bucket_kind text NOT NULL CHECK (bucket_kind IN ('hourly','daily')),
    bucket_start timestamptz NOT NULL,
    reason text NOT NULL CHECK (reason IN ('accepted_result','late_result')),
    invalidated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (monitor_id, bucket_kind, bucket_start),
    CHECK (
      (bucket_kind = 'hourly' AND bucket_start = date_trunc('hour', bucket_start)) OR
      (bucket_kind = 'daily' AND bucket_start = date_trunc('day', bucket_start))
    )
);
CREATE INDEX monitor_rollup_invalidations_order_idx
    ON monitor_rollup_invalidations (bucket_kind, bucket_start, monitor_id);

CREATE TABLE monitoring_rollup_checkpoint (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    hourly_through timestamptz NOT NULL,
    daily_through date NOT NULL,
    last_retention_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO monitoring_rollup_checkpoint(singleton,hourly_through,daily_through)
SELECT true,
       date_trunc('hour', GREATEST(COALESCE(min(starts_at),CURRENT_TIMESTAMP),CURRENT_TIMESTAMP-INTERVAL '7 days'))-INTERVAL '1 hour',
       GREATEST(COALESCE(min(starts_at),CURRENT_TIMESTAMP)::date,CURRENT_DATE-90)-1
FROM monitor_schedule_periods;

INSERT INTO monitor_reliability_states(monitor_id)
SELECT id FROM monitors;
