DROP TABLE health_checks;

DROP INDEX check_jobs_claim_idx;

ALTER TABLE check_jobs
    DROP CONSTRAINT check_jobs_lease_consistency_check,
    DROP CONSTRAINT check_jobs_attempt_limit_check,
    DROP COLUMN lease_expires_at,
    DROP COLUMN lease_token,
    DROP COLUMN lease_owner,
    DROP COLUMN max_attempts,
    DROP COLUMN attempt_count;
