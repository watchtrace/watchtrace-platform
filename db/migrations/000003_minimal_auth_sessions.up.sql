CREATE TABLE auth_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id),
    token_digest bytea NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (expires_at > created_at)
);

CREATE INDEX auth_sessions_expires_at_idx ON auth_sessions (expires_at);
