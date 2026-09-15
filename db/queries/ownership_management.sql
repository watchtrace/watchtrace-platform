-- name: AuthorizeProjectMembership :one
SELECT projects.organization_id::text AS organization_id, org_members.role
FROM projects
JOIN organizations
  ON organizations.id = projects.organization_id
 AND organizations.deleted_at IS NULL
JOIN org_members
  ON org_members.organization_id = projects.organization_id
 AND org_members.user_id = sqlc.arg(user_id)::text::uuid
WHERE projects.id = sqlc.arg(project_id)::text::uuid;

-- name: AuthorizeEnvironmentMembership :one
SELECT environments.organization_id::text AS organization_id, org_members.role
FROM environments
JOIN organizations
  ON organizations.id = environments.organization_id
 AND organizations.deleted_at IS NULL
JOIN org_members
  ON org_members.organization_id = environments.organization_id
 AND org_members.user_id = sqlc.arg(user_id)::text::uuid
WHERE environments.id = sqlc.arg(environment_id)::text::uuid;

-- name: InsertTenantRefreshEvent :exec
INSERT INTO api_refresh_events (
    organization_id, environment_id, event_type, resource_type, resource_id
)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    NULLIF(sqlc.arg(environment_id)::text, '')::uuid,
    sqlc.arg(event_type),
    sqlc.arg(resource_type),
    sqlc.arg(resource_id)::text::uuid
);

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (
    organization_id, actor_user_id, action, resource_type, resource_id
)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(actor_user_id)::text::uuid,
    sqlc.arg(action),
    sqlc.arg(resource_type),
    sqlc.arg(resource_id)::text::uuid
);

-- name: ListAccessibleOrganizations :many
SELECT organizations.id::text AS id, organizations.name, organizations.slug,
       org_members.role, organizations.created_at
FROM organizations
JOIN org_members ON org_members.organization_id = organizations.id
WHERE org_members.user_id = sqlc.arg(user_id)::text::uuid
  AND organizations.deleted_at IS NULL
ORDER BY organizations.created_at, organizations.id
LIMIT 100;

-- name: GetAccessibleOrganization :one
SELECT organizations.id::text AS id, organizations.name, organizations.slug,
       org_members.role, organizations.created_at
FROM organizations
JOIN org_members
  ON org_members.organization_id = organizations.id
 AND org_members.user_id = sqlc.arg(user_id)::text::uuid
WHERE organizations.id = sqlc.arg(organization_id)::text::uuid
  AND organizations.deleted_at IS NULL;

-- name: UpdateOrganizationName :exec
UPDATE organizations
SET name = sqlc.arg(name), updated_at = CURRENT_TIMESTAMP
WHERE id = sqlc.arg(organization_id)::text::uuid
  AND deleted_at IS NULL;

-- name: SoftDeleteOrganization :exec
UPDATE organizations
SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
WHERE id = sqlc.arg(organization_id)::text::uuid
  AND deleted_at IS NULL;

-- name: ListTenantProjects :many
SELECT id::text AS id, organization_id::text AS organization_id, name,
       description, created_at, updated_at
FROM projects
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
ORDER BY created_at, id
LIMIT 100;

-- name: GetTenantProject :one
SELECT id::text AS id, organization_id::text AS organization_id, name,
       description, created_at, updated_at
FROM projects
WHERE id = sqlc.arg(project_id)::text::uuid;

-- name: CreateTenantProject :one
INSERT INTO projects (organization_id, name, description)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(name),
    sqlc.arg(description)
)
RETURNING id::text AS id, organization_id::text AS organization_id, name,
          description, created_at, updated_at;

-- name: UpdateTenantProject :one
UPDATE projects
SET name = sqlc.arg(name), description = sqlc.arg(description),
    updated_at = CURRENT_TIMESTAMP
WHERE id = sqlc.arg(project_id)::text::uuid
RETURNING id::text AS id, organization_id::text AS organization_id, name,
          description, created_at, updated_at;

-- name: DeleteEmptyTenantProject :execrows
DELETE FROM projects
WHERE id = sqlc.arg(project_id)::text::uuid
  AND NOT EXISTS (
      SELECT 1 FROM environments WHERE environments.project_id = projects.id
  );

-- name: ListTenantEnvironments :many
SELECT id::text AS id, organization_id::text AS organization_id,
       project_id::text AS project_id, name, environment_type, created_at, updated_at
FROM environments
WHERE project_id = sqlc.arg(project_id)::text::uuid
ORDER BY created_at, id
LIMIT 100;

-- name: GetTenantEnvironment :one
SELECT id::text AS id, organization_id::text AS organization_id,
       project_id::text AS project_id, name, environment_type, created_at, updated_at
FROM environments
WHERE id = sqlc.arg(environment_id)::text::uuid;

-- name: CreateTenantEnvironment :one
INSERT INTO environments (organization_id, project_id, name, environment_type)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(project_id)::text::uuid,
    sqlc.arg(name),
    sqlc.arg(environment_type)
)
RETURNING id::text AS id, organization_id::text AS organization_id,
          project_id::text AS project_id, name, environment_type, created_at, updated_at;

-- name: UpdateTenantEnvironment :one
UPDATE environments
SET name = sqlc.arg(name), environment_type = sqlc.arg(environment_type),
    updated_at = CURRENT_TIMESTAMP
WHERE id = sqlc.arg(environment_id)::text::uuid
RETURNING id::text AS id, organization_id::text AS organization_id,
          project_id::text AS project_id, name, environment_type, created_at, updated_at;

-- name: DeleteEmptyTenantEnvironment :execrows
DELETE FROM environments
WHERE id = sqlc.arg(environment_id)::text::uuid
  AND NOT EXISTS (
      SELECT 1 FROM monitors WHERE monitors.environment_id = environments.id
  );

-- name: LockOrganizationMemberRole :one
SELECT role
FROM org_members
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND user_id = sqlc.arg(user_id)::text::uuid
FOR UPDATE;

-- name: GetOrganizationMemberNotificationPreference :one
SELECT incident_notifications_enabled
FROM org_members
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND user_id = sqlc.arg(user_id)::text::uuid;

-- name: UpdateOrganizationMember :one
UPDATE org_members
SET role = sqlc.arg(role),
    incident_notifications_enabled = sqlc.arg(incident_notifications_enabled),
    updated_at = CURRENT_TIMESTAMP
FROM users
WHERE org_members.organization_id = sqlc.arg(organization_id)::text::uuid
  AND org_members.user_id = sqlc.arg(user_id)::text::uuid
  AND users.id = org_members.user_id
RETURNING org_members.user_id::text AS user_id, users.email, org_members.role,
          org_members.incident_notifications_enabled, org_members.created_at;

-- name: GetOrganizationMemberRole :one
SELECT role
FROM org_members
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND user_id = sqlc.arg(user_id)::text::uuid;

-- name: DeleteOrganizationMember :exec
DELETE FROM org_members
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND user_id = sqlc.arg(user_id)::text::uuid;
