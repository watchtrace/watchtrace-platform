CREATE TABLE worker_pools (
    id text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),
    mode text NOT NULL CHECK (mode IN ('hosted', 'customer_vpc')),
    enabled boolean NOT NULL DEFAULT false,
    lifecycle_state text NOT NULL DEFAULT 'provisioning'
      CHECK (lifecycle_state IN ('provisioning','active','draining','revoked','deleting','failed')),
    worker_version text,
    schema_min smallint NOT NULL DEFAULT 1 CHECK (schema_min IN (1,2)),
    schema_max smallint NOT NULL DEFAULT 2 CHECK (schema_max IN (1,2) AND schema_max >= schema_min),
    capabilities jsonb NOT NULL DEFAULT '["get","head"]'::jsonb CHECK (jsonb_typeof(capabilities) = 'array'),
    network_policy_version integer NOT NULL DEFAULT 1 CHECK (network_policy_version > 0),
    allowed_cidrs cidr[] NOT NULL DEFAULT '{}',
    encryption_key_id text,
    encryption_public_key bytea,
    result_key_id text,
    result_public_key bytea,
    job_queue_url text,
    job_queue_arn text,
    job_dlq_url text,
    job_dlq_arn text,
    manifest_digest bytea CHECK (manifest_digest IS NULL OR octet_length(manifest_digest) = 32),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (mode = 'customer_vpc' OR cardinality(allowed_cidrs) = 0),
    CHECK ((encryption_key_id IS NULL) = (encryption_public_key IS NULL)),
    CHECK ((result_key_id IS NULL) = (result_public_key IS NULL)),
    CHECK (encryption_public_key IS NULL OR octet_length(encryption_public_key) = 32),
    CHECK (result_public_key IS NULL OR octet_length(result_public_key) = 32)
);

INSERT INTO worker_pools (id, mode, enabled, lifecycle_state) VALUES ('hosted', 'hosted', true, 'active');

CREATE TABLE worker_pool_credentials (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    worker_pool_id text NOT NULL REFERENCES worker_pools (id) ON DELETE CASCADE,
    purpose text NOT NULL CHECK (purpose IN ('job_encryption','result_signing','mtls_certificate')),
    key_id text NOT NULL CHECK (octet_length(key_id) BETWEEN 1 AND 64),
    public_material bytea,
    fingerprint text NOT NULL CHECK (octet_length(fingerprint) BETWEEN 1 AND 128),
    status text NOT NULL CHECK (status IN ('pending','active','retired','revoked')),
    activates_at timestamptz NOT NULL,
    retires_at timestamptz,
    revoked_at timestamptz,
    not_after timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (worker_pool_id, purpose, key_id),
    CHECK (status <> 'revoked' OR revoked_at IS NOT NULL),
    CHECK (retires_at IS NULL OR retires_at >= activates_at)
);

CREATE TABLE worker_pool_audit_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    worker_pool_id text NOT NULL,
    action text NOT NULL CHECK (action IN ('register','activate','drain','revoke','delete_start','delete_complete','fail','reconcile','rotate','redrive')),
    actor text NOT NULL CHECK (octet_length(actor) BETWEEN 1 AND 128),
    reason text NOT NULL CHECK (octet_length(reason) BETWEEN 1 AND 240),
    safe_details jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(safe_details) = 'object'),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE monitors DROP CONSTRAINT monitors_method_check;
ALTER TABLE monitors
    ADD CONSTRAINT monitors_method_check CHECK (method IN ('GET', 'HEAD')),
    ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    ADD COLUMN paused_at timestamptz,
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN headers_ciphertext bytea,
    ADD COLUMN header_key_version integer,
    ADD COLUMN worker_pool_id text NOT NULL DEFAULT 'hosted' REFERENCES worker_pools (id),
    ADD CONSTRAINT monitors_headers_encryption_check CHECK (
      (headers_ciphertext IS NULL AND header_key_version IS NULL) OR
      (headers_ciphertext IS NOT NULL AND octet_length(headers_ciphertext) <= 16384
       AND header_key_version IS NOT NULL AND header_key_version > 0));

DROP INDEX monitors_due_schedule_idx;
CREATE INDEX monitors_due_schedule_idx ON monitors (next_check_at, id)
    WHERE paused_at IS NULL AND deleted_at IS NULL;

ALTER TABLE check_jobs DROP CONSTRAINT check_jobs_state_check;
ALTER TABLE check_jobs DROP CONSTRAINT check_jobs_lease_consistency_check;
ALTER TABLE check_jobs
    ADD CONSTRAINT check_jobs_state_check CHECK (state IN
      ('pending','pending_publish','published','running','completed','cancelled','expired','dead','quarantined')),
    ADD COLUMN monitor_version bigint NOT NULL DEFAULT 1 CHECK (monitor_version > 0),
    ADD COLUMN worker_pool_id text NOT NULL DEFAULT 'hosted' REFERENCES worker_pools (id),
    ADD COLUMN snapshot_hash bytea CHECK (snapshot_hash IS NULL OR octet_length(snapshot_hash) = 32),
    ADD COLUMN expires_at timestamptz,
    ADD COLUMN processing_token uuid,
    ADD COLUMN worker_id text CHECK (worker_id IS NULL OR octet_length(worker_id) <= 128),
    ADD COLUMN execution_attempt_id uuid,
    ADD COLUMN receive_count integer NOT NULL DEFAULT 0 CHECK (receive_count >= 0),
    ADD COLUMN sqs_message_id text CHECK (sqs_message_id IS NULL OR octet_length(sqs_message_id) <= 128),
    ADD COLUMN last_safe_error text CHECK (last_safe_error IS NULL OR octet_length(last_safe_error) <= 120);

DROP INDEX check_jobs_one_outstanding_scheduled_idx;
CREATE UNIQUE INDEX check_jobs_one_outstanding_scheduled_idx ON check_jobs (monitor_id)
    WHERE job_type = 'scheduled' AND state IN ('pending','pending_publish','published','running');
CREATE INDEX check_jobs_deadline_idx ON check_jobs (expires_at, id)
    WHERE state IN ('pending','pending_publish','published','running');
CREATE INDEX check_jobs_terminal_cleanup_idx ON check_jobs (completed_at, id)
    WHERE state IN ('completed','cancelled','expired','dead','quarantined');

CREATE TABLE check_dispatch_outbox (
    job_id uuid PRIMARY KEY REFERENCES check_jobs (id) ON DELETE CASCADE,
    worker_pool_id text NOT NULL REFERENCES worker_pools (id),
    queue_url text NOT NULL CHECK (octet_length(queue_url) <= 2048),
    message_body bytea NOT NULL CHECK (octet_length(message_body) BETWEEN 1 AND 65536),
    schema_version smallint NOT NULL DEFAULT 2 CHECK (schema_version IN (1,2)),
    platform_key_id text NOT NULL CHECK (octet_length(platform_key_id) BETWEEN 1 AND 64),
    worker_encryption_key_id text NOT NULL CHECK (octet_length(worker_encryption_key_id) BETWEEN 1 AND 64),
    snapshot_hash bytea NOT NULL CHECK (octet_length(snapshot_hash) = 32),
    message_deduplication_id text NOT NULL CHECK (octet_length(message_deduplication_id) <= 128),
    message_group_id text NOT NULL CHECK (octet_length(message_group_id) <= 128),
    expires_at timestamptz NOT NULL,
    publish_state text NOT NULL DEFAULT 'pending'
      CHECK (publish_state IN ('pending','publishing','published','ambiguous','expired','repaired')),
    publish_attempts smallint NOT NULL DEFAULT 0 CHECK (publish_attempts BETWEEN 0 AND 3),
    next_attempt_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    publish_lease_token uuid,
    publish_lease_expires_at timestamptz,
    sqs_message_id text CHECK (sqs_message_id IS NULL OR octet_length(sqs_message_id) <= 128),
    last_safe_error text CHECK (last_safe_error IS NULL OR octet_length(last_safe_error) <= 120),
    published_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (message_deduplication_id = job_id::text),
    CHECK (message_group_id = job_id::text)
);
CREATE INDEX check_dispatch_ready_idx ON check_dispatch_outbox (next_attempt_at, created_at, job_id)
    WHERE publish_state IN ('pending','ambiguous');

CREATE FUNCTION reject_dispatch_payload_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.worker_pool_id IS DISTINCT FROM OLD.worker_pool_id
     OR NEW.queue_url IS DISTINCT FROM OLD.queue_url
     OR NEW.message_body IS DISTINCT FROM OLD.message_body
     OR NEW.schema_version IS DISTINCT FROM OLD.schema_version
     OR NEW.platform_key_id IS DISTINCT FROM OLD.platform_key_id
     OR NEW.worker_encryption_key_id IS DISTINCT FROM OLD.worker_encryption_key_id
     OR NEW.snapshot_hash IS DISTINCT FROM OLD.snapshot_hash
     OR NEW.message_deduplication_id IS DISTINCT FROM OLD.message_deduplication_id
     OR NEW.message_group_id IS DISTINCT FROM OLD.message_group_id
     OR NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
    RAISE EXCEPTION 'dispatch payload is immutable' USING ERRCODE = '23000';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER check_dispatch_payload_immutable
BEFORE UPDATE ON check_dispatch_outbox
FOR EACH ROW EXECUTE FUNCTION reject_dispatch_payload_mutation();

ALTER TABLE health_checks
    ADD COLUMN result_id uuid,
    ADD COLUMN snapshot_hash bytea CHECK (snapshot_hash IS NULL OR octet_length(snapshot_hash) = 32),
    ADD COLUMN worker_pool_id text REFERENCES worker_pools (id),
    ADD COLUMN worker_id text CHECK (worker_id IS NULL OR octet_length(worker_id) <= 128),
    ADD COLUMN execution_attempt_id uuid,
    ADD COLUMN dns_duration_microseconds bigint CHECK (dns_duration_microseconds >= 0),
    ADD COLUMN connect_duration_microseconds bigint CHECK (connect_duration_microseconds >= 0),
    ADD COLUMN tls_duration_microseconds bigint CHECK (tls_duration_microseconds >= 0),
    ADD COLUMN first_byte_duration_microseconds bigint CHECK (first_byte_duration_microseconds >= 0);
CREATE UNIQUE INDEX health_checks_result_id_unique_idx ON health_checks (result_id) WHERE result_id IS NOT NULL;
ALTER TABLE health_checks DROP CONSTRAINT health_checks_error_category_check;
ALTER TABLE health_checks ADD CONSTRAINT health_checks_error_category_check CHECK (error_category IN (
  'invalid_target','unsafe_target','dns','connection','tls','timeout','http_protocol',
  'response_body','response_too_large','redirect_limit','unexpected_status'));

CREATE TABLE check_result_conflicts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id uuid NOT NULL REFERENCES check_jobs (id),
    result_id uuid NOT NULL,
    worker_pool_id text NOT NULL,
    snapshot_hash bytea NOT NULL CHECK (octet_length(snapshot_hash) = 32),
    safe_reason text NOT NULL CHECK (octet_length(safe_reason) <= 120),
    encrypted_payload bytea CHECK (encrypted_payload IS NULL OR octet_length(encrypted_payload) <= 65536 + 64),
    received_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX check_result_conflicts_result_id_idx ON check_result_conflicts (result_id);

CREATE TABLE monitor_evaluation_positions (
    monitor_id uuid PRIMARY KEY REFERENCES monitors (id) ON DELETE CASCADE,
    last_scheduled_at timestamptz,
    last_job_id uuid,
    invalidated_from timestamptz,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE monitor_schedule_periods (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    monitor_id uuid NOT NULL,
    monitor_version bigint NOT NULL CHECK (monitor_version > 0),
    interval_seconds integer NOT NULL CHECK (interval_seconds IN (60,120,300,600,1800)),
    worker_pool_id text NOT NULL REFERENCES worker_pools (id),
    starts_at timestamptz NOT NULL,
    first_slot_at timestamptz NOT NULL,
    ends_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (first_slot_at >= starts_at),
    CHECK (ends_at IS NULL OR ends_at >= starts_at),
    FOREIGN KEY (organization_id, environment_id, monitor_id)
      REFERENCES monitors (organization_id, environment_id, id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX monitor_schedule_periods_one_open_idx
    ON monitor_schedule_periods (monitor_id) WHERE ends_at IS NULL;
CREATE INDEX monitor_schedule_periods_report_idx
    ON monitor_schedule_periods (monitor_id, starts_at, ends_at);
INSERT INTO monitor_schedule_periods(organization_id,environment_id,monitor_id,monitor_version,interval_seconds,worker_pool_id,starts_at,first_slot_at)
SELECT organization_id,environment_id,id,version,interval_seconds,worker_pool_id,created_at,GREATEST(created_at,next_check_at)
FROM monitors WHERE paused_at IS NULL AND deleted_at IS NULL;

CREATE TABLE monitoring_quarantine (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    queue_kind text NOT NULL CHECK (queue_kind IN ('job','result')),
    job_id uuid,
    result_id uuid,
    worker_pool_id text,
    snapshot_hash bytea CHECK (snapshot_hash IS NULL OR octet_length(snapshot_hash) = 32),
    safe_reason text NOT NULL CHECK (octet_length(safe_reason) BETWEEN 1 AND 120),
    encrypted_payload bytea NOT NULL CHECK (octet_length(encrypted_payload) BETWEEN 1 AND 65600),
    redrive_count smallint NOT NULL DEFAULT 0 CHECK (redrive_count BETWEEN 0 AND 3),
    status text NOT NULL DEFAULT 'quarantined' CHECK (status IN ('quarantined','redriven','discarded')),
    approver text,
    redrive_reason text,
    quarantined_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    reviewed_at timestamptz,
    expires_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP + INTERVAL '14 days'
);
CREATE INDEX monitoring_quarantine_expiry_idx ON monitoring_quarantine (expires_at, id) WHERE status='quarantined';
CREATE UNIQUE INDEX monitoring_quarantine_result_id_idx ON monitoring_quarantine (result_id) WHERE result_id IS NOT NULL;

CREATE TABLE monitoring_coverage_gaps (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    monitor_id uuid NOT NULL,
    scheduled_at timestamptz NOT NULL,
    reason text NOT NULL CHECK (reason IN ('admission_limit','expired','dead','missed')),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (monitor_id, scheduled_at, reason),
    FOREIGN KEY (organization_id, environment_id, monitor_id)
      REFERENCES monitors (organization_id, environment_id, id) ON DELETE CASCADE
);
CREATE INDEX monitoring_coverage_gaps_report_idx
    ON monitoring_coverage_gaps (organization_id, environment_id, monitor_id, scheduled_at);

CREATE TABLE monitor_rollups_hourly (
    organization_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    monitor_id uuid NOT NULL,
    bucket_start timestamptz NOT NULL,
    expected_checks integer NOT NULL CHECK (expected_checks >= 0),
    observed_checks integer NOT NULL CHECK (observed_checks >= 0),
    successful_checks integer NOT NULL CHECK (successful_checks >= 0),
    unknown_checks integer NOT NULL CHECK (unknown_checks >= 0),
    total_duration_microseconds bigint NOT NULL CHECK (total_duration_microseconds >= 0),
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (monitor_id, bucket_start),
    FOREIGN KEY (organization_id, environment_id, monitor_id)
      REFERENCES monitors (organization_id, environment_id, id) ON DELETE CASCADE
);
CREATE INDEX monitor_rollups_hourly_retention_idx ON monitor_rollups_hourly (bucket_start);

CREATE TABLE monitor_rollups_daily (
    organization_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    monitor_id uuid NOT NULL,
    bucket_start date NOT NULL,
    expected_checks integer NOT NULL CHECK (expected_checks >= 0),
    observed_checks integer NOT NULL CHECK (observed_checks >= 0),
    successful_checks integer NOT NULL CHECK (successful_checks >= 0),
    unknown_checks integer NOT NULL CHECK (unknown_checks >= 0),
    total_duration_microseconds bigint NOT NULL CHECK (total_duration_microseconds >= 0),
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (monitor_id, bucket_start),
    FOREIGN KEY (organization_id, environment_id, monitor_id)
      REFERENCES monitors (organization_id, environment_id, id) ON DELETE CASCADE
);
CREATE INDEX monitor_rollups_daily_retention_idx ON monitor_rollups_daily (bucket_start);

CREATE TABLE monitoring_operational_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type text NOT NULL CHECK (event_type IN
      ('admission_limit','dispatch_ambiguous','job_dlq','result_dlq','result_conflict','clock_skew','dependency_open','pool_drift','quarantine','redrive','late_correction')),
    job_id uuid,
    worker_pool_id text,
    safe_details text NOT NULL CHECK (octet_length(safe_details) <= 120),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX monitoring_operational_events_created_idx
    ON monitoring_operational_events (created_at DESC);
