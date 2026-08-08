DROP TABLE check_jobs;

DROP INDEX monitors_due_schedule_idx;

ALTER TABLE monitors
    DROP CONSTRAINT monitors_tenant_environment_id_key,
    DROP COLUMN next_check_at;
