-- name: CreateOrganization :one
INSERT INTO organizations (name, slug)
VALUES ($1, $2)
RETURNING id::text AS id, name, slug;

-- name: CreateOwnerMembership :exec
INSERT INTO org_members (organization_id, user_id, role)
VALUES (sqlc.arg(organization_id)::text::uuid, sqlc.arg(user_id)::text::uuid, 'owner');

-- name: CreateProject :one
INSERT INTO projects (organization_id, name, description)
VALUES (sqlc.arg(organization_id)::text::uuid, sqlc.arg(name), sqlc.arg(description))
RETURNING id::text AS id, organization_id::text AS organization_id, name, description;

-- name: CreateProductionEnvironment :one
INSERT INTO environments (organization_id, project_id, name, environment_type)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(project_id)::text::uuid,
    'Production',
    'production'
)
RETURNING
    id::text AS id,
    organization_id::text AS organization_id,
    project_id::text AS project_id,
    name,
    environment_type;

-- name: GetOrganizationMembershipRole :one
SELECT org_members.role
FROM org_members
JOIN organizations ON organizations.id = org_members.organization_id
WHERE org_members.organization_id = sqlc.arg(organization_id)::text::uuid
  AND org_members.user_id = sqlc.arg(user_id)::text::uuid
  AND organizations.deleted_at IS NULL;

-- name: ListOrganizationMembers :many
SELECT users.id::text AS user_id, users.email, org_members.role,
       org_members.incident_notifications_enabled, org_members.created_at
FROM org_members
JOIN users ON users.id = org_members.user_id
WHERE org_members.organization_id = sqlc.arg(organization_id)::text::uuid
ORDER BY org_members.created_at, users.id
LIMIT 100;

-- name: ExistingOrganizationMemberByEmail :one
SELECT EXISTS (
    SELECT 1 FROM org_members
    JOIN users ON users.id = org_members.user_id
    WHERE org_members.organization_id = sqlc.arg(organization_id)::text::uuid
      AND lower(btrim(users.email)) = sqlc.arg(email)
) AS member_exists;

-- name: InvalidatePendingInvitation :execrows
UPDATE org_invitations SET accepted_at = CURRENT_TIMESTAMP
WHERE organization_id = sqlc.arg(organization_id)::text::uuid
  AND lower(btrim(email)) = sqlc.arg(email)
  AND accepted_at IS NULL;

-- name: CreateOrganizationInvitation :exec
INSERT INTO org_invitations (organization_id, invited_by_user_id, email, role, token_digest, expires_at)
VALUES (sqlc.arg(organization_id)::text::uuid, sqlc.arg(invited_by_user_id)::text::uuid,
        sqlc.arg(email), sqlc.arg(role), sqlc.arg(token_digest), sqlc.arg(expires_at));

-- name: LockOrganizationInvitation :one
SELECT id::text AS id, organization_id::text AS organization_id, email, role, expires_at, accepted_at
FROM org_invitations
WHERE token_digest = sqlc.arg(token_digest)
FOR UPDATE;

-- name: AcceptOrganizationInvitation :execrows
WITH consumed AS (
    UPDATE org_invitations SET accepted_at = CURRENT_TIMESTAMP
    WHERE id = sqlc.arg(invitation_id)::text::uuid AND accepted_at IS NULL
    RETURNING organization_id, role
)
INSERT INTO org_members (organization_id, user_id, role)
SELECT organization_id, sqlc.arg(user_id)::text::uuid, role FROM consumed
ON CONFLICT (organization_id, user_id) DO NOTHING;
