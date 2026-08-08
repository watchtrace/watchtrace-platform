-- name: LockDueMonitors :many
SELECT
    monitors.id::text AS monitor_id,
    monitors.organization_id::text AS organization_id,
    monitors.environment_id::text AS environment_id,
    monitors.interval_seconds,
    monitors.next_check_at,
    CURRENT_TIMESTAMP::timestamptz AS scheduler_time
FROM monitors
WHERE monitors.next_check_at <= CURRENT_TIMESTAMP
  AND NOT EXISTS (
      SELECT 1
      FROM check_jobs
      WHERE check_jobs.monitor_id = monitors.id
        AND check_jobs.job_type = 'scheduled'
        AND check_jobs.state IN ('pending', 'running')
  )
ORDER BY monitors.next_check_at, monitors.id
LIMIT sqlc.arg(batch_size)
FOR UPDATE OF monitors SKIP LOCKED;

-- name: CreateScheduledCheckJob :execrows
INSERT INTO check_jobs (
    organization_id,
    environment_id,
    monitor_id,
    job_type,
    state,
    scheduled_at
)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    sqlc.arg(monitor_id)::text::uuid,
    'scheduled',
    'pending',
    sqlc.arg(scheduled_at)
)
ON CONFLICT (monitor_id, scheduled_at) DO NOTHING;

-- name: AdvanceMonitorSchedule :execrows
UPDATE monitors
SET next_check_at = sqlc.arg(next_check_at)
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND environment_id = sqlc.arg(environment_id)::text::uuid
  AND id = sqlc.arg(monitor_id)::text::uuid
  AND next_check_at = sqlc.arg(scheduled_at);
