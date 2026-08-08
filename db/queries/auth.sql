-- name: CreateUser :one
INSERT INTO users (email, password_hash)
VALUES ($1, $2)
RETURNING id::text AS id, email, (email_verified_at IS NOT NULL)::boolean AS email_verified;

-- name: CreateEmailVerificationToken :exec
INSERT INTO user_action_tokens (user_id, purpose, token_digest, expires_at)
VALUES (
    sqlc.arg(user_id)::text::uuid,
    'email_verification',
    sqlc.arg(token_digest),
    sqlc.arg(expires_at)
);

-- name: LockEmailVerificationToken :one
SELECT
    user_action_tokens.id::text AS id,
    user_action_tokens.expires_at,
    user_action_tokens.used_at
FROM user_action_tokens
WHERE user_action_tokens.purpose = 'email_verification'
  AND user_action_tokens.token_digest = sqlc.arg(token_digest)
FOR UPDATE;

-- name: CompleteEmailVerification :one
WITH consumed AS (
    UPDATE user_action_tokens
    SET used_at = CURRENT_TIMESTAMP
    WHERE id = sqlc.arg(token_id)::text::uuid
      AND purpose = 'email_verification'
      AND used_at IS NULL
    RETURNING user_id
)
UPDATE users
SET email_verified_at = COALESCE(email_verified_at, CURRENT_TIMESTAMP),
    updated_at = CURRENT_TIMESTAMP
FROM consumed
WHERE users.id = consumed.user_id
RETURNING
    users.id::text AS id,
    users.email,
    (users.email_verified_at IS NOT NULL)::boolean AS email_verified;

-- name: GetUserForPasswordReset :one
SELECT id::text AS id, email
FROM users
WHERE lower(btrim(email)) = $1;

-- name: InvalidateActivePasswordResetTokens :execrows
UPDATE user_action_tokens
SET used_at = CURRENT_TIMESTAMP
WHERE user_id = sqlc.arg(user_id)::text::uuid
  AND purpose = 'password_reset'
  AND used_at IS NULL;

-- name: CreatePasswordResetToken :exec
INSERT INTO user_action_tokens (user_id, purpose, token_digest, expires_at)
VALUES (
    sqlc.arg(user_id)::text::uuid,
    'password_reset',
    sqlc.arg(token_digest),
    sqlc.arg(expires_at)
);

-- name: LockPasswordResetToken :one
SELECT
    user_action_tokens.id::text AS id,
    user_action_tokens.user_id::text AS user_id,
    user_action_tokens.expires_at,
    user_action_tokens.used_at
FROM user_action_tokens
WHERE user_action_tokens.purpose = 'password_reset'
  AND user_action_tokens.token_digest = sqlc.arg(token_digest)
FOR UPDATE;

-- name: CompletePasswordReset :execrows
WITH consumed AS (
    UPDATE user_action_tokens
    SET used_at = CURRENT_TIMESTAMP
    WHERE id = sqlc.arg(token_id)::text::uuid
      AND purpose = 'password_reset'
      AND used_at IS NULL
    RETURNING user_id
)
UPDATE users
SET password_hash = sqlc.arg(password_hash),
    updated_at = CURRENT_TIMESTAMP
FROM consumed
WHERE users.id = consumed.user_id;

-- name: GetUserForLogin :one
SELECT
    id::text AS id,
    email,
    password_hash,
    (email_verified_at IS NOT NULL)::boolean AS email_verified
FROM users
WHERE lower(btrim(email)) = $1;

-- name: CreateAuthSession :exec
INSERT INTO auth_sessions (user_id, family_id, token_digest, expires_at)
VALUES (
    sqlc.arg(user_id)::text::uuid,
    sqlc.arg(family_id)::text::uuid,
    sqlc.arg(token_digest),
    sqlc.arg(expires_at)
);

-- name: CreateRefreshTokenFamily :one
INSERT INTO refresh_tokens (user_id, family_id, token_digest, expires_at)
VALUES (
    sqlc.arg(user_id)::text::uuid,
    gen_random_uuid(),
    sqlc.arg(token_digest),
    sqlc.arg(expires_at)
)
RETURNING id::text AS id, family_id::text AS family_id;

-- name: CreateRotatedRefreshToken :one
INSERT INTO refresh_tokens (user_id, family_id, token_digest, expires_at)
VALUES (
    sqlc.arg(user_id)::text::uuid,
    sqlc.arg(family_id)::text::uuid,
    sqlc.arg(token_digest),
    sqlc.arg(expires_at)
)
RETURNING id::text AS id;

-- name: LockRefreshTokenForRotation :one
SELECT
    refresh_tokens.id::text AS id,
    refresh_tokens.user_id::text AS user_id,
    refresh_tokens.family_id::text AS family_id,
    refresh_tokens.expires_at,
    refresh_tokens.rotated_at,
    refresh_tokens.revoked_at,
    users.email,
    (users.email_verified_at IS NOT NULL)::boolean AS email_verified
FROM refresh_tokens
JOIN users ON users.id = refresh_tokens.user_id
WHERE refresh_tokens.token_digest = sqlc.arg(token_digest)
FOR UPDATE OF refresh_tokens;

-- name: MarkRefreshTokenRotated :execrows
UPDATE refresh_tokens
SET rotated_at = CURRENT_TIMESTAMP,
    replaced_by_id = sqlc.arg(replaced_by_id)::text::uuid
WHERE id = sqlc.arg(id)::text::uuid
  AND rotated_at IS NULL
  AND revoked_at IS NULL;

-- name: RevokeRefreshTokenFamily :execrows
UPDATE refresh_tokens
SET revoked_at = CURRENT_TIMESTAMP
WHERE family_id = sqlc.arg(family_id)::text::uuid
  AND revoked_at IS NULL;

-- name: RevokeAccessTokenFamily :execrows
UPDATE auth_sessions
SET revoked_at = CURRENT_TIMESTAMP
WHERE family_id = sqlc.arg(family_id)::text::uuid
  AND revoked_at IS NULL;

-- name: RevokeRefreshTokensForUser :execrows
UPDATE refresh_tokens
SET revoked_at = CURRENT_TIMESTAMP
WHERE user_id = sqlc.arg(user_id)::text::uuid
  AND revoked_at IS NULL;

-- name: RevokeAccessTokensForUser :execrows
UPDATE auth_sessions
SET revoked_at = CURRENT_TIMESTAMP
WHERE user_id = sqlc.arg(user_id)::text::uuid
  AND revoked_at IS NULL;

-- name: DeleteExpiredOrRevokedAccessTokens :execrows
WITH candidates AS (
    SELECT id
    FROM auth_sessions
    WHERE expires_at <= CURRENT_TIMESTAMP
       OR revoked_at IS NOT NULL
    ORDER BY COALESCE(revoked_at, expires_at), id
    LIMIT sqlc.arg(batch_size)
)
DELETE FROM auth_sessions
USING candidates
WHERE auth_sessions.id = candidates.id;

-- name: DeleteExpiredRefreshTokenFamilies :execrows
WITH candidates AS (
    SELECT family_id
    FROM refresh_tokens
    GROUP BY family_id
    HAVING max(expires_at) <= CURRENT_TIMESTAMP
    ORDER BY max(expires_at), family_id
    LIMIT sqlc.arg(batch_size)
)
DELETE FROM refresh_tokens
USING candidates
WHERE refresh_tokens.family_id = candidates.family_id;

-- name: GetUserByAuthSession :one
SELECT
    users.id::text AS id,
    users.email,
    (users.email_verified_at IS NOT NULL)::boolean AS email_verified
FROM auth_sessions
JOIN users ON users.id = auth_sessions.user_id
WHERE auth_sessions.token_digest = $1
  AND auth_sessions.expires_at > CURRENT_TIMESTAMP
  AND auth_sessions.revoked_at IS NULL;
