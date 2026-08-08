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
