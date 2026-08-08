-- name: LockEnvironmentForMonitorCreation :one
SELECT environments.organization_id::text AS organization_id
FROM environments
JOIN organizations ON organizations.id = environments.organization_id
JOIN org_members
  ON org_members.organization_id = environments.organization_id
 AND org_members.user_id = sqlc.arg(user_id)::text::uuid
WHERE environments.id = sqlc.arg(environment_id)::text::uuid
  AND organizations.deleted_at IS NULL
  AND org_members.role IN ('owner', 'admin', 'member')
FOR UPDATE OF organizations;

-- name: CountOrganizationMonitors :one
SELECT count(*) AS monitor_count
FROM monitors
WHERE organization_id = sqlc.arg(organization_id)::text::uuid;

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
    created_at,
    updated_at;

-- name: GetAccessibleEnvironmentOrganization :one
SELECT environments.organization_id::text AS organization_id
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
    created_at,
    updated_at
FROM monitors
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND environment_id = sqlc.arg(environment_id)::text::uuid
ORDER BY created_at, id;
