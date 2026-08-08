DROP TABLE refresh_tokens;

DROP INDEX auth_sessions_active_family_idx;

ALTER TABLE auth_sessions
    DROP CONSTRAINT auth_sessions_revoked_at_check,
    DROP COLUMN revoked_at,
    DROP COLUMN family_id;
