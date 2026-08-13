DROP TABLE monitoring_operational_events;
DROP TABLE monitor_rollups_daily;
DROP TABLE monitor_rollups_hourly;
DROP TABLE monitoring_coverage_gaps;
DROP TABLE monitoring_quarantine;
DROP TABLE monitor_schedule_periods;
DROP TABLE monitor_evaluation_positions;
DROP TABLE check_result_conflicts;

DROP INDEX health_checks_result_id_unique_idx;
ALTER TABLE health_checks DROP CONSTRAINT health_checks_error_category_check;
UPDATE health_checks SET error_category='response_body' WHERE error_category='response_too_large';
UPDATE health_checks SET error_category='http_protocol' WHERE error_category='redirect_limit';
ALTER TABLE health_checks
    DROP COLUMN result_id,
    DROP COLUMN first_byte_duration_microseconds,
    DROP COLUMN tls_duration_microseconds,
    DROP COLUMN connect_duration_microseconds,
    DROP COLUMN dns_duration_microseconds,
    DROP COLUMN execution_attempt_id,
    DROP COLUMN worker_id,
    DROP COLUMN worker_pool_id,
    DROP COLUMN snapshot_hash;
ALTER TABLE health_checks ADD CONSTRAINT health_checks_error_category_check CHECK (error_category IN (
  'invalid_target','unsafe_target','dns','connection','tls','timeout','http_protocol','response_body','unexpected_status'));

DROP TABLE check_dispatch_outbox;
DROP FUNCTION IF EXISTS reject_dispatch_payload_mutation();
DROP INDEX check_jobs_terminal_cleanup_idx;
DROP INDEX check_jobs_deadline_idx;
DROP INDEX check_jobs_one_outstanding_scheduled_idx;
ALTER TABLE check_jobs DROP CONSTRAINT check_jobs_state_check;
UPDATE check_jobs
SET state=CASE
      WHEN state='completed' THEN 'completed'
      WHEN state='cancelled' THEN 'cancelled'
      ELSE 'dead'
    END,
    lease_owner=NULL,
    lease_token=NULL,
    lease_expires_at=NULL;
ALTER TABLE check_jobs
    DROP COLUMN last_safe_error,
    DROP COLUMN sqs_message_id,
    DROP COLUMN receive_count,
    DROP COLUMN execution_attempt_id,
    DROP COLUMN worker_id,
    DROP COLUMN processing_token,
    DROP COLUMN expires_at,
    DROP COLUMN snapshot_hash,
    DROP COLUMN worker_pool_id,
    DROP COLUMN monitor_version,
    ADD CONSTRAINT check_jobs_state_check CHECK (state IN ('pending','running','completed','cancelled','dead')),
    ADD CONSTRAINT check_jobs_lease_consistency_check CHECK (
      (state = 'running' AND lease_owner IS NOT NULL AND lease_token IS NOT NULL
       AND lease_expires_at IS NOT NULL AND started_at IS NOT NULL AND completed_at IS NULL)
      OR (state <> 'running' AND lease_owner IS NULL AND lease_token IS NULL AND lease_expires_at IS NULL));
CREATE UNIQUE INDEX check_jobs_one_outstanding_scheduled_idx ON check_jobs (monitor_id)
    WHERE job_type = 'scheduled' AND state IN ('pending','running');

DROP INDEX monitors_due_schedule_idx;
ALTER TABLE monitors
    DROP CONSTRAINT monitors_headers_encryption_check,
    DROP COLUMN worker_pool_id,
    DROP COLUMN header_key_version,
    DROP COLUMN headers_ciphertext,
    DROP COLUMN deleted_at,
    DROP COLUMN paused_at,
    DROP COLUMN version,
    DROP CONSTRAINT monitors_method_check;
UPDATE monitors SET method='GET' WHERE method='HEAD';
ALTER TABLE monitors ADD CONSTRAINT monitors_method_check CHECK (method = 'GET');
CREATE INDEX monitors_due_schedule_idx ON monitors (next_check_at, id);
DROP TABLE worker_pool_audit_events;
DROP TABLE worker_pool_credentials;
DROP TABLE worker_pools;
