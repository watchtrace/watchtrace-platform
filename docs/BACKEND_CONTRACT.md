# Phase 1 Backend Contract

The customer API is frozen at version `v1` for the Phase 1 React application.
The machine-readable contract is
[`api/customer-v1.openapi.yaml`](../api/customer-v1.openapi.yaml). PostgreSQL is
the source of truth; browser code must not query the database, SQS, or worker
gateways directly.

## Customer capabilities

Authenticated APIs cover the complete Phase 1 workflow:

- account signup, verification, login, refresh, logout, invitation acceptance,
  and password reset;
- organization, membership, notification preference, project, and environment
  list/read/create/update/supported-delete operations;
- monitor list/read/create/update/test/pause/resume/soft-delete operations;
- recent checks, scheduled-only reliability reports, environment dashboards,
  incident lists/timelines/notification states, acknowledgement, and manual
  resolution; and
- tenant-scoped SSE refresh hints with ordinary API polling as the recovery
  path.

Tenant objects return the caller's current effective `role` and
`allowed_actions`. Roles are read on every request. Cross-organization objects
are returned as not found, and every child query retains its organization or
environment boundary.

## Bounds and time

Timestamps use RFC 3339 and UTC. Reporting/read endpoints require `from` and
`to`. Recent checks allow at most 31 days, dashboards 31 days, and monitor and
incident reports 366 days. Cursor pages contain at most 100 records and use a
stable timestamp-plus-UUID ordering. Tenant and monitor collection responses
are capped at 100 records; Phase 1 product limits keep those collections below
that cap.

Reliability excludes manual checks. It returns observed uptime and coverage
separately, returns JSON `null` when a ratio has a zero denominator, and
reports pending rollup correction through `fresh` and `corrected_at`.

## Live refresh and polling

`GET /api/v1/environments/{environmentId}/events` accepts `Last-Event-ID` and
replays at most 100 committed events. Each event contains only its durable ID,
type, resource type, and resource ID. It never contains customer records,
emails, tokens, monitor URLs, or headers. Events are hints: a client reloads
the relevant ordinary API and performs a full dashboard poll every 15 seconds
while disconnected or when replay history is unavailable.

## Errors and rate limits

All failures use the safe error envelope documented in
[`API_CONVENTIONS.md`](API_CONVENTIONS.md). Authentication, invitations,
mutations, manual checks, reports, and concurrent SSE connections have separate
fixed-window limits. A `429` response includes `Retry-After` in seconds.

## Operational health and maintenance

`GET /health/operations` returns bounded, non-sensitive PostgreSQL health:
scheduler and result-consumer delay, completed/failed/missed checks,
notification age, job-ledger/outbox state and age, disk pressure, and the last
start/success/failure of each maintenance task. The monitor engine's `/metrics`
adds SQS available, delayed, and in-flight counts for both FIFO queues and both
DLQs. SQS's native oldest-message metric remains part of the manual Phase 1
operator dashboard; database ledger/outbox ages provide the corresponding
application-side age.

Workers expose journal counts and oldest accepted journal age on `/metrics`.
The database-free gateway exposes configured-pool and active-pull counts.
Their `/health/ready` endpoints reflect whether they can accept new work.

The API automatically removes expired sessions, action tokens, invitations,
terminal notification deliveries, and old refresh hints. The monitor engine
reclaims queue leases, expires dead work, removes old ledger rows, repairs
rollups, and applies raw/rollup retention. Every scheduled maintenance family
records last-success state. `backup` is an explicit status slot updated by the
deployment backup job; backup creation and restore remain deployment/runbook
operations.

After the deployment backup and its integrity check complete, the job records
the safe timestamp without exposing backup credentials:

```sh
go run ./cmd/maintenance record-backup-success
```

On shutdown the API stops accepting new connections and gives in-flight HTTP
requests the configured bounded grace period. Queue loops receive cancellation,
stop new scheduling and claims, and have a ten-second process drain bound.

## Contract changes

The Phase 1 contract is frozen after the Phase 1.4 gate. A required frontend
behavior that is absent from OpenAPI is a backend defect: reopen the applicable
Phase 1.4 package, add tests, update the contract, and verify the backend before
resuming frontend work.
