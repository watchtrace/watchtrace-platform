-- name: LockEnvironmentForMonitorCreation :one
SELECT environments.organization_id::text AS organization_id, org_members.role
FROM environments
JOIN organizations ON organizations.id = environments.organization_id
JOIN org_members
  ON org_members.organization_id = environments.organization_id
 AND org_members.user_id = sqlc.arg(user_id)::text::uuid
WHERE environments.id = sqlc.arg(environment_id)::text::uuid
  AND organizations.deleted_at IS NULL
FOR UPDATE OF organizations;

-- name: CountOrganizationMonitors :one
SELECT count(*) AS monitor_count
FROM monitors
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND deleted_at IS NULL;

-- name: CreateMonitor :one
INSERT INTO monitors (
    organization_id,
    environment_id,
    name,
    target_url,
    interval_seconds,
    timeout_seconds,
    expected_status_min,
    expected_status_max
)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    sqlc.arg(name),
    sqlc.arg(target_url),
    sqlc.arg(interval_seconds),
    sqlc.arg(timeout_seconds),
    sqlc.arg(expected_status_min),
    sqlc.arg(expected_status_max)
)
RETURNING
    id::text AS id,
    organization_id::text AS organization_id,
    environment_id::text AS environment_id,
    name,
    target_url,
    method,
    interval_seconds,
    timeout_seconds,
    expected_status_min,
    expected_status_max,
    version,
    paused_at,
    worker_pool_id,
    headers_ciphertext,
    header_key_version,
    created_at,
    updated_at;

-- name: GetAccessibleEnvironmentOrganization :one
SELECT environments.organization_id::text AS organization_id, org_members.role
FROM environments
JOIN organizations ON organizations.id = environments.organization_id
JOIN org_members
  ON org_members.organization_id = environments.organization_id
 AND org_members.user_id = sqlc.arg(user_id)::text::uuid
WHERE environments.id = sqlc.arg(environment_id)::text::uuid
  AND organizations.deleted_at IS NULL;

-- name: ListEnvironmentMonitors :many
SELECT
    id::text AS id,
    organization_id::text AS organization_id,
    environment_id::text AS environment_id,
    name,
    target_url,
    method,
    interval_seconds,
    timeout_seconds,
    expected_status_min,
    expected_status_max,
    version,
    paused_at,
    worker_pool_id,
    headers_ciphertext,
    header_key_version,
    created_at,
    updated_at
FROM monitors
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND environment_id = sqlc.arg(environment_id)::text::uuid
  AND deleted_at IS NULL
ORDER BY created_at, id;

-- name: GetEnvironmentMonitor :one
SELECT
    id::text AS id,
    organization_id::text AS organization_id,
    environment_id::text AS environment_id,
    name,
    target_url,
    method,
    interval_seconds,
    timeout_seconds,
    expected_status_min,
    expected_status_max,
    version,
    paused_at,
    worker_pool_id,
    headers_ciphertext,
    header_key_version,
    created_at,
    updated_at
FROM monitors
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND environment_id = sqlc.arg(environment_id)::text::uuid
  AND id = sqlc.arg(monitor_id)::text::uuid
  AND deleted_at IS NULL;

-- name: GetLatestScheduledMonitorResult :one
SELECT succeeded
FROM health_checks
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND environment_id = sqlc.arg(environment_id)::text::uuid
  AND monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND job_type = 'scheduled'
ORDER BY scheduled_at DESC, job_id
LIMIT 1;

-- name: ListRecentMonitorResults :many
SELECT
    job_id::text AS job_id,
    job_type,
    scheduled_at,
    started_at,
    completed_at,
    succeeded,
    status_code,
    error_category,
    total_duration_microseconds
FROM health_checks
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND environment_id = sqlc.arg(environment_id)::text::uuid
  AND monitor_id = sqlc.arg(monitor_id)::text::uuid
ORDER BY scheduled_at DESC, job_id
LIMIT 20;
