ALTER TABLE user_action_tokens
    DROP CONSTRAINT user_action_tokens_purpose_check,
    ADD CONSTRAINT user_action_tokens_purpose_check
        CHECK (purpose IN ('email_verification', 'password_reset'));

DROP INDEX user_action_tokens_active_user_purpose_idx;

CREATE UNIQUE INDEX user_action_tokens_active_user_purpose_idx
    ON user_action_tokens (user_id, purpose)
    WHERE used_at IS NULL;
