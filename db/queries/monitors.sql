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
ORDER BY created_at, id
LIMIT 100;

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

-- name: StoreSecureMonitorConfiguration :exec
UPDATE monitors
SET method = sqlc.arg(method),
    headers_ciphertext = sqlc.arg(headers_ciphertext),
    header_key_version = sqlc.narg(header_key_version),
    worker_pool_id = sqlc.arg(worker_pool_id),
    next_check_at = CURRENT_TIMESTAMP
      + mod(hashtextextended(id::text, 0) & 2147483647, interval_seconds::bigint)
        * INTERVAL '1 second'
WHERE id = sqlc.arg(monitor_id)::text::uuid;

-- name: OpenMonitorSchedulePeriod :exec
INSERT INTO monitor_schedule_periods (
    organization_id, environment_id, monitor_id, monitor_version,
    interval_seconds, worker_pool_id, starts_at, first_slot_at
)
SELECT organization_id, environment_id, id, version, interval_seconds,
       worker_pool_id, CURRENT_TIMESTAMP, next_check_at
FROM monitors
WHERE id = sqlc.arg(monitor_id)::text::uuid;

-- name: GetDurableMonitorState :one
SELECT monitor_reliability_states.display_state,
       monitor_reliability_states.last_observed_scheduled_at
FROM monitor_reliability_states
JOIN monitors ON monitors.id = monitor_reliability_states.monitor_id
WHERE monitors.organization_id = sqlc.arg(organization_id)::text::uuid
  AND monitors.environment_id = sqlc.arg(environment_id)::text::uuid
  AND monitors.id = sqlc.arg(monitor_id)::text::uuid;

-- name: LockManagedMonitor :one
SELECT monitors.id::text AS id,
       monitors.organization_id::text AS organization_id,
       monitors.environment_id::text AS environment_id,
       monitors.version,
       monitors.worker_pool_id,
       monitors.target_url,
       monitors.method,
       monitors.timeout_seconds,
       monitors.expected_status_min,
       monitors.expected_status_max,
       monitors.headers_ciphertext,
       monitors.header_key_version,
       org_members.role
FROM monitors
JOIN org_members
  ON org_members.organization_id = monitors.organization_id
 AND org_members.user_id = sqlc.arg(user_id)::text::uuid
WHERE monitors.environment_id = sqlc.arg(environment_id)::text::uuid
  AND monitors.id = sqlc.arg(monitor_id)::text::uuid
  AND monitors.deleted_at IS NULL
FOR UPDATE OF monitors;

-- name: UpdateManagedMonitor :one
UPDATE monitors
SET name = sqlc.arg(name),
    target_url = sqlc.arg(target_url),
    method = sqlc.arg(method),
    interval_seconds = sqlc.arg(interval_seconds)::integer,
    timeout_seconds = sqlc.arg(timeout_seconds),
    expected_status_min = sqlc.arg(expected_status_min),
    expected_status_max = sqlc.arg(expected_status_max),
    headers_ciphertext = sqlc.arg(headers_ciphertext),
    header_key_version = sqlc.narg(header_key_version),
    worker_pool_id = sqlc.arg(worker_pool_id),
    version = version + 1,
    updated_at = CURRENT_TIMESTAMP,
    next_check_at = CASE
      WHEN paused_at IS NULL THEN CURRENT_TIMESTAMP
        + mod(hashtextextended(id::text, 0) & 2147483647,
              sqlc.arg(interval_seconds)::bigint) * INTERVAL '1 second'
      ELSE next_check_at
    END
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND environment_id = sqlc.arg(environment_id)::text::uuid
  AND id = sqlc.arg(monitor_id)::text::uuid
  AND deleted_at IS NULL
RETURNING id::text AS id, organization_id::text AS organization_id,
          environment_id::text AS environment_id, name, target_url, method,
          interval_seconds, timeout_seconds, expected_status_min,
          expected_status_max, version, paused_at, worker_pool_id,
          created_at, updated_at;

-- name: SetManagedMonitorPaused :one
UPDATE monitors
SET paused_at = CASE
      WHEN sqlc.arg(paused)::boolean THEN CURRENT_TIMESTAMP
      ELSE NULL
    END,
    next_check_at = CASE
      WHEN sqlc.arg(paused)::boolean THEN next_check_at
      ELSE CURRENT_TIMESTAMP
        + mod(hashtextextended(id::text, 0) & 2147483647,
              interval_seconds::bigint) * INTERVAL '1 second'
    END,
    version = version + 1,
    updated_at = CURRENT_TIMESTAMP
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND environment_id = sqlc.arg(environment_id)::text::uuid
  AND id = sqlc.arg(monitor_id)::text::uuid
  AND deleted_at IS NULL
RETURNING id::text AS id, organization_id::text AS organization_id,
          environment_id::text AS environment_id, name, target_url, method,
          interval_seconds, timeout_seconds, expected_status_min,
          expected_status_max, version, paused_at, worker_pool_id,
          headers_ciphertext, header_key_version, created_at, updated_at;

-- name: CloseMonitorSchedulePeriod :exec
UPDATE monitor_schedule_periods
SET ends_at = CURRENT_TIMESTAMP
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND ends_at IS NULL;

-- name: SoftDeleteMonitor :execrows
UPDATE monitors
SET deleted_at = CURRENT_TIMESTAMP, version = version + 1,
    updated_at = CURRENT_TIMESTAMP
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND environment_id = sqlc.arg(environment_id)::text::uuid
  AND id = sqlc.arg(monitor_id)::text::uuid
  AND deleted_at IS NULL;

-- name: AcquireManualTestQueueLock :exec
SELECT pg_advisory_xact_lock(742019205);

-- name: CountActiveManualTestJobs :one
SELECT count(*)
FROM check_jobs
WHERE job_type = 'manual_test'
  AND state IN ('pending', 'pending_publish', 'published', 'running');

-- name: HasScheduledQueuePressure :one
SELECT
    EXISTS (
        SELECT 1
        FROM monitors
        WHERE paused_at IS NULL
          AND deleted_at IS NULL
          AND next_check_at < CURRENT_TIMESTAMP - INTERVAL '30 seconds'
    )
    OR (
        SELECT count(*) >= 900
        FROM check_jobs
        WHERE job_type = 'scheduled'
          AND state IN ('pending', 'pending_publish', 'published', 'running')
    ) AS has_pressure;

-- name: GetManualDispatchWorkerPool :one
SELECT encryption_key_id, encryption_public_key, network_policy_version, job_queue_url
FROM worker_pools
WHERE id = sqlc.arg(worker_pool_id)
  AND enabled
  AND lifecycle_state = 'active'
  AND schema_min <= sqlc.arg(schema_version)
  AND schema_max >= sqlc.arg(schema_version)
FOR SHARE;

-- name: NewManualJobID :one
SELECT gen_random_uuid()::text AS id;

-- name: CreateManualCheckJob :one
INSERT INTO check_jobs (
    id, organization_id, environment_id, monitor_id, job_type, state,
    scheduled_at, monitor_version, worker_pool_id, snapshot_hash, expires_at
)
VALUES (
    sqlc.arg(job_id)::text::uuid,
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    sqlc.arg(monitor_id)::text::uuid,
    'manual_test',
    'pending_publish',
    sqlc.arg(scheduled_at),
    sqlc.arg(monitor_version),
    sqlc.arg(worker_pool_id),
    sqlc.arg(snapshot_hash),
    sqlc.arg(expires_at)
)
RETURNING id::text AS id;

-- name: CreateManualDispatchOutbox :exec
INSERT INTO check_dispatch_outbox (
    job_id, worker_pool_id, queue_url, message_body, schema_version,
    platform_key_id, worker_encryption_key_id, snapshot_hash,
    message_deduplication_id, message_group_id, expires_at
)
VALUES (
    sqlc.arg(job_id)::text::uuid,
    sqlc.arg(worker_pool_id),
    sqlc.arg(queue_url),
    sqlc.arg(message_body),
    sqlc.arg(schema_version),
    sqlc.arg(platform_key_id),
    sqlc.arg(worker_encryption_key_id),
    sqlc.arg(snapshot_hash),
    sqlc.arg(job_id),
    sqlc.arg(job_id),
    sqlc.arg(expires_at)
);

-- name: InsertMonitorRefreshEvent :exec
INSERT INTO api_refresh_events (
    organization_id, environment_id, event_type, resource_type, resource_id
)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    sqlc.arg(event_type),
    sqlc.arg(resource_type),
    sqlc.arg(resource_id)::text::uuid
);
