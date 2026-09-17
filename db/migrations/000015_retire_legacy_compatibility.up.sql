-- The wt_local_ issuer was replaced on 2026-08-08 and issued 15-minute access
-- sessions. Stop rather than retire its parser if an unexpectedly long-lived
-- pre-cutover session is still active.
DO $$
BEGIN
  IF EXISTS (
      SELECT 1
      FROM auth_sessions
      WHERE created_at < TIMESTAMPTZ '2026-08-08 18:00:00+00'
        AND expires_at > CURRENT_TIMESTAMP
        AND revoked_at IS NULL
  ) THEN
    RAISE EXCEPTION 'legacy access-session expiry audit failed';
  END IF;
END
$$;

-- Historical monitor results can predate the ordered evaluator introduced in
-- migration 12. Backfill those monitors before the API stops deriving state
-- from the latest raw result.
INSERT INTO monitor_reliability_states (monitor_id)
SELECT id FROM monitors
ON CONFLICT DO NOTHING;

ALTER TABLE monitor_result_evaluations
    DROP CONSTRAINT monitor_result_evaluations_consecutive_failures_check,
    DROP CONSTRAINT monitor_result_evaluations_consecutive_successes_check,
    ADD CONSTRAINT monitor_result_evaluations_consecutive_failures_check
      CHECK (consecutive_failures BETWEEN 0 AND 20),
    ADD CONSTRAINT monitor_result_evaluations_consecutive_successes_check
      CHECK (consecutive_successes BETWEEN 0 AND 20);

CREATE TEMP TABLE legacy_reliability_backfill_targets
ON COMMIT DROP AS
SELECT states.monitor_id
FROM monitor_reliability_states AS states
WHERE states.last_observed_scheduled_at IS NULL
  AND EXISTS (
      SELECT 1
      FROM health_checks
      WHERE health_checks.monitor_id = states.monitor_id
        AND health_checks.job_type = 'scheduled'
  );

WITH RECURSIVE ordered_results AS (
    SELECT health_checks.monitor_id,
           health_checks.job_id,
           health_checks.scheduled_at,
           health_checks.succeeded,
           row_number() OVER (
               PARTITION BY health_checks.monitor_id
               ORDER BY health_checks.scheduled_at, health_checks.job_id
           ) AS result_number,
           COALESCE(alert_rules.failure_threshold, 3)::smallint AS failure_threshold,
           COALESCE(alert_rules.recovery_threshold, 2)::smallint AS recovery_threshold
    FROM health_checks
    JOIN legacy_reliability_backfill_targets AS targets
      ON targets.monitor_id = health_checks.monitor_id
    LEFT JOIN alert_rules
      ON alert_rules.monitor_id = health_checks.monitor_id
     AND alert_rules.rule_key = 'consecutive_failures'
     AND alert_rules.enabled
    WHERE health_checks.job_type = 'scheduled'
), evaluated AS (
    SELECT ordered_results.monitor_id,
           ordered_results.job_id,
           ordered_results.scheduled_at,
           ordered_results.succeeded,
           ordered_results.result_number,
           ordered_results.failure_threshold,
           ordered_results.recovery_threshold,
           CASE
             WHEN ordered_results.succeeded THEN 'healthy'
             WHEN ordered_results.failure_threshold = 1 THEN 'down'
             ELSE 'degraded'
           END::text AS observed_state,
           CASE
             WHEN ordered_results.succeeded THEN 0
             ELSE LEAST(1, ordered_results.failure_threshold)
           END::smallint AS consecutive_failures,
           0::smallint AS consecutive_successes
    FROM ordered_results
    WHERE ordered_results.result_number = 1

    UNION ALL

    SELECT current.monitor_id,
           current.job_id,
           current.scheduled_at,
           current.succeeded,
           current.result_number,
           current.failure_threshold,
           current.recovery_threshold,
           CASE
             WHEN current.succeeded
                  AND previous.observed_state = 'down'
                  AND previous.consecutive_successes + 1 < current.recovery_threshold
               THEN 'down'
             WHEN current.succeeded THEN 'healthy'
             WHEN LEAST(previous.consecutive_failures + 1, current.failure_threshold)
                    >= current.failure_threshold
               THEN 'down'
             ELSE 'degraded'
           END::text AS observed_state,
           CASE
             WHEN current.succeeded THEN 0
             ELSE LEAST(previous.consecutive_failures + 1, current.failure_threshold)
           END::smallint AS consecutive_failures,
           CASE
             WHEN current.succeeded
                  AND previous.observed_state = 'down'
                  AND previous.consecutive_successes + 1 < current.recovery_threshold
               THEN previous.consecutive_successes + 1
             ELSE 0
           END::smallint AS consecutive_successes
    FROM evaluated AS previous
    JOIN ordered_results AS current
      ON current.monitor_id = previous.monitor_id
     AND current.result_number = previous.result_number + 1
)
INSERT INTO monitor_result_evaluations (
    monitor_id, job_id, scheduled_at, succeeded, observed_state,
    consecutive_failures, consecutive_successes
)
SELECT monitor_id, job_id, scheduled_at, succeeded, observed_state,
       consecutive_failures, consecutive_successes
FROM evaluated
ON CONFLICT (job_id) DO UPDATE
SET observed_state = EXCLUDED.observed_state,
    consecutive_failures = EXCLUDED.consecutive_failures,
    consecutive_successes = EXCLUDED.consecutive_successes,
    evaluated_at = CURRENT_TIMESTAMP;

WITH latest_observation AS (
    SELECT DISTINCT ON (evaluations.monitor_id)
           evaluations.monitor_id,
           evaluations.scheduled_at,
           evaluations.job_id,
           evaluations.observed_state,
           evaluations.consecutive_failures,
           evaluations.consecutive_successes
    FROM monitor_result_evaluations AS evaluations
    JOIN legacy_reliability_backfill_targets AS targets
      ON targets.monitor_id = evaluations.monitor_id
    ORDER BY evaluations.monitor_id,
             evaluations.scheduled_at DESC,
             evaluations.job_id DESC
), newest_expected AS (
    SELECT targets.monitor_id, expected.slot
    FROM legacy_reliability_backfill_targets AS targets
    LEFT JOIN LATERAL (
        SELECT slots.slot::timestamptz AS slot
        FROM monitor_schedule_periods AS periods
        CROSS JOIN LATERAL generate_series(
            periods.first_slot_at,
            LEAST(
                COALESCE(periods.ends_at - INTERVAL '1 microsecond', CURRENT_TIMESTAMP),
                CURRENT_TIMESTAMP
            ),
            make_interval(secs => periods.interval_seconds)
        ) AS slots(slot)
        WHERE periods.monitor_id = targets.monitor_id
          AND periods.starts_at <= CURRENT_TIMESTAMP
        ORDER BY slots.slot DESC
        LIMIT 1
    ) AS expected ON true
), displayed AS (
    SELECT newest_expected.monitor_id,
           newest_expected.slot,
           evaluation.job_id,
           evaluation.observed_state
    FROM newest_expected
    LEFT JOIN LATERAL (
        SELECT evaluations.job_id, evaluations.observed_state
        FROM monitor_result_evaluations AS evaluations
        WHERE evaluations.monitor_id = newest_expected.monitor_id
          AND evaluations.scheduled_at = newest_expected.slot
        ORDER BY evaluations.job_id DESC
        LIMIT 1
    ) AS evaluation ON true
)
UPDATE monitor_reliability_states AS states
SET display_state = COALESCE(displayed.observed_state, 'unknown'),
    observed_state = latest_observation.observed_state,
    consecutive_failures = latest_observation.consecutive_failures,
    consecutive_successes = latest_observation.consecutive_successes,
    last_observed_scheduled_at = latest_observation.scheduled_at,
    last_observed_job_id = latest_observation.job_id,
    newest_expected_scheduled_at = displayed.slot,
    last_evaluated_scheduled_at = displayed.slot,
    last_evaluated_job_id = displayed.job_id,
    updated_at = CURRENT_TIMESTAMP
FROM latest_observation
JOIN displayed ON displayed.monitor_id = latest_observation.monitor_id
WHERE states.monitor_id = latest_observation.monitor_id;

WITH newest_expected AS (
    SELECT states.monitor_id,
           states.last_evaluated_scheduled_at AS scheduled_at,
           states.last_evaluated_job_id AS job_id
    FROM monitor_reliability_states AS states
    JOIN legacy_reliability_backfill_targets AS targets
      ON targets.monitor_id = states.monitor_id
    WHERE states.last_evaluated_scheduled_at IS NOT NULL
)
INSERT INTO monitor_evaluation_positions (
    monitor_id, last_scheduled_at, last_job_id, invalidated_from
)
SELECT monitor_id, scheduled_at, job_id, NULL
FROM newest_expected
ON CONFLICT (monitor_id) DO UPDATE
SET last_scheduled_at = EXCLUDED.last_scheduled_at,
    last_job_id = EXCLUDED.last_job_id,
    invalidated_from = NULL,
    updated_at = CURRENT_TIMESTAMP;

-- These columns belonged to the removed PostgreSQL-leasing checker. Current
-- dispatch leases live in check_dispatch_outbox and SQS receipt handles.
ALTER TABLE check_jobs
    DROP CONSTRAINT check_jobs_attempt_limit_check,
    DROP COLUMN attempt_count,
    DROP COLUMN max_attempts,
    DROP COLUMN lease_owner,
    DROP COLUMN lease_token,
    DROP COLUMN lease_expires_at;
