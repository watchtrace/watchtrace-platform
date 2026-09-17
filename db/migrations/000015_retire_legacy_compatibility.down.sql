ALTER TABLE check_jobs
    ADD COLUMN attempt_count smallint NOT NULL DEFAULT 0
      CHECK (attempt_count BETWEEN 0 AND 3),
    ADD COLUMN max_attempts smallint NOT NULL DEFAULT 3
      CHECK (max_attempts = 3),
    ADD COLUMN lease_owner text
      CHECK (lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 128)),
    ADD COLUMN lease_token uuid,
    ADD COLUMN lease_expires_at timestamptz,
    ADD CONSTRAINT check_jobs_attempt_limit_check
      CHECK (attempt_count <= max_attempts);

UPDATE monitor_result_evaluations
SET consecutive_failures = LEAST(consecutive_failures, 3),
    consecutive_successes = LEAST(consecutive_successes, 2);

ALTER TABLE monitor_result_evaluations
    DROP CONSTRAINT monitor_result_evaluations_consecutive_failures_check,
    DROP CONSTRAINT monitor_result_evaluations_consecutive_successes_check,
    ADD CONSTRAINT monitor_result_evaluations_consecutive_failures_check
      CHECK (consecutive_failures BETWEEN 0 AND 3),
    ADD CONSTRAINT monitor_result_evaluations_consecutive_successes_check
      CHECK (consecutive_successes BETWEEN 0 AND 2);
