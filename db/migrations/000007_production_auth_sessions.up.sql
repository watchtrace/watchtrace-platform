ALTER TABLE auth_sessions
    ADD COLUMN family_id uuid,
    ADD COLUMN revoked_at timestamptz;

UPDATE auth_sessions
SET family_id = gen_random_uuid();

ALTER TABLE auth_sessions
    ALTER COLUMN family_id SET NOT NULL,
    ADD CONSTRAINT auth_sessions_revoked_at_check
        CHECK (revoked_at IS NULL OR revoked_at >= created_at);

CREATE INDEX auth_sessions_active_family_idx
    ON auth_sessions (family_id)
    WHERE revoked_at IS NULL;

CREATE TABLE refresh_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id),
    family_id uuid NOT NULL,
    token_digest bytea NOT NULL UNIQUE
        CHECK (octet_length(token_digest) = 32),
    expires_at timestamptz NOT NULL,
    rotated_at timestamptz,
    revoked_at timestamptz,
    replaced_by_id uuid REFERENCES refresh_tokens (id),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (expires_at > created_at),
    CHECK (rotated_at IS NULL OR rotated_at >= created_at),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at),
    CHECK (
        (rotated_at IS NULL AND replaced_by_id IS NULL)
        OR
        (rotated_at IS NOT NULL AND replaced_by_id IS NOT NULL)
    )
);

CREATE INDEX refresh_tokens_active_family_idx
    ON refresh_tokens (family_id, expires_at)
    WHERE revoked_at IS NULL;
