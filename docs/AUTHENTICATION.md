# Minimal Authentication API

P1-102 provides the account session needed by the first backend monitoring
slice. It is intentionally smaller than the production access-and-refresh
session design owned by P1-201.

## Endpoints

### `POST /api/v1/auth/signup`

Creates a user without creating an organization, project, or environment.
Successful requests return HTTP 201.

### `POST /api/v1/auth/login`

Authenticates an existing user. An unknown email and an incorrect password
both return the same HTTP 401 `invalid_credentials` response. Successful
requests return HTTP 200.

Both endpoints accept only `application/json` with this shape:

```json
{
  "email": "user@example.com",
  "password": "a-password-of-at-least-12-bytes"
}
```

Emails are trimmed, normalized to lowercase, validated, and limited to 254
bytes. Passwords must contain 12–1024 bytes. Passwords are hashed with Argon2id
using a unique random salt and are never returned or logged.

The success response is:

```json
{
  "user": {
    "id": "9c0d5e17-7862-4435-a1a2-95664ef557d7",
    "email": "user@example.com",
    "email_verified": false
  },
  "session": {
    "token": "wt_local_<opaque-value>",
    "token_type": "Bearer",
    "expires_at": "2026-08-08T12:15:00Z"
  }
}
```

Authentication responses set `Cache-Control: no-store`. Clients keep the raw
token and send it as `Authorization: Bearer <token>` to authenticated endpoints,
including the [default ownership API](OWNERSHIP_API.md). The database stores
only a SHA-256 digest of the random 256-bit token. Sessions expire after 15
minutes.

## Errors

In addition to the shared API errors:

| HTTP status | Code | Meaning |
|---:|---|---|
| 401 | `invalid_credentials` | Email or password did not authenticate. |
| 401 | `invalid_session` | A protected route received no valid bearer session. |
| 409 | `email_in_use` | The normalized signup email already exists. |
| 422 | `validation_failed` | Email or password failed validation. |

Refresh-token rotation, production browser cookies, logout, revocation, email
verification, and password reset are not part of this local-development
session. They remain assigned to their later Phase 1 tasks.
