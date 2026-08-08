# API Conventions

These conventions apply to every WatchTrace HTTP API endpoint. The versioned
OpenAPI contract will reference them when it is introduced by P1-508.

## Request IDs

Every response includes an `X-Request-ID` header. A client-provided ID is kept
only when it is 1–64 characters, begins with an ASCII letter or digit, and
contains only ASCII letters, digits, `.`, `_`, or `-`. Missing or invalid IDs
are replaced with a generated UUID.

The same ID is available to handlers through both the Gin context and the
standard Go request context. API error responses include it for support and log
correlation.

## Error responses

API errors use one JSON envelope:

```json
{
  "error": {
    "code": "invalid_request",
    "message": "request body is invalid",
    "request_id": "36f27662-3e8b-4ac7-96b6-70ddfbe5fe9f"
  }
}
```

`code` is stable and intended for clients. `message` is safe for users and must
not contain internal errors, request bodies, credentials, or database details.
The initial standard codes are:

| HTTP status | Code |
|---:|---|
| 400 | `invalid_request` |
| 401 | `invalid_credentials` |
| 401 | `invalid_session` |
| 404 | `not_found` |
| 404 | `environment_not_found` |
| 405 | `method_not_allowed` |
| 409 | `email_in_use` |
| 409 | `organization_slug_in_use` |
| 409 | `monitor_limit_reached` |
| 415 | `unsupported_media_type` |
| 422 | `validation_failed` |
| 500 | `internal_error` |

## JSON requests

JSON endpoints use `Content-Type: application/json`. The shared decoder:

- permits one JSON value only;
- rejects unknown object fields;
- applies Gin binding validation tags;
- rejects bodies larger than 1 MiB; and
- returns safe errors without echoing body content or decoder details.

## Health endpoints

| Endpoint | Meaning | Success | Failure |
|---|---|---:|---:|
| `GET /health` | Backward-compatible liveness alias | 200 | Process unavailable |
| `GET /health/live` | Process is running and serving HTTP | 200 | Process unavailable |
| `GET /health/ready` | Registered readiness checks pass | 200 | 503 |

Health responses set `Cache-Control: no-store`. A failed readiness check returns
`{"status":"not_ready"}` without exposing its internal error. The API command
registers a PostgreSQL connectivity check. P1-513 will add essential-worker
checks and complete shutdown-aware readiness.

## Safe request logging

Request logs include the request ID, HTTP method, matched route template,
status, duration, and component. They deliberately exclude raw URLs, query
strings, request and response bodies, client IP addresses, authorization
headers, cookies, and arbitrary headers. Panic logs include a stack trace but
never include the recovered panic value.

Authentication-specific request and response shapes are documented in
[`AUTHENTICATION.md`](AUTHENTICATION.md).
