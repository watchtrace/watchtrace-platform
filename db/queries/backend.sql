-- name: AuthorizeBackendMonitor :one
SELECT monitors.organization_id::text AS organization_id, org_members.role
FROM monitors
JOIN organizations
  ON organizations.id = monitors.organization_id
 AND organizations.deleted_at IS NULL
JOIN org_members
  ON org_members.organization_id = monitors.organization_id
 AND org_members.user_id = sqlc.arg(user_id)::text::uuid
WHERE monitors.environment_id = sqlc.arg(environment_id)::text::uuid
  AND monitors.id = sqlc.arg(monitor_id)::text::uuid
  AND monitors.deleted_at IS NULL;

-- name: ListBackendChecks :many
SELECT job_id::text AS job_id, job_type, scheduled_at, started_at,
       completed_at, succeeded, status_code, error_category,
       total_duration_microseconds
FROM health_checks
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND scheduled_at >= sqlc.arg(from_at)
  AND scheduled_at < sqlc.arg(to_at)
  AND (sqlc.arg(job_type)::text = '' OR job_type = sqlc.arg(job_type))
  AND (
      NOT sqlc.arg(has_cursor)::boolean
      OR (scheduled_at, job_id) < (
          sqlc.arg(cursor_at), sqlc.arg(cursor_id)::text::uuid
      )
  )
ORDER BY scheduled_at DESC, job_id DESC
LIMIT sqlc.arg(result_limit);

-- name: GetMonitorLatencyTotals :one
SELECT count(*) AS sample_count,
       COALESCE(sum(total_duration_microseconds), 0)::bigint AS total_duration_us
FROM health_checks
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND job_type = 'scheduled'
  AND scheduled_at >= sqlc.arg(from_at)
  AND scheduled_at < sqlc.arg(to_at);

-- name: GetFirstMonitorRollupInvalidation :one
SELECT invalidated_at
FROM monitor_rollup_invalidations
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND bucket_start < sqlc.arg(to_at)
  AND bucket_start + CASE bucket_kind
        WHEN 'hourly' THEN INTERVAL '1 hour'
        ELSE INTERVAL '1 day'
      END > sqlc.arg(from_at)
ORDER BY invalidated_at
LIMIT 1;

-- name: GetEnvironmentMonitorStateCounts :one
SELECT
    count(*) FILTER (WHERE COALESCE(states.display_state, 'unknown') = 'healthy') AS healthy,
    count(*) FILTER (WHERE COALESCE(states.display_state, 'unknown') = 'degraded') AS degraded,
    count(*) FILTER (WHERE COALESCE(states.display_state, 'unknown') = 'down') AS down,
    count(*) FILTER (WHERE COALESCE(states.display_state, 'unknown') = 'unknown') AS unknown
FROM monitors
LEFT JOIN monitor_reliability_states AS states ON states.monitor_id = monitors.id
WHERE monitors.environment_id = sqlc.arg(environment_id)::text::uuid
  AND monitors.deleted_at IS NULL;

-- name: CountOpenEnvironmentIncidents :one
SELECT count(*)
FROM incidents
WHERE environment_id = sqlc.arg(environment_id)::text::uuid
  AND status = 'open';

-- name: GetEnvironmentReliability :one
WITH expected AS (
    SELECT periods.monitor_id, slot
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
    ) AS slot
    WHERE periods.environment_id = sqlc.arg(environment_id)::text::uuid
      AND periods.starts_at < sqlc.arg(to_at)
      AND COALESCE(periods.ends_at, sqlc.arg(to_at)) > sqlc.arg(from_at)
      AND slot >= GREATEST(sqlc.arg(from_at), periods.starts_at)
), aggregate AS (
    SELECT count(*)::bigint AS expected,
           count(health_checks.job_id)::bigint AS observed,
           count(health_checks.job_id) FILTER (WHERE health_checks.succeeded)::bigint AS successful,
           count(health_checks.job_id)::bigint AS latency_samples,
           COALESCE(sum(health_checks.total_duration_microseconds), 0)::bigint AS latency_total_us
    FROM expected
    LEFT JOIN health_checks
      ON health_checks.monitor_id = expected.monitor_id
     AND health_checks.job_type = 'scheduled'
     AND health_checks.scheduled_at = expected.slot
)
SELECT expected, observed, successful, latency_samples, latency_total_us
FROM aggregate;

-- name: GetFirstEnvironmentRollupInvalidation :one
SELECT invalidations.invalidated_at
FROM monitor_rollup_invalidations AS invalidations
JOIN monitors ON monitors.id = invalidations.monitor_id
WHERE monitors.environment_id = sqlc.arg(environment_id)::text::uuid
  AND invalidations.bucket_start < sqlc.arg(to_at)
  AND invalidations.bucket_start + CASE invalidations.bucket_kind
        WHEN 'hourly' THEN INTERVAL '1 hour'
        ELSE INTERVAL '1 day'
      END > sqlc.arg(from_at)
ORDER BY invalidations.invalidated_at
LIMIT 1;

-- name: ListBackendIncidents :many
SELECT id::text AS id, organization_id::text AS organization_id,
       environment_id::text AS environment_id, monitor_id::text AS monitor_id,
       status, started_at, opened_at, acknowledged_at,
       acknowledged_by_user_id, resolved_at, resolved_by_user_id,
       resolution_kind, resolution_reason
FROM incidents
WHERE environment_id = sqlc.arg(environment_id)::text::uuid
  AND opened_at >= sqlc.arg(from_at)
  AND opened_at < sqlc.arg(to_at)
  AND (sqlc.arg(status)::text = '' OR status = sqlc.arg(status))
  AND (
      NOT sqlc.arg(has_cursor)::boolean
      OR (opened_at, id) < (
          sqlc.arg(cursor_at), sqlc.arg(cursor_id)::text::uuid
      )
  )
ORDER BY opened_at DESC, id DESC
LIMIT sqlc.arg(result_limit);

-- name: ListBackendIncidentEvents :many
SELECT id::text AS id, event_type,
       actor_user_id, source_job_id,
       safe_reason, occurred_at
FROM incident_events
WHERE incident_id = sqlc.arg(incident_id)::text::uuid
ORDER BY occurred_at, id
LIMIT 200;

-- name: ListBackendNotificationDeliveries :many
SELECT delivery_id::text AS delivery_id, transition, state, attempt_count,
       next_attempt_at, last_provider_status, accepted_at, failed_at
FROM notification_outbox
WHERE incident_id = sqlc.arg(incident_id)::text::uuid
ORDER BY created_at, delivery_id
LIMIT 200;
