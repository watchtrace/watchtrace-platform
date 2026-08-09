# Default Ownership API

P1-103 provides the authenticated transaction that creates a user's initial
tenant hierarchy.

## `POST /api/v1/organizations`

The endpoint requires the short-lived session from the authentication API:

```text
Authorization: Bearer wt_local_<opaque-value>
Content-Type: application/json
```

The request contains only customer-selectable organization and project data:

```json
{
  "organization": {
    "name": "Example Organization",
    "slug": "example-organization"
  },
  "project": {
    "name": "Main API",
    "description": "Production API monitoring"
  }
}
```

Names are trimmed and limited to 120 bytes. Slugs are normalized to lowercase,
limited to 63 bytes, and contain only lowercase letters, digits, and internal
hyphens. Project descriptions are trimmed and limited to 1,000 bytes.

The caller cannot choose the owner user ID, role, environment name, or
environment type. The authenticated user becomes the sole `owner`, and the
server creates a `Production` environment of type `production`.

A successful request returns HTTP 201:

```json
{
  "organization": {
    "id": "f282490f-9476-469b-96cd-8cf9cfbf0734",
    "name": "Example Organization",
    "slug": "example-organization"
  },
  "membership": {
    "organization_id": "f282490f-9476-469b-96cd-8cf9cfbf0734",
    "user_id": "0f7ddd2e-3d0d-48a8-8752-64b684cc96ef",
    "role": "owner"
  },
  "project": {
    "id": "9939dc4f-8cab-4c09-9382-4ff66377f232",
    "organization_id": "f282490f-9476-469b-96cd-8cf9cfbf0734",
    "name": "Main API",
    "description": "Production API monitoring"
  },
  "environment": {
    "id": "b726f37d-978f-4796-a6a5-c3b86bb1c8c2",
    "organization_id": "f282490f-9476-469b-96cd-8cf9cfbf0734",
    "project_id": "9939dc4f-8cab-4c09-9382-4ff66377f232",
    "name": "Production",
    "type": "production"
  }
}
```

PostgreSQL creates the organization, owner membership, project, and production
environment in one transaction. A failure at any step leaves none of those
rows behind. The environment's composite foreign key also prevents a project
from another organization from being linked.

| HTTP status | Code | Meaning |
|---:|---|---|
| 401 | `invalid_session` | Bearer session is missing, invalid, or expired. |
| 409 | `organization_slug_in_use` | The normalized slug already exists. |
| 422 | `validation_failed` | Organization or project data is invalid. |

## Membership and invitations

`GET /api/v1/organizations/{orgId}/members` returns the current members for a
tenant where the caller has a current database membership. Every role may read
the member list.

Owners and admins may call `POST /api/v1/organizations/{orgId}/invitations`:

```json
{"email":"teammate@example.com","role":"member"}
```

The assignable roles are `admin`, `member`, and `viewer`; ownership cannot be
transferred through an invitation. The API returns safe invitation metadata,
never the raw token. A replacement request invalidates the previous pending
invitation, and an existing member returns HTTP 409 `already_member`.

The email link supplies its token to authenticated
`POST /api/v1/auth/accept-invitation` as `{"token":"wt_invite_<opaque>"}`.
The authenticated account must have a verified email matching the invitation.
Tokens expire after seven days, are stored only as SHA-256 digests, and cannot
be reused. Acceptance and membership creation are one transaction.

The server reads roles from `org_members` on every operation. Owners and admins
manage invitations and monitors; members manage monitors; viewers have
read-only monitor and membership access. Requests outside the caller's tenant
return the same not-found response as unknown tenant resources.
