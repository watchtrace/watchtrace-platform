-- name: CountActiveScheduledJobs :one
SELECT count(*)
FROM check_jobs
WHERE job_type = 'scheduled'
  AND state IN ('pending', 'pending_publish', 'published', 'running');

-- name: NewScheduledJobID :one
SELECT gen_random_uuid()::text AS job_id;

-- name: ListDueScheduledMonitors :many
SELECT monitors.id::text AS id,
       monitors.organization_id::text AS organization_id,
       monitors.environment_id::text AS environment_id,
       monitors.version,
       monitors.target_url,
       monitors.method,
       monitors.interval_seconds,
       monitors.timeout_seconds,
       monitors.expected_status_min,
       monitors.expected_status_max,
       monitors.next_check_at,
       monitors.headers_ciphertext,
       monitors.header_key_version,
       monitors.worker_pool_id,
       worker_pools.network_policy_version,
       worker_pools.encryption_key_id,
       worker_pools.encryption_public_key,
       worker_pools.job_queue_url,
       worker_pools.schema_min,
       worker_pools.schema_max
FROM monitors
JOIN worker_pools
  ON worker_pools.id = monitors.worker_pool_id
 AND worker_pools.enabled
 AND worker_pools.lifecycle_state = 'active'
WHERE monitors.next_check_at <= CURRENT_TIMESTAMP
  AND monitors.paused_at IS NULL
  AND monitors.deleted_at IS NULL
  AND NOT EXISTS (
      SELECT 1
      FROM check_jobs
      WHERE check_jobs.monitor_id = monitors.id
        AND check_jobs.job_type = 'scheduled'
        AND check_jobs.state IN ('pending', 'pending_publish', 'published', 'running')
  )
ORDER BY monitors.next_check_at, monitors.id
LIMIT sqlc.arg(batch_size)
FOR UPDATE OF monitors SKIP LOCKED;

-- name: InsertMonitoringCoverageGap :exec
INSERT INTO monitoring_coverage_gaps (
    organization_id, environment_id, monitor_id, scheduled_at, reason
)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    sqlc.arg(monitor_id)::text::uuid,
    sqlc.arg(scheduled_at),
    sqlc.arg(reason)
)
ON CONFLICT DO NOTHING;

-- name: InsertMonitoringOperationalEvent :exec
INSERT INTO monitoring_operational_events (
    event_type, job_id, worker_pool_id, safe_details
)
VALUES (
    sqlc.arg(event_type),
    NULLIF(sqlc.arg(job_id)::text, '')::uuid,
    NULLIF(sqlc.arg(worker_pool_id)::text, ''),
    sqlc.arg(safe_details)
);

-- name: UpdateMonitorNextCheck :exec
UPDATE monitors
SET next_check_at = sqlc.arg(next_check_at)
WHERE id = sqlc.arg(monitor_id)::text::uuid
  AND next_check_at = sqlc.arg(previous_check_at);

-- name: CreateScheduledCheckJob :execrows
INSERT INTO check_jobs (
    id, organization_id, environment_id, monitor_id, job_type, state,
    scheduled_at, monitor_version, worker_pool_id, snapshot_hash, expires_at
)
VALUES (
    sqlc.arg(job_id)::text::uuid,
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    sqlc.arg(monitor_id)::text::uuid,
    'scheduled',
    'pending_publish',
    sqlc.arg(scheduled_at),
    sqlc.arg(monitor_version),
    sqlc.arg(worker_pool_id),
    sqlc.arg(snapshot_hash),
    sqlc.arg(expires_at)
)
ON CONFLICT (monitor_id, scheduled_at) DO NOTHING;

-- name: CreateScheduledDispatchOutbox :exec
INSERT INTO check_dispatch_outbox (
    job_id, worker_pool_id, queue_url, message_body, schema_version,
    platform_key_id, worker_encryption_key_id, snapshot_hash,
    message_deduplication_id, message_group_id, expires_at
)
VALUES (
    sqlc.arg(job_id)::text::uuid,
    sqlc.arg(worker_pool_id),
    sqlc.arg(queue_url),
    sqlc.arg(message_body),
    sqlc.arg(schema_version),
    sqlc.arg(platform_key_id),
    sqlc.arg(worker_encryption_key_id),
    sqlc.arg(snapshot_hash),
    sqlc.arg(job_id),
    sqlc.arg(job_id),
    sqlc.arg(expires_at)
);

-- name: ClaimDispatchOutbox :one
WITH candidate AS (
    SELECT job_id
    FROM check_dispatch_outbox
    WHERE publish_state = 'pending'
      AND publish_attempts < 3
      AND next_attempt_at <= CURRENT_TIMESTAMP
    ORDER BY created_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
UPDATE check_dispatch_outbox AS outbox
SET publish_state = 'publishing',
    publish_attempts = publish_attempts + 1,
    publish_lease_token = gen_random_uuid(),
    publish_lease_expires_at = CURRENT_TIMESTAMP + INTERVAL '45 seconds',
    updated_at = CURRENT_TIMESTAMP
FROM candidate
WHERE outbox.job_id = candidate.job_id
RETURNING outbox.job_id::text AS job_id,
          outbox.worker_pool_id,
          outbox.queue_url,
          outbox.message_body,
          outbox.snapshot_hash,
          outbox.message_deduplication_id,
          outbox.message_group_id,
          outbox.expires_at,
          outbox.publish_attempts,
          outbox.schema_version,
          outbox.platform_key_id,
          outbox.worker_encryption_key_id,
          outbox.publish_lease_token::text AS publish_lease_token;

-- name: ExpireClaimedDispatch :exec
UPDATE check_dispatch_outbox
SET publish_state = 'expired',
    publish_lease_token = NULL,
    publish_lease_expires_at = NULL,
    updated_at = CURRENT_TIMESTAMP
WHERE job_id = sqlc.arg(job_id)::text::uuid
  AND publish_lease_token = sqlc.arg(lease_token)::text::uuid;

-- name: ExpireUnpublishedCheckJob :exec
UPDATE check_jobs
SET state = 'expired',
    completed_at = CURRENT_TIMESTAMP,
    last_safe_error = 'dispatch_expired'
WHERE id = sqlc.arg(job_id)::text::uuid
  AND state IN ('pending', 'pending_publish');

-- name: InsertExpiredCoverageGapFromJob :exec
INSERT INTO monitoring_coverage_gaps (
    organization_id, environment_id, monitor_id, scheduled_at, reason
)
SELECT organization_id, environment_id, monitor_id, scheduled_at, 'expired'
FROM check_jobs
WHERE id = sqlc.arg(job_id)::text::uuid
ON CONFLICT DO NOTHING;

-- name: MarkDispatchPublished :exec
UPDATE check_dispatch_outbox
SET publish_state = 'published',
    sqs_message_id = sqlc.arg(message_id),
    published_at = CURRENT_TIMESTAMP,
    publish_lease_token = NULL,
    publish_lease_expires_at = NULL,
    last_safe_error = NULL,
    updated_at = CURRENT_TIMESTAMP
WHERE job_id = sqlc.arg(job_id)::text::uuid
  AND publish_lease_token = sqlc.arg(lease_token)::text::uuid;

-- name: MarkCheckJobPublished :exec
UPDATE check_jobs
SET state = 'published', sqs_message_id = sqlc.arg(message_id)
WHERE id = sqlc.arg(job_id)::text::uuid
  AND state IN ('pending', 'pending_publish');

-- name: MarkDispatchPublishFailure :exec
UPDATE check_dispatch_outbox
SET publish_state = sqlc.arg(publish_state),
    next_attempt_at = CURRENT_TIMESTAMP
      + sqlc.arg(delay_seconds)::integer * INTERVAL '1 second',
    publish_lease_token = NULL,
    publish_lease_expires_at = NULL,
    last_safe_error = 'sqs_send_failed',
    updated_at = CURRENT_TIMESTAMP
WHERE job_id = sqlc.arg(job_id)::text::uuid
  AND publish_lease_token = sqlc.arg(lease_token)::text::uuid;

-- name: LockResultCheckJob :one
SELECT check_jobs.worker_pool_id,
       check_jobs.snapshot_hash,
       COALESCE(
           (
               SELECT worker_pool_credentials.public_material
               FROM worker_pool_credentials
               WHERE worker_pool_credentials.worker_pool_id = check_jobs.worker_pool_id
                 AND worker_pool_credentials.purpose = 'result_signing'
                 AND worker_pool_credentials.key_id = sqlc.arg(result_key_id)
                 AND worker_pool_credentials.status IN ('active', 'retired')
               ORDER BY worker_pool_credentials.activates_at DESC
               LIMIT 1
           ),
           CASE
             WHEN worker_pools.result_key_id = sqlc.arg(result_key_id)
             THEN worker_pools.result_public_key
           END
       )::bytea AS result_public_key,
       check_jobs.state,
       check_jobs.job_type,
       check_jobs.organization_id::text AS organization_id,
       check_jobs.environment_id::text AS environment_id,
       check_jobs.monitor_id::text AS monitor_id,
       check_jobs.scheduled_at,
       check_jobs.expires_at
FROM check_jobs
JOIN worker_pools ON worker_pools.id = check_jobs.worker_pool_id
WHERE check_jobs.id = sqlc.arg(job_id)::text::uuid
FOR UPDATE OF check_jobs;

-- name: GetAcceptedHealthCheck :one
SELECT snapshot_hash, execution_attempt_id::text AS execution_attempt_id,
       result_id::text AS result_id
FROM health_checks
WHERE job_id = sqlc.arg(job_id)::text::uuid;

-- name: InsertResultConflict :exec
INSERT INTO check_result_conflicts (
    job_id, result_id, worker_pool_id, snapshot_hash, safe_reason, encrypted_payload
)
VALUES (
    sqlc.arg(job_id)::text::uuid,
    sqlc.arg(result_id)::text::uuid,
    sqlc.arg(worker_pool_id),
    sqlc.arg(snapshot_hash),
    'conflicting valid result',
    sqlc.arg(encrypted_payload)
)
ON CONFLICT (result_id) DO NOTHING;

-- name: InsertConflictingResultQuarantine :exec
INSERT INTO monitoring_quarantine (
    queue_kind, job_id, result_id, worker_pool_id, snapshot_hash,
    safe_reason, encrypted_payload
)
VALUES (
    'result',
    sqlc.arg(job_id)::text::uuid,
    sqlc.arg(result_id)::text::uuid,
    sqlc.arg(worker_pool_id),
    sqlc.arg(snapshot_hash),
    'conflicting valid result',
    sqlc.arg(encrypted_payload)
);

-- name: MarkDispatchRepaired :exec
UPDATE check_dispatch_outbox
SET publish_state = 'repaired', updated_at = CURRENT_TIMESTAMP
WHERE job_id = sqlc.arg(job_id)::text::uuid
  AND publish_state <> 'published';

-- name: InsertAcceptedHealthCheck :exec
INSERT INTO health_checks (
    job_id, result_id, organization_id, environment_id, monitor_id, job_type,
    scheduled_at, started_at, completed_at, succeeded, status_code,
    error_category, total_duration_microseconds, snapshot_hash, worker_pool_id,
    worker_id, execution_attempt_id, dns_duration_microseconds,
    connect_duration_microseconds, tls_duration_microseconds,
    first_byte_duration_microseconds
)
VALUES (
    sqlc.arg(job_id)::text::uuid,
    sqlc.arg(result_id)::text::uuid,
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    sqlc.arg(monitor_id)::text::uuid,
    sqlc.arg(job_type),
    sqlc.arg(scheduled_at),
    sqlc.arg(started_at),
    sqlc.arg(completed_at),
    sqlc.arg(succeeded),
    sqlc.narg(status_code),
    sqlc.narg(error_category),
    sqlc.arg(total_duration_microseconds),
    sqlc.arg(snapshot_hash),
    sqlc.arg(worker_pool_id),
    sqlc.arg(worker_id),
    sqlc.arg(execution_attempt_id)::text::uuid,
    sqlc.narg(dns_duration_microseconds),
    sqlc.narg(connect_duration_microseconds),
    sqlc.narg(tls_duration_microseconds),
    sqlc.narg(first_byte_duration_microseconds)
);

-- name: CompleteCheckJob :exec
UPDATE check_jobs
SET state = 'completed',
    started_at = sqlc.arg(started_at),
    completed_at = sqlc.arg(completed_at),
    worker_id = sqlc.arg(worker_id),
    execution_attempt_id = sqlc.arg(execution_attempt_id)::text::uuid,
    last_safe_error = NULL
WHERE id = sqlc.arg(job_id)::text::uuid;

-- name: DeleteRecoveredCoverageGaps :exec
DELETE FROM monitoring_coverage_gaps
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND scheduled_at = sqlc.arg(scheduled_at)
  AND reason IN ('expired', 'dead');

-- name: UpsertHourlyRollupInvalidation :exec
INSERT INTO monitor_rollup_invalidations (
    monitor_id, bucket_kind, bucket_start, reason
)
VALUES (
    sqlc.arg(monitor_id)::text::uuid,
    'hourly',
    date_trunc('hour', sqlc.arg(scheduled_at)::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
    sqlc.arg(reason)
)
ON CONFLICT (monitor_id, bucket_kind, bucket_start)
DO UPDATE SET reason = EXCLUDED.reason, invalidated_at = CURRENT_TIMESTAMP;

-- name: UpsertDailyRollupInvalidation :exec
INSERT INTO monitor_rollup_invalidations (
    monitor_id, bucket_kind, bucket_start, reason
)
VALUES (
    sqlc.arg(monitor_id)::text::uuid,
    'daily',
    date_trunc('day', sqlc.arg(scheduled_at)::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
    sqlc.arg(reason)
)
ON CONFLICT (monitor_id, bucket_kind, bucket_start)
DO UPDATE SET reason = EXCLUDED.reason, invalidated_at = CURRENT_TIMESTAMP;

-- name: InsertAcceptedCheckRefreshEvent :exec
INSERT INTO api_refresh_events (
    organization_id, environment_id, event_type, resource_type, resource_id
)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    'check.accepted',
    'check',
    sqlc.arg(job_id)::text::uuid
);

-- name: DatabasePing :one
SELECT 1::integer AS ready;

-- name: InsertInvalidResultQuarantine :exec
INSERT INTO monitoring_quarantine (queue_kind, safe_reason, encrypted_payload)
VALUES ('result', 'invalid result DLQ envelope', sqlc.arg(encrypted_payload));

-- name: InsertResultDLQQuarantine :exec
INSERT INTO monitoring_quarantine (
    queue_kind, job_id, result_id, worker_pool_id, snapshot_hash,
    safe_reason, encrypted_payload
)
VALUES (
    'result',
    sqlc.arg(job_id)::text::uuid,
    sqlc.arg(result_id)::text::uuid,
    sqlc.arg(worker_pool_id),
    decode(sqlc.arg(snapshot_hash), 'hex'),
    'result DLQ recovery',
    sqlc.arg(encrypted_payload)
)
ON CONFLICT DO NOTHING;

-- name: MarkCheckJobDead :execrows
UPDATE check_jobs
SET state = 'dead', completed_at = CURRENT_TIMESTAMP, last_safe_error = 'job_dlq'
WHERE id = sqlc.arg(job_id)::text::uuid
  AND worker_pool_id = sqlc.arg(worker_pool_id)
  AND state <> 'completed';

-- name: InsertDeadCoverageGapFromJob :exec
INSERT INTO monitoring_coverage_gaps (
    organization_id, environment_id, monitor_id, scheduled_at, reason
)
SELECT organization_id, environment_id, monitor_id, scheduled_at, 'dead'
FROM check_jobs
WHERE id = sqlc.arg(job_id)::text::uuid
ON CONFLICT DO NOTHING;

-- name: ReclaimDispatchPublisherLeases :execrows
UPDATE check_dispatch_outbox
SET publish_state = CASE
      WHEN publish_attempts >= 3 THEN 'ambiguous'
      ELSE 'pending'
    END,
    publish_lease_token = NULL,
    publish_lease_expires_at = NULL,
    last_safe_error = 'publisher_interrupted',
    updated_at = CURRENT_TIMESTAMP
WHERE publish_state = 'publishing'
  AND publish_lease_expires_at < CURRENT_TIMESTAMP;

-- name: DeleteOldLedgerJobs :execrows
DELETE FROM check_jobs
WHERE (state = 'completed' AND completed_at < sqlc.arg(completed_before))
   OR (state IN ('dead', 'expired', 'cancelled', 'quarantined')
       AND completed_at < sqlc.arg(nonterminal_before));

-- name: GetCheckJobStateCounts :one
SELECT count(*) FILTER (WHERE state IN ('pending', 'pending_publish')) AS pending,
       count(*) FILTER (WHERE state = 'published') AS published,
       count(*) FILTER (WHERE state = 'running') AS running,
       count(*) FILTER (WHERE state = 'dead') AS dead,
       count(*) FILTER (WHERE state = 'expired') AS expired
FROM check_jobs;

-- name: GetOldestActiveCheckJob :one
SELECT created_at
FROM check_jobs
WHERE state IN ('pending', 'pending_publish', 'published', 'running')
ORDER BY created_at
LIMIT 1;

-- name: GetDispatchStateCounts :one
SELECT count(*) FILTER (WHERE publish_state IN ('pending', 'publishing')) AS pending,
       count(*) FILTER (WHERE publish_state = 'ambiguous') AS ambiguous
FROM check_dispatch_outbox;

-- name: GetOldestNonterminalDispatch :one
SELECT created_at
FROM check_dispatch_outbox
WHERE publish_state IN ('pending', 'publishing', 'ambiguous')
ORDER BY created_at
LIMIT 1;

-- name: SweepExpiredCheckJobs :execrows
WITH expired AS (
    UPDATE check_jobs
    SET state = 'expired',
        completed_at = CURRENT_TIMESTAMP,
        last_safe_error = 'start_expired'
    WHERE state IN ('pending', 'pending_publish', 'published', 'running')
      AND expires_at < CURRENT_TIMESTAMP
      AND NOT EXISTS (
          SELECT 1 FROM health_checks WHERE health_checks.job_id = check_jobs.id
      )
    RETURNING organization_id, environment_id, monitor_id, scheduled_at
)
INSERT INTO monitoring_coverage_gaps (
    organization_id, environment_id, monitor_id, scheduled_at, reason
)
SELECT organization_id, environment_id, monitor_id, scheduled_at, 'expired'
FROM expired
ON CONFLICT DO NOTHING;
