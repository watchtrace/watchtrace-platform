-- name: GetQuarantineRecord :one
SELECT queue_kind,
       COALESCE(job_id::text,'')::text AS job_id,
       COALESCE(result_id::text,'')::text AS result_id,
       COALESCE(worker_pool_id,'')::text AS worker_pool_id,
       status,encrypted_payload,redrive_count,expires_at
FROM monitoring_quarantine
WHERE id=$1::uuid;

-- name: GetRecoveryResultVerification :one
SELECT COALESCE((SELECT public_material
                 FROM worker_pool_credentials
                 WHERE worker_pool_id=j.worker_pool_id
                   AND purpose='result_signing'
                   AND key_id=$2
                   AND status IN ('active','retired')
                 ORDER BY activates_at DESC
                 LIMIT 1),wp.result_public_key) AS public_key,
       j.state
FROM check_jobs j
JOIN worker_pools wp ON wp.id=j.worker_pool_id
WHERE j.id=$1::uuid AND j.worker_pool_id=$3;

-- name: MarkQuarantineRedriven :execrows
UPDATE monitoring_quarantine
SET status='redriven',redrive_count=redrive_count+1,approver=$2,redrive_reason=$3,reviewed_at=CURRENT_TIMESTAMP
WHERE id=$1::uuid AND status='quarantined' AND redrive_count<3;

-- name: InsertQueueRecoveryAudit :exec
INSERT INTO worker_pool_audit_events(worker_pool_id,action,actor,reason,safe_details)
VALUES($1,'redrive',$2,$3,jsonb_build_object('quarantine_id',$4::text,'count',1));

-- name: InsertQueueRecoveryOperationalEvent :exec
INSERT INTO monitoring_operational_events(event_type,job_id,worker_pool_id,safe_details)
VALUES('redrive',$1::uuid,$2,'controlled result redrive');
