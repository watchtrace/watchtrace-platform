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

### `POST /api/v1/auth/logout`

Logout accepts only `application/json`:

```json
{
  "all_sessions": false
}
```

With `all_sessions` omitted or `false`, WatchTrace revokes the access and
refresh tokens in the family identified by the browser refresh cookie. With
`all_sessions` set to `true`, an active refresh token revokes every access and
refresh family for the user. Successful logout returns HTTP 204 and clears the
refresh cookie. Missing, malformed, unknown, expired, and already revoked
cookies are idempotent successes and do not reveal session validity.

### `POST /api/v1/auth/verify-email`

Signup creates a random 256-bit verification token with a 24-hour lifetime and
delivers it by email. The raw token is never returned by the API, written to
logs, or stored in PostgreSQL; only its SHA-256 digest is stored.

The verification endpoint accepts only `application/json`:

```json
{
  "token": "wt_verify_<opaque-value>"
}
```

A valid token atomically sets the user's verification time and marks the token
used. Success returns HTTP 200 with the safe `user` object and
`email_verified: true`. Missing, malformed, unknown, expired, and already used
tokens all return the same HTTP 400 `invalid_verification_token` response.
Tokens cannot be reused.

### `POST /api/v1/auth/forgot-password`

Accepts `{"email":"user@example.com"}` and always returns HTTP 202 with an
empty body for a syntactically valid email. Known and unknown accounts, plus
database or local delivery failures, have the same public response. For a
known account, WatchTrace invalidates any previous unused reset token and
emails a new random 256-bit token with a one-hour lifetime. Only its SHA-256
digest is stored.

### `POST /api/v1/auth/reset-password`

Accepts only `application/json`:

```json
{
  "token": "wt_reset_<opaque-value>",
  "new_password": "a-new-password-of-at-least-12-bytes"
}
```

A valid token is consumed in the same transaction that replaces the Argon2id
password hash, invalidates the user's other unused reset tokens, and revokes
all existing access and refresh sessions. Success returns HTTP 204 and clears
the refresh cookie. Missing, malformed, unknown, expired, and already used
tokens all return HTTP 400 `invalid_reset_token`. Tokens cannot be reused.

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

The API removes expired and revoked access-token rows in bounded hourly
batches. It retains revoked refresh-token families until every token in the
family has expired, then removes the family in a bounded batch. This retention
is deliberate: deleting an unexpired rotated token would prevent replay
detection.

## Errors

In addition to the shared API errors:

| HTTP status | Code | Meaning |
|---:|---|---|
| 401 | `invalid_credentials` | Email or password did not authenticate. |
| 401 | `invalid_refresh_token` | The refresh session could not be rotated. |
| 401 | `invalid_session` | A protected route received no valid bearer session. |
| 400 | `invalid_verification_token` | The email-verification token is invalid, expired, or used. |
| 400 | `invalid_reset_token` | The password-reset token is invalid, expired, or used. |
| 409 | `email_in_use` | The normalized signup email already exists. |
| 422 | `validation_failed` | Email or password failed validation. |

For local development, Docker Compose runs Mailpit with SMTP on
`127.0.0.1:1025` and its mailbox UI on `http://127.0.0.1:8025`. The built-in
adapter refuses non-loopback SMTP servers and non-loopback action URLs,
preventing this plaintext development path from being mistaken for production
delivery. Set `WATCHTRACE_PASSWORD_RESET_URL` to the local reset page URL;
OCI/provider email delivery remains assigned to P1-406.
