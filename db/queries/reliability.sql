-- name: GetMonitorReliabilityReport :one
WITH expected_slots AS (
    SELECT slot
    FROM monitor_schedule_periods AS periods
    CROSS JOIN LATERAL generate_series(
        periods.first_slot_at
          + GREATEST(
              0,
              ceil(EXTRACT(EPOCH FROM (sqlc.arg(from_at) - periods.first_slot_at))
                   / periods.interval_seconds)::bigint
            ) * make_interval(secs => periods.interval_seconds),
        LEAST(COALESCE(periods.ends_at, sqlc.arg(to_at)), sqlc.arg(to_at))
          - INTERVAL '1 microsecond',
        make_interval(secs => periods.interval_seconds)
    ) AS slots(slot)
    WHERE periods.monitor_id = sqlc.arg(monitor_id)::text::uuid
      AND periods.starts_at < sqlc.arg(to_at)
      AND COALESCE(periods.ends_at, sqlc.arg(to_at)) > sqlc.arg(from_at)
      AND slots.slot >= GREATEST(sqlc.arg(from_at), periods.starts_at)
), aggregate AS (
    SELECT count(*)::bigint AS expected,
           count(health_checks.job_id)::bigint AS observed,
           count(health_checks.job_id) FILTER (WHERE health_checks.succeeded)::bigint AS successful
    FROM expected_slots
    LEFT JOIN health_checks
      ON health_checks.monitor_id = sqlc.arg(monitor_id)::text::uuid
     AND health_checks.job_type = 'scheduled'
     AND health_checks.scheduled_at = expected_slots.slot
)
SELECT expected, observed, successful
FROM aggregate;

-- name: EnsureMonitorReliabilityState :exec
INSERT INTO monitor_reliability_states (monitor_id)
SELECT id
FROM monitors
WHERE id = sqlc.arg(monitor_id)::text::uuid
ON CONFLICT DO NOTHING;

-- name: LockMonitorReliabilityState :one
SELECT display_state,
       observed_state,
       consecutive_failures,
       consecutive_successes,
       last_observed_scheduled_at,
       COALESCE(last_observed_job_id::text, '')::text AS last_observed_job_id,
       last_evaluated_scheduled_at,
       COALESCE(last_evaluated_job_id::text, '')::text AS last_evaluated_job_id
FROM monitor_reliability_states
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
FOR UPDATE;

-- name: GetMonitorAlertThresholds :one
SELECT failure_threshold, recovery_threshold
FROM alert_rules
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND rule_key = 'consecutive_failures'
  AND enabled;

-- name: GetReliabilityBaselineBefore :one
SELECT observed_state,
       consecutive_failures,
       consecutive_successes,
       scheduled_at,
       job_id::text AS job_id
FROM monitor_result_evaluations
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND (scheduled_at, job_id) < (
      sqlc.arg(scheduled_at), sqlc.arg(job_id)::text::uuid
  )
ORDER BY scheduled_at DESC, job_id DESC
LIMIT 1;

-- name: DeleteReliabilityEvaluationsFrom :exec
DELETE FROM monitor_result_evaluations
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND (scheduled_at, job_id) >= (
      sqlc.arg(scheduled_at), sqlc.arg(job_id)::text::uuid
  );

-- name: ListAllScheduledHealthChecks :many
SELECT job_id::text AS job_id, scheduled_at, succeeded
FROM health_checks
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND job_type = 'scheduled'
ORDER BY scheduled_at, job_id;

-- name: ListScheduledHealthChecksFrom :many
SELECT job_id::text AS job_id, scheduled_at, succeeded
FROM health_checks
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND job_type = 'scheduled'
  AND (scheduled_at, job_id) >= (
      sqlc.arg(scheduled_at), sqlc.arg(job_id)::text::uuid
  )
ORDER BY scheduled_at, job_id;

-- name: ListScheduledHealthChecksAfter :many
SELECT job_id::text AS job_id, scheduled_at, succeeded
FROM health_checks
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND job_type = 'scheduled'
  AND (scheduled_at, job_id) > (
      sqlc.arg(scheduled_at), sqlc.arg(job_id)::text::uuid
  )
ORDER BY scheduled_at, job_id;

-- name: UpsertMonitorResultEvaluation :exec
INSERT INTO monitor_result_evaluations (
    monitor_id, job_id, scheduled_at, succeeded, observed_state,
    consecutive_failures, consecutive_successes
)
VALUES (
    sqlc.arg(monitor_id)::text::uuid,
    sqlc.arg(job_id)::text::uuid,
    sqlc.arg(scheduled_at),
    sqlc.arg(succeeded),
    sqlc.arg(observed_state),
    sqlc.arg(consecutive_failures),
    sqlc.arg(consecutive_successes)
)
ON CONFLICT (job_id)
DO UPDATE SET observed_state = EXCLUDED.observed_state,
              consecutive_failures = EXCLUDED.consecutive_failures,
              consecutive_successes = EXCLUDED.consecutive_successes,
              evaluated_at = CURRENT_TIMESTAMP;

-- name: GetNewestExpectedMonitorSlot :one
SELECT slot::timestamptz AS slot
FROM monitor_schedule_periods AS periods
CROSS JOIN LATERAL generate_series(
    periods.first_slot_at,
    LEAST(
        COALESCE(periods.ends_at - INTERVAL '1 microsecond', sqlc.arg(evaluated_at)::timestamptz),
        sqlc.arg(evaluated_at)::timestamptz
    ),
    make_interval(secs => periods.interval_seconds)
) AS slots(slot)
WHERE periods.monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND periods.starts_at <= sqlc.arg(evaluated_at)::timestamptz
ORDER BY slot DESC
LIMIT 1;

-- name: GetMonitorEvaluationAtSlot :one
SELECT observed_state, job_id::text AS job_id
FROM monitor_result_evaluations
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND scheduled_at = sqlc.arg(scheduled_at)
ORDER BY job_id DESC
LIMIT 1;

-- name: UpdateMonitorReliabilityState :exec
UPDATE monitor_reliability_states
SET display_state = sqlc.arg(display_state),
    observed_state = sqlc.arg(observed_state),
    consecutive_failures = sqlc.arg(consecutive_failures),
    consecutive_successes = sqlc.arg(consecutive_successes),
    last_observed_scheduled_at = sqlc.narg(last_observed_scheduled_at),
    last_observed_job_id = NULLIF(sqlc.arg(last_observed_job_id)::text, '')::uuid,
    newest_expected_scheduled_at = sqlc.narg(newest_expected_scheduled_at),
    last_evaluated_scheduled_at = sqlc.narg(newest_expected_scheduled_at),
    last_evaluated_job_id = NULLIF(sqlc.arg(last_evaluated_job_id)::text, '')::uuid,
    updated_at = CURRENT_TIMESTAMP
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid;

-- name: UpsertMonitorEvaluationPosition :exec
INSERT INTO monitor_evaluation_positions (
    monitor_id, last_scheduled_at, last_job_id, invalidated_from
)
VALUES (
    sqlc.arg(monitor_id)::text::uuid,
    sqlc.arg(last_scheduled_at),
    NULLIF(sqlc.arg(last_job_id)::text, '')::uuid,
    NULL
)
ON CONFLICT (monitor_id)
DO UPDATE SET last_scheduled_at = EXCLUDED.last_scheduled_at,
              last_job_id = EXCLUDED.last_job_id,
              invalidated_from = NULL,
              updated_at = CURRENT_TIMESTAMP;

-- name: InsertMonitorStateCorrection :exec
INSERT INTO monitor_state_correction_events (
    monitor_id, accepted_job_id, corrected_from,
    previous_display_state, corrected_display_state
)
VALUES (
    sqlc.arg(monitor_id)::text::uuid,
    sqlc.arg(accepted_job_id)::text::uuid,
    sqlc.arg(corrected_from),
    sqlc.arg(previous_display_state),
    sqlc.arg(corrected_display_state)
)
ON CONFLICT (monitor_id, accepted_job_id) DO NOTHING;

-- name: ListMonitorsDueForStateRefresh :many
SELECT DISTINCT monitors.id::text AS monitor_id
FROM monitors
JOIN monitor_schedule_periods
  ON monitor_schedule_periods.monitor_id = monitors.id
WHERE monitors.deleted_at IS NULL
  AND monitor_schedule_periods.starts_at <= sqlc.arg(evaluated_at)
ORDER BY monitor_id
LIMIT sqlc.arg(result_limit);

-- name: UpsertHourlyMonitorRollups :execrows
WITH expected_slots AS (
    SELECT periods.organization_id,
           periods.environment_id,
           periods.monitor_id,
           slot
    FROM monitor_schedule_periods AS periods
    CROSS JOIN LATERAL generate_series(
        periods.first_slot_at
          + GREATEST(
              0,
              ceil(EXTRACT(EPOCH FROM (sqlc.arg(bucket_start) - periods.first_slot_at))
                   / periods.interval_seconds)::bigint
            ) * make_interval(secs => periods.interval_seconds),
        LEAST(
            COALESCE(
                periods.ends_at,
                LEAST(sqlc.arg(bucket_start) + INTERVAL '1 hour', CURRENT_TIMESTAMP)
            ),
            LEAST(sqlc.arg(bucket_start) + INTERVAL '1 hour', CURRENT_TIMESTAMP)
        ) - INTERVAL '1 microsecond',
        make_interval(secs => periods.interval_seconds)
    ) AS slots(slot)
    WHERE periods.starts_at < LEAST(
              sqlc.arg(bucket_start) + INTERVAL '1 hour', CURRENT_TIMESTAMP
          )
      AND COALESCE(
              periods.ends_at,
              LEAST(sqlc.arg(bucket_start) + INTERVAL '1 hour', CURRENT_TIMESTAMP)
          ) > sqlc.arg(bucket_start)
      AND slots.slot >= GREATEST(sqlc.arg(bucket_start), periods.starts_at)
), aggregate AS (
    SELECT expected_slots.organization_id,
           expected_slots.environment_id,
           expected_slots.monitor_id,
           count(*)::integer AS expected_checks,
           count(health_checks.job_id)::integer AS observed_checks,
           count(health_checks.job_id) FILTER (WHERE health_checks.succeeded)::integer AS successful_checks,
           COALESCE(sum(health_checks.total_duration_microseconds), 0)::bigint AS total_duration_microseconds
    FROM expected_slots
    LEFT JOIN health_checks
      ON health_checks.monitor_id = expected_slots.monitor_id
     AND health_checks.job_type = 'scheduled'
     AND health_checks.scheduled_at = expected_slots.slot
    GROUP BY expected_slots.organization_id,
             expected_slots.environment_id,
             expected_slots.monitor_id
)
INSERT INTO monitor_rollups_hourly (
    organization_id, environment_id, monitor_id, bucket_start,
    expected_checks, observed_checks, successful_checks, unknown_checks,
    total_duration_microseconds, updated_at
)
SELECT organization_id,
       environment_id,
       monitor_id,
       sqlc.arg(bucket_start),
       expected_checks,
       observed_checks,
       successful_checks,
       GREATEST(expected_checks - observed_checks, 0),
       total_duration_microseconds,
       CURRENT_TIMESTAMP
FROM aggregate
ON CONFLICT (monitor_id, bucket_start)
DO UPDATE SET expected_checks = EXCLUDED.expected_checks,
              observed_checks = EXCLUDED.observed_checks,
              successful_checks = EXCLUDED.successful_checks,
              unknown_checks = EXCLUDED.unknown_checks,
              total_duration_microseconds = EXCLUDED.total_duration_microseconds,
              updated_at = CURRENT_TIMESTAMP;

-- name: DeleteHourlyRollupInvalidations :exec
DELETE FROM monitor_rollup_invalidations
WHERE bucket_kind = 'hourly'
  AND bucket_start = sqlc.arg(bucket_start);

-- name: HasHourlyRollupInvalidationsForDay :one
SELECT EXISTS (
    SELECT 1
    FROM monitor_rollup_invalidations
    WHERE bucket_kind = 'hourly'
      AND bucket_start >= sqlc.arg(bucket_date)::text::date
      AND bucket_start < sqlc.arg(bucket_date)::text::date + INTERVAL '1 day'
) AS has_invalidations;

-- name: UpsertDailyMonitorRollups :execrows
INSERT INTO monitor_rollups_daily (
    organization_id, environment_id, monitor_id, bucket_start,
    expected_checks, observed_checks, successful_checks, unknown_checks,
    total_duration_microseconds, updated_at
)
SELECT organization_id,
       environment_id,
       monitor_id,
       sqlc.arg(bucket_date)::text::date,
       sum(expected_checks),
       sum(observed_checks),
       sum(successful_checks),
       sum(unknown_checks),
       sum(total_duration_microseconds),
       CURRENT_TIMESTAMP
FROM monitor_rollups_hourly
WHERE bucket_start >= sqlc.arg(bucket_date)::text::date
  AND bucket_start < sqlc.arg(bucket_date)::text::date + INTERVAL '1 day'
GROUP BY organization_id, environment_id, monitor_id
ON CONFLICT (monitor_id, bucket_start)
DO UPDATE SET expected_checks = EXCLUDED.expected_checks,
              observed_checks = EXCLUDED.observed_checks,
              successful_checks = EXCLUDED.successful_checks,
              unknown_checks = EXCLUDED.unknown_checks,
              total_duration_microseconds = EXCLUDED.total_duration_microseconds,
              updated_at = CURRENT_TIMESTAMP;

-- name: DeleteDailyRollupInvalidations :exec
DELETE FROM monitor_rollup_invalidations
WHERE bucket_kind = 'daily'
  AND bucket_start = sqlc.arg(bucket_date)::text::date;

-- name: GetNextRepairableRollupInvalidation :one
SELECT bucket_kind, bucket_start
FROM monitor_rollup_invalidations AS invalidation
WHERE bucket_kind = 'hourly'
   OR NOT EXISTS (
       SELECT 1
       FROM monitor_rollup_invalidations AS hourly
       WHERE hourly.bucket_kind = 'hourly'
         AND hourly.bucket_start >= invalidation.bucket_start
         AND hourly.bucket_start < invalidation.bucket_start + INTERVAL '1 day'
   )
ORDER BY CASE bucket_kind WHEN 'hourly' THEN 0 ELSE 1 END, bucket_start
LIMIT 1;

-- name: LockHourlyRollupCheckpoint :one
SELECT hourly_through
FROM monitoring_rollup_checkpoint
WHERE singleton
FOR UPDATE;

-- name: AdvanceHourlyRollupCheckpoint :exec
UPDATE monitoring_rollup_checkpoint
SET hourly_through = GREATEST(hourly_through, sqlc.arg(hourly_through)),
    updated_at = CURRENT_TIMESTAMP
WHERE singleton;

-- name: LockDailyRollupCheckpoint :one
SELECT daily_through
FROM monitoring_rollup_checkpoint
WHERE singleton
FOR UPDATE;

-- name: AdvanceDailyRollupCheckpoint :exec
UPDATE monitoring_rollup_checkpoint
SET daily_through = GREATEST(daily_through, sqlc.arg(daily_through)::text::date),
    updated_at = CURRENT_TIMESTAMP
WHERE singleton;

-- name: DeleteRetainedHealthChecks :execrows
DELETE FROM health_checks
WHERE scheduled_at < sqlc.arg(delete_before)
  AND EXISTS (
      SELECT 1
      FROM monitor_rollups_hourly
      WHERE monitor_rollups_hourly.monitor_id = health_checks.monitor_id
        AND monitor_rollups_hourly.bucket_start = date_trunc('hour', health_checks.scheduled_at)
  )
  AND NOT EXISTS (
      SELECT 1
      FROM monitor_rollup_invalidations
      WHERE monitor_rollup_invalidations.monitor_id = health_checks.monitor_id
        AND monitor_rollup_invalidations.bucket_kind = 'hourly'
        AND monitor_rollup_invalidations.bucket_start = date_trunc('hour', health_checks.scheduled_at)
  );

-- name: DeleteRetainedCoverageGaps :execrows
DELETE FROM monitoring_coverage_gaps
WHERE scheduled_at < sqlc.arg(delete_before)
  AND EXISTS (
      SELECT 1
      FROM monitor_rollups_hourly
      WHERE monitor_rollups_hourly.monitor_id = monitoring_coverage_gaps.monitor_id
        AND monitor_rollups_hourly.bucket_start = date_trunc('hour', monitoring_coverage_gaps.scheduled_at)
  )
  AND NOT EXISTS (
      SELECT 1
      FROM monitor_rollup_invalidations
      WHERE monitor_rollup_invalidations.monitor_id = monitoring_coverage_gaps.monitor_id
        AND monitor_rollup_invalidations.bucket_kind = 'hourly'
        AND monitor_rollup_invalidations.bucket_start = date_trunc('hour', monitoring_coverage_gaps.scheduled_at)
  );

-- name: DeleteRetainedHourlyRollups :execrows
DELETE FROM monitor_rollups_hourly
WHERE monitor_rollups_hourly.bucket_start < sqlc.arg(delete_before)
  AND EXISTS (
      SELECT 1
      FROM monitor_rollups_daily
      WHERE monitor_rollups_daily.monitor_id = monitor_rollups_hourly.monitor_id
        AND monitor_rollups_daily.bucket_start = monitor_rollups_hourly.bucket_start::date
  )
  AND NOT EXISTS (
      SELECT 1
      FROM monitor_rollup_invalidations
      WHERE monitor_rollup_invalidations.monitor_id = monitor_rollups_hourly.monitor_id
        AND monitor_rollup_invalidations.bucket_kind = 'daily'
        AND monitor_rollup_invalidations.bucket_start = monitor_rollups_hourly.bucket_start::date
  );

-- name: DeleteRetainedDailyRollups :execrows
DELETE FROM monitor_rollups_daily
WHERE bucket_start < sqlc.arg(delete_before)::text::date;

-- name: DeleteRetainedCheckJobs :execrows
DELETE FROM check_jobs
WHERE (
        (check_jobs.state = 'completed' AND check_jobs.completed_at < sqlc.arg(completed_before))
        OR (
            check_jobs.state IN ('dead', 'expired', 'cancelled', 'quarantined')
            AND check_jobs.completed_at < sqlc.arg(nonterminal_before)
        )
      )
  AND NOT EXISTS (
      SELECT 1 FROM health_checks WHERE health_checks.job_id = check_jobs.id
  );

-- name: RecordRetentionCheckpoint :exec
UPDATE monitoring_rollup_checkpoint
SET last_retention_at = sqlc.arg(retained_at), updated_at = CURRENT_TIMESTAMP
WHERE singleton;
