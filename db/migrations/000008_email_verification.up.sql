CREATE TABLE user_action_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id),
    purpose text NOT NULL CHECK (purpose = 'email_verification'),
    token_digest bytea NOT NULL UNIQUE
        CHECK (octet_length(token_digest) = 32),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (expires_at > created_at),
    CHECK (used_at IS NULL OR used_at >= created_at)
);

CREATE INDEX user_action_tokens_active_user_purpose_idx
    ON user_action_tokens (user_id, purpose, expires_at)
    WHERE used_at IS NULL;
