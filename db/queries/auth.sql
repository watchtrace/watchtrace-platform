-- name: CreateUser :one
INSERT INTO users (email, password_hash)
VALUES ($1, $2)
RETURNING id::text AS id, email, (email_verified_at IS NOT NULL)::boolean AS email_verified;

-- name: GetUserForLogin :one
SELECT
    id::text AS id,
    email,
    password_hash,
    (email_verified_at IS NOT NULL)::boolean AS email_verified
FROM users
WHERE lower(btrim(email)) = $1;

-- name: CreateAuthSession :exec
INSERT INTO auth_sessions (user_id, token_digest, expires_at)
VALUES (sqlc.arg(user_id)::text::uuid, sqlc.arg(token_digest), sqlc.arg(expires_at));

-- name: GetUserByAuthSession :one
SELECT
    users.id::text AS id,
    users.email,
    (users.email_verified_at IS NOT NULL)::boolean AS email_verified
FROM auth_sessions
JOIN users ON users.id = auth_sessions.user_id
WHERE auth_sessions.token_digest = $1
  AND auth_sessions.expires_at > CURRENT_TIMESTAMP;
