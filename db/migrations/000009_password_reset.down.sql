DELETE FROM user_action_tokens
WHERE purpose = 'password_reset';

DROP INDEX user_action_tokens_active_user_purpose_idx;

CREATE INDEX user_action_tokens_active_user_purpose_idx
    ON user_action_tokens (user_id, purpose, expires_at)
    WHERE used_at IS NULL;

ALTER TABLE user_action_tokens
    DROP CONSTRAINT user_action_tokens_purpose_check,
    ADD CONSTRAINT user_action_tokens_purpose_check
        CHECK (purpose = 'email_verification');
