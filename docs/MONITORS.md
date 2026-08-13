# Monitor Lifecycle API

Phase 1.2 extends the original tenant-scoped API to the complete GET/HEAD
lifecycle with encrypted custom headers, pause/resume, soft deletion, stable
versions, and bounded manual tests.

All three endpoints require `Authorization: Bearer <token>` and use the environment
identifier from the path:

```text
POST /api/v1/environments/{environmentId}/monitors
GET  /api/v1/environments/{environmentId}/monitors
GET  /api/v1/environments/{environmentId}/monitors/{monitorId}
PUT  /api/v1/environments/{environmentId}/monitors/{monitorId}
DELETE /api/v1/environments/{environmentId}/monitors/{monitorId}
POST /api/v1/environments/{environmentId}/monitors/{monitorId}/pause
POST /api/v1/environments/{environmentId}/monitors/{monitorId}/resume
POST /api/v1/environments/{environmentId}/monitors/{monitorId}/test
```

The server resolves the environment's organization and the caller's current
membership from PostgreSQL. Callers do not provide an organization ID. An
unknown environment and an environment in another organization both return the
same HTTP 404 response.

Owners, admins, and members may create monitors. Viewers may list monitors but
cannot create them. Creation is serialized per organization, enforcing the
initial limit of 100 monitors even when requests arrive concurrently.

## Create a monitor

```json
{
  "name": "API health",
  "url": "https://api.example.com/health",
  "interval_seconds": 300,
  "timeout_seconds": 5,
  "expected_status_min": 200,
  "expected_status_max": 299,
  "method": "GET",
  "headers": {"Authorization": "Bearer secret"},
  "worker_pool_id": "hosted"
}
```

Only `name` and `url` are required. Defaults and accepted values are:

| Field | Default | Accepted values |
|---|---:|---|
| Method | `GET` | `GET` or `HEAD` |
| Interval | 300 seconds | 60, 120, 300, 600, or 1,800 seconds |
| Timeout | 5 seconds | 1–10 seconds |
| Expected status | 200–299 | Ordered range within 100–599 |

Names are trimmed and limited to 120 bytes. URLs are trimmed, limited to 2,048
bytes, must be absolute HTTP or HTTPS URLs, and cannot include user information
or a fragment. Only ports 80 and 443 are accepted. Literal private, loopback,
link-local, multicast, metadata, and other special-use IPv4 and IPv6 targets
are rejected during creation; obvious localhost names are rejected as well.

Hostname resolution is intentionally repeated at request time rather than
trusted from monitor creation. The destination guard validates every resolved
IPv4 and IPv6 address immediately before dialing and pins the connection to a
validated IP, so the HTTP transport cannot perform a second unvalidated DNS
lookup. Any unsafe answer rejects the whole resolution result. Environment
HTTP proxy settings are ignored because a proxy would move the actual network
boundary away from the guarded dialer.

Redirects are limited to three; every hop is revalidated and all custom headers
are stripped if the hostname changes. Header values are encrypted with a
versioned key held outside PostgreSQL. Responses return sorted `header_names`
only, never values. The checker never stores response bodies.

A successful creation returns HTTP 201 with the stored configuration:

```json
{
  "id": "845d1e0d-933c-41a6-9af3-f479c757868b",
  "organization_id": "7768ec42-f9d1-4afb-bb12-1b691233ae32",
  "environment_id": "326d2fd5-70ed-444e-b713-6126f94c12e9",
  "name": "API health",
  "url": "https://api.example.com/health",
  "method": "GET",
  "interval_seconds": 300,
  "timeout_seconds": 5,
  "expected_status_min": 200,
  "expected_status_max": 299,
  "created_at": "2026-08-08T12:00:00Z",
  "updated_at": "2026-08-08T12:00:00Z",
  "version": 1,
  "paused": false,
  "worker_pool_id": "hosted",
  "header_names": ["Authorization"]
}
```

## List monitors

The list endpoint returns a stable creation order and a non-null array. The
organization currently has a hard limit of 100 monitors, so this initial list
is bounded without pagination.

```json
{
  "monitors": []
}
```

## Update, pause, resume, delete, and test

`PUT` replaces the configurable fields and increments `version`. Pausing or
editing stops future scheduling but cannot mutate an already encrypted and
published job snapshot. Resuming returns to the monitor's stable schedule.
`DELETE` is a soft delete and removes the monitor from list/detail/scheduling.

`POST .../test` creates a `manual_test` job and immutable encrypted outbox row
in one transaction. Manual jobs share the same two-minute expiry and worker
protocol but are capped globally and never affect uptime or incident state.

## Read monitor state and recent checks

The detail endpoint returns the stored monitor configuration, its current
state, and at most 20 recent results ordered by descending `scheduled_at` and
then ascending `job_id`. The fixed limit keeps this first dashboard query
bounded; the later history API adds explicit time ranges and cursor pagination.

```json
{
  "id": "845d1e0d-933c-41a6-9af3-f479c757868b",
  "organization_id": "7768ec42-f9d1-4afb-bb12-1b691233ae32",
  "environment_id": "326d2fd5-70ed-444e-b713-6126f94c12e9",
  "name": "API health",
  "url": "https://api.example.com/health",
  "method": "GET",
  "interval_seconds": 300,
  "timeout_seconds": 5,
  "expected_status_min": 200,
  "expected_status_max": 299,
  "created_at": "2026-08-08T12:00:00Z",
  "updated_at": "2026-08-08T12:00:00Z",
  "state": "degraded",
  "recent_checks": [
    {
      "job_id": "6474d7ab-e0e1-43bb-9995-4913441d46e2",
      "job_type": "scheduled",
      "scheduled_at": "2026-08-08T12:05:00Z",
      "started_at": "2026-08-08T12:05:01Z",
      "completed_at": "2026-08-08T12:05:01.25Z",
      "succeeded": false,
      "status_code": 503,
      "error_category": "unexpected_status",
      "total_duration_microseconds": 250000
    }
  ]
}
```

The initial state semantics use the shared Phase 1 vocabulary:

- `unknown`: no scheduled result exists; missing data is never considered
  healthy.
- `healthy`: the latest scheduled result succeeded.
- `degraded`: the latest scheduled result failed.

The `down` state and consecutive failure/recovery counters are owned by the
later monitor state-machine task. Manual-test results may appear in
`recent_checks`, but they never affect current state, uptime, or incidents.
Nullable `status_code` and `error_category` fields are returned as JSON `null`.
Response bodies are not part of the storage or API contract.

Every detail query is qualified by organization, environment, and monitor
after the caller's current membership is resolved. An inaccessible environment
returns `environment_not_found`; a missing monitor, a monitor in another
organization, and a monitor from a different environment all return the same
`monitor_not_found` response.

| HTTP status | Code | Meaning |
|---:|---|---|
| 401 | `invalid_session` | Bearer session is missing, invalid, or expired. |
| 404 | `environment_not_found` | Environment is absent or inaccessible. |
| 404 | `monitor_not_found` | Monitor is absent or outside the authorized environment. |
| 409 | `monitor_limit_reached` | Organization already has 100 monitors. |
| 429 | `manual_queue_full` | The bounded manual-test queue is full. |
| 503 | `monitor_queue_unavailable` | The selected worker pool is not provisioned. |
| 422 | `validation_failed` | Monitor configuration is invalid. |
