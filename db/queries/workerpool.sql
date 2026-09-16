-- name: RegisterWorkerPool :exec
INSERT INTO worker_pools(id,mode,enabled,lifecycle_state,schema_min,schema_max,encryption_key_id,encryption_public_key,result_key_id,result_public_key,job_queue_url,job_queue_arn,job_dlq_url,job_dlq_arn)
VALUES($1,$2,false,'provisioning',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12);

-- name: InsertWorkerPoolCredential :exec
INSERT INTO worker_pool_credentials(worker_pool_id,purpose,key_id,public_material,fingerprint,status,activates_at)
VALUES($1,$2,$3,$4,$5,'pending',CURRENT_TIMESTAMP);

-- name: InsertWorkerPoolMTLSCredential :exec
INSERT INTO worker_pool_credentials(worker_pool_id,purpose,key_id,fingerprint,status,activates_at,not_after)
VALUES($1,'mtls_certificate',$2,$3,'pending',CURRENT_TIMESTAMP,$4);

-- name: LockWorkerPoolActivationReadiness :one
SELECT lifecycle_state='provisioning'
       AND job_queue_url IS NOT NULL
       AND job_queue_arn IS NOT NULL
       AND job_dlq_url IS NOT NULL
       AND job_dlq_arn IS NOT NULL
       AND encryption_public_key IS NOT NULL
       AND result_public_key IS NOT NULL
       AND (SELECT count(*)=CASE WHEN worker_pools.mode='customer_vpc' THEN 3 ELSE 2 END
            FROM worker_pool_credentials c
            WHERE c.worker_pool_id=worker_pools.id
              AND c.status='pending'
              AND (c.purpose<>'mtls_certificate' OR c.not_after>CURRENT_TIMESTAMP+INTERVAL '10 days')) AS ready
FROM worker_pools
WHERE worker_pools.id=$1
FOR UPDATE;

-- name: ActivateWorkerPool :exec
UPDATE worker_pools
SET enabled=true,lifecycle_state='active',manifest_digest=$2,updated_at=CURRENT_TIMESTAMP
WHERE id=$1;

-- name: ActivatePendingWorkerPoolCredentials :exec
UPDATE worker_pool_credentials SET status='active'
WHERE worker_pool_id=$1 AND status='pending';

-- name: LockWorkerPoolLifecycleState :one
SELECT lifecycle_state FROM worker_pools WHERE id=$1 FOR UPDATE;

-- name: UpdateWorkerPoolLifecycle :exec
UPDATE worker_pools
SET lifecycle_state=$2,enabled=false,updated_at=CURRENT_TIMESTAMP
WHERE id=$1;

-- name: RevokeWorkerPoolCredentials :exec
UPDATE worker_pool_credentials
SET status='revoked',revoked_at=CURRENT_TIMESTAMP
WHERE worker_pool_id=$1 AND status IN ('pending','active','retired');

-- name: LockWorkerPoolManifestDigest :one
SELECT manifest_digest FROM worker_pools WHERE id=$1 FOR UPDATE;

-- name: FailWorkerPool :exec
UPDATE worker_pools
SET enabled=false,lifecycle_state='failed',updated_at=CURRENT_TIMESTAMP
WHERE id=$1;

-- name: InsertWorkerPoolDriftEvent :exec
INSERT INTO monitoring_operational_events(event_type,worker_pool_id,safe_details)
VALUES('pool_drift',$1,$2);

-- name: LockWorkerPoolDeletionState :one
SELECT lifecycle_state,
       (SELECT count(*)::bigint FROM check_jobs WHERE check_jobs.worker_pool_id=$1 AND state IN ('pending','pending_publish','published','running')) AS jobs,
       (SELECT count(*)::bigint FROM worker_pool_credentials WHERE worker_pool_credentials.worker_pool_id=$1 AND status<>'revoked' AND (not_after IS NULL OR not_after>CURRENT_TIMESTAMP)) AS trusted_credentials
FROM worker_pools
WHERE worker_pools.id=$1
FOR UPDATE;

-- name: DeleteWorkerPool :exec
DELETE FROM worker_pools WHERE id=$1;

-- name: InsertWorkerPoolAudit :exec
INSERT INTO worker_pool_audit_events(worker_pool_id,action,actor,reason)
VALUES($1,$2,$3,$4);
