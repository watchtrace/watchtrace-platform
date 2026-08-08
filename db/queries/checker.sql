-- name: ClaimPendingCheckJob :one
WITH candidate AS (
    SELECT check_jobs.id
    FROM check_jobs
    WHERE check_jobs.state = 'pending'
      AND check_jobs.attempt_count < check_jobs.max_attempts
    ORDER BY
        CASE check_jobs.job_type WHEN 'scheduled' THEN 0 ELSE 1 END,
        check_jobs.scheduled_at,
        check_jobs.id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
UPDATE check_jobs
SET state = 'running',
    attempt_count = check_jobs.attempt_count + 1,
    lease_owner = sqlc.arg(lease_owner),
    lease_token = gen_random_uuid(),
    lease_expires_at = CURRENT_TIMESTAMP + sqlc.arg(lease_seconds)::integer * INTERVAL '1 second',
    started_at = COALESCE(check_jobs.started_at, CURRENT_TIMESTAMP),
    completed_at = NULL
FROM candidate, monitors
WHERE check_jobs.id = candidate.id
  AND monitors.organization_id = check_jobs.organization_id
  AND monitors.environment_id = check_jobs.environment_id
  AND monitors.id = check_jobs.monitor_id
RETURNING
    check_jobs.id::text AS job_id,
    check_jobs.organization_id::text AS organization_id,
    check_jobs.environment_id::text AS environment_id,
    check_jobs.monitor_id::text AS monitor_id,
    check_jobs.job_type,
    check_jobs.scheduled_at,
    check_jobs.started_at,
    check_jobs.lease_token::text AS lease_token,
    check_jobs.lease_expires_at,
    check_jobs.attempt_count,
    monitors.target_url,
    monitors.method,
    monitors.timeout_seconds,
    monitors.expected_status_min,
    monitors.expected_status_max;

-- name: LockCheckJobForCompletion :one
SELECT
    id::text AS job_id,
    organization_id::text AS organization_id,
    environment_id::text AS environment_id,
    monitor_id::text AS monitor_id,
    job_type,
    state,
    scheduled_at,
    COALESCE(
        lease_token,
        '00000000-0000-0000-0000-000000000000'::uuid
    )::text AS lease_token
FROM check_jobs
WHERE id = sqlc.arg(job_id)::text::uuid
FOR UPDATE;

-- name: HealthCheckExists :one
SELECT EXISTS (
    SELECT 1
    FROM health_checks
    WHERE job_id = sqlc.arg(job_id)::text::uuid
) AS result_exists;

-- name: InsertHealthCheck :execrows
INSERT INTO health_checks (
    job_id,
    organization_id,
    environment_id,
    monitor_id,
    job_type,
    scheduled_at,
    started_at,
    completed_at,
    succeeded,
    status_code,
    error_category,
    total_duration_microseconds
)
VALUES (
    sqlc.arg(job_id)::text::uuid,
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    sqlc.arg(monitor_id)::text::uuid,
    sqlc.arg(job_type),
    sqlc.arg(scheduled_at),
    sqlc.arg(started_at),
    sqlc.arg(completed_at),
    sqlc.arg(succeeded),
    sqlc.narg(status_code),
    sqlc.narg(error_category),
    sqlc.arg(total_duration_microseconds)
)
ON CONFLICT (job_id) DO NOTHING;

-- name: CompleteCheckJob :execrows
UPDATE check_jobs
SET state = 'completed',
    completed_at = sqlc.arg(completed_at),
    lease_owner = NULL,
    lease_token = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg(job_id)::text::uuid
  AND state = 'running'
  AND lease_token = sqlc.arg(lease_token)::text::uuid;
