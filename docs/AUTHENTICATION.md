# Authentication API

WatchTrace uses short-lived bearer access tokens and rotating refresh-token
families. Raw access and refresh tokens are returned only to the client; the
database stores their SHA-256 digests.

## Endpoints

### `POST /api/v1/auth/signup`

Creates a user without creating an organization, project, or environment.
Successful requests return HTTP 201.

### `POST /api/v1/auth/login`

Authenticates an existing user. An unknown email and an incorrect password
both return the same HTTP 401 `invalid_credentials` response. Successful
requests return HTTP 200.

Signup and login accept only `application/json` with this shape:

```json
{
  "email": "user@example.com",
  "password": "a-password-of-at-least-12-bytes"
}
```

Emails are trimmed, normalized to lowercase, validated, and limited to 254
bytes. Passwords must contain 12-1024 bytes. Passwords are hashed with Argon2id
using a unique random salt and are never returned or logged.

### `POST /api/v1/auth/refresh`

Rotates the refresh token received in the `watchtrace_refresh` cookie and
returns a new access token. The request does not have a JSON body. A missing,
invalid, expired, revoked, or previously rotated refresh token returns HTTP 401
`invalid_refresh_token` without revealing which condition occurred.

Each successful rotation invalidates the presented refresh token and issues a
replacement in the same family. Reusing an already rotated token revokes every
access and refresh token in that family. A later login creates an independent
family.

## Tokens and cookies

Successful signup, login, and refresh responses include a short-lived access
token:

```json
{
  "user": {
    "id": "9c0d5e17-7862-4435-a1a2-95664ef557d7",
    "email": "user@example.com",
    "email_verified": false
  },
  "session": {
    "token": "wt_access_<opaque-value>",
    "token_type": "Bearer",
    "expires_at": "2026-08-08T12:15:00Z"
  }
}
```

Access tokens expire after 15 minutes and are sent as `Authorization: Bearer
<token>` to authenticated endpoints. Existing `wt_local_` access tokens issued
before this migration remain valid until their original expiry.

The refresh token is never included in JSON. It is an opaque random token with
a 30-day expiry delivered only through `Set-Cookie`. The cookie is `HttpOnly`,
`SameSite=Strict`, and scoped to `/api/v1/auth`. Set
`WATCHTRACE_ENVIRONMENT=production` in production so the cookie also has the
required `Secure` attribute; the default development mode permits local HTTP.
Authentication responses set `Cache-Control: no-store`.

## Errors

In addition to the shared API errors:

| HTTP status | Code | Meaning |
|---:|---|---|
| 401 | `invalid_credentials` | Email or password did not authenticate. |
| 401 | `invalid_refresh_token` | The refresh session could not be rotated. |
| 401 | `invalid_session` | A protected route received no valid bearer session. |
| 409 | `email_in_use` | The normalized signup email already exists. |
| 422 | `validation_failed` | Email or password failed validation. |

Logout, explicit current/all-session revocation, and expired-session cleanup
are assigned to P1-202. Email verification and password reset remain assigned
to their later Phase 1 tasks.
