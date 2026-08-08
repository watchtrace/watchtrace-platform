# Account and Ownership Schema

Migration `000002_accounts_and_ownership` introduces the minimum relational
schema for account identity and tenant ownership:

- `users` stores a UUID identity, case-insensitively unique email, password
  hash, and optional email-verification time.
- `organizations` is the tenant root and has a case-insensitively unique slug.
- `org_members` associates users with organizations using the `owner`, `admin`,
  `member`, or `viewer` role and records the incident-notification preference.
- `projects` belongs directly to an organization.
- `environments` carries its organization ID and references a project through
  the composite `(organization_id, project_id)` key. PostgreSQL therefore
  rejects a project from another organization even if both identifiers exist.

Public identifiers are database-generated UUIDv4 values. All time columns use
PostgreSQL `timestamptz`, which represents instants independently of the client
or server session time zone. Organization deletion is represented by nullable
`deleted_at`; the ownership foreign keys intentionally do not cascade deletes.

A partial unique index allows at most one `owner` membership per organization.
Creating the user, organization, owner membership, project, and default
production environment atomically—and ensuring a newly created organization
never exists without an owner—belongs to P1-103 rather than this schema-only
task.
