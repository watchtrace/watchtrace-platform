package fifo

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/incident"
	"github.com/watchtrace/watchtrace-platform/internal/quarantine"
	"github.com/watchtrace/watchtrace-platform/internal/reliability"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
	"time"
)

type ResultDelivery struct {
	Body         []byte
	Attributes   envelope.Attributes
	Receipt      string
	ReceiveCount int
}
type ResultSource interface {
	PullResult(context.Context, time.Duration) (ResultDelivery, error)
	AcknowledgeResult(context.Context, ResultDelivery) error
}
type ResultConsumer struct {
	db     DB
	source ResultSource
	now    func() time.Time
	sealer *quarantine.Sealer
}

func NewResultConsumer(db DB, source ResultSource) *ResultConsumer {
	return &ResultConsumer{db: db, source: source, now: time.Now}
}

func NewResultConsumerWithQuarantine(db DB, source ResultSource, sealer *quarantine.Sealer) *ResultConsumer {
	return &ResultConsumer{db: db, source: source, now: time.Now, sealer: sealer}
}
func (c *ResultConsumer) ConsumeNext(ctx context.Context) (bool, error) {
	ready, err := c.databaseReady(ctx)
	if err != nil || !ready {
		if err == nil {
			err = errors.New("result database unavailable")
		}
		return false, err
	}
	delivery, err := c.source.PullResult(ctx, 20*time.Second)
	if errors.Is(err, workqueue.ErrNoMessage) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	peeked, err := envelope.PeekResult(delivery.Body)
	if err != nil || peeked.JobID != delivery.Attributes.JobID || peeked.ResultID != delivery.Attributes.ResultID || peeked.WorkerPoolID != delivery.Attributes.WorkerPoolID || peeked.SnapshotHash != delivery.Attributes.SnapshotHash || peeked.ResultKeyID != delivery.Attributes.ResultKeyID || peeked.SchemaVersion != delivery.Attributes.SchemaVersion {
		return true, envelope.ErrInvalid
	}
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(context.Background())
	var poolID string
	var hash, public []byte
	var state, jobType, organizationID, environmentID, monitorID string
	var scheduled, expires time.Time
	err = tx.QueryRow(ctx, `SELECT j.worker_pool_id,j.snapshot_hash,
COALESCE((SELECT c.public_material FROM worker_pool_credentials c WHERE c.worker_pool_id=j.worker_pool_id AND c.purpose='result_signing' AND c.key_id=$2 AND c.status IN('active','retired') ORDER BY c.activates_at DESC LIMIT 1),CASE WHEN wp.result_key_id=$2 THEN wp.result_public_key END),
j.state,j.job_type,j.organization_id::text,j.environment_id::text,j.monitor_id::text,j.scheduled_at,j.expires_at FROM check_jobs j JOIN worker_pools wp ON wp.id=j.worker_pool_id WHERE j.id=$1::uuid FOR UPDATE OF j`, peeked.JobID, peeked.ResultKeyID).Scan(&poolID, &hash, &public, &state, &jobType, &organizationID, &environmentID, &monitorID, &scheduled, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, envelope.ErrInvalid
	}
	if err != nil {
		return true, err
	}
	result, err := envelope.VerifyResult(delivery.Body, ed25519.PublicKey(public))
	if err != nil || result.WorkerPoolID != poolID || result.SnapshotHash != fmt.Sprintf("%x", hash) || !result.ScheduledAt.Equal(scheduled) || result.StartedAt.Before(scheduled.Add(-5*time.Second)) || result.StartedAt.After(expires.Add(5*time.Second)) || result.CompletedAt.After(c.now().UTC().Add(5*time.Second)) {
		return true, envelope.ErrInvalid
	}
	var existingHash []byte
	var existingAttempt, existingResultID string
	existingErr := tx.QueryRow(ctx, `SELECT snapshot_hash,execution_attempt_id::text,result_id::text FROM health_checks WHERE job_id=$1::uuid`, result.JobID).Scan(&existingHash, &existingAttempt, &existingResultID)
	if existingErr == nil {
		if fmt.Sprintf("%x", existingHash) != result.SnapshotHash || existingAttempt != result.AttemptID || existingResultID != result.ResultID {
			var encrypted []byte
			if c.sealer != nil {
				encrypted, err = c.sealer.Seal(delivery.Body, []byte("result:"+result.ResultID))
				if err != nil {
					return true, err
				}
			}
			_, _ = tx.Exec(ctx, `INSERT INTO check_result_conflicts(job_id,result_id,worker_pool_id,snapshot_hash,safe_reason,encrypted_payload) VALUES($1::uuid,$2::uuid,$3,$4,'conflicting valid result',$5) ON CONFLICT(result_id) DO NOTHING`, result.JobID, result.ResultID, poolID, hash, encrypted)
			if encrypted != nil {
				_, _ = tx.Exec(ctx, `INSERT INTO monitoring_quarantine(queue_kind,job_id,result_id,worker_pool_id,snapshot_hash,safe_reason,encrypted_payload) VALUES('result',$1::uuid,$2::uuid,$3,$4,'conflicting valid result',$5)`, result.JobID, result.ResultID, poolID, hash, encrypted)
			}
			_, _ = tx.Exec(ctx, `INSERT INTO monitoring_operational_events(event_type,job_id,worker_pool_id,safe_details) VALUES('result_conflict',$1::uuid,$2,'conflicting valid result')`, result.JobID, poolID)
			if err = tx.Commit(ctx); err != nil {
				return true, err
			}
			return true, c.source.AcknowledgeResult(ctx, delivery)
		}
		_, _ = tx.Exec(ctx, `UPDATE check_dispatch_outbox SET publish_state='repaired',updated_at=CURRENT_TIMESTAMP WHERE job_id=$1::uuid AND publish_state<>'published'`, result.JobID)
		if err = tx.Commit(ctx); err != nil {
			return true, err
		}
		return true, c.source.AcknowledgeResult(ctx, delivery)
	} else if !errors.Is(existingErr, pgx.ErrNoRows) {
		return true, existingErr
	}
	_, err = tx.Exec(ctx, `INSERT INTO health_checks(job_id,result_id,organization_id,environment_id,monitor_id,job_type,scheduled_at,started_at,completed_at,succeeded,status_code,error_category,total_duration_microseconds,snapshot_hash,worker_pool_id,worker_id,execution_attempt_id,dns_duration_microseconds,connect_duration_microseconds,tls_duration_microseconds,first_byte_duration_microseconds) VALUES($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17::uuid,$18,$19,$20,$21)`, result.JobID, result.ResultID, organizationID, environmentID, monitorID, jobType, scheduled, result.StartedAt, result.CompletedAt, result.Succeeded, result.StatusCode, result.ErrorCategory, result.TotalMicros, hash, poolID, result.WorkerID, result.AttemptID, result.DNSMicros, result.ConnectMicros, result.TLSMicros, result.FirstByteMicros)
	if err != nil {
		return true, err
	}
	_, err = tx.Exec(ctx, `UPDATE check_jobs SET state='completed',started_at=$1,completed_at=$2,worker_id=$3,execution_attempt_id=$4::uuid,last_safe_error=NULL,lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL WHERE id=$5::uuid`, result.StartedAt, result.CompletedAt, result.WorkerID, result.AttemptID, result.JobID)
	if err != nil {
		return true, err
	}
	_, err = tx.Exec(ctx, `UPDATE check_dispatch_outbox SET publish_state=CASE WHEN publish_state='published' THEN publish_state ELSE 'repaired' END,updated_at=CURRENT_TIMESTAMP WHERE job_id=$1::uuid`, result.JobID)
	if err != nil {
		return true, err
	}
	_, err = tx.Exec(ctx, `DELETE FROM monitoring_coverage_gaps WHERE monitor_id=$1::uuid AND scheduled_at=$2 AND reason IN('expired','dead')`, monitorID, scheduled)
	if err != nil {
		return true, err
	}
	if jobType == "scheduled" {
		evaluatedAt := c.now().UTC()
		corrected, evaluationErr := reliability.EvaluateAcceptedTx(ctx, tx, monitorID, result.JobID, scheduled, expires, evaluatedAt)
		if evaluationErr != nil {
			return true, evaluationErr
		}
		if !evaluatedAt.After(expires.Add(10 * time.Minute)) {
			if evaluationErr = incident.ApplyEvaluationTx(ctx, tx, monitorID, result.JobID, corrected, evaluatedAt); evaluationErr != nil {
				return true, evaluationErr
			}
		}
		reason := "accepted_result"
		if corrected || evaluatedAt.After(expires.Add(10*time.Minute)) {
			reason = "late_result"
			details := "ordered state and rollup correction"
			if !corrected {
				details = "raw and rollup correction only"
			}
			if _, err = tx.Exec(ctx, `INSERT INTO monitoring_operational_events(event_type,job_id,worker_pool_id,safe_details) VALUES('late_correction',$1::uuid,$2,$3)`, result.JobID, poolID, details); err != nil {
				return true, err
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO monitor_rollup_invalidations(monitor_id,bucket_kind,bucket_start,reason)
VALUES($1::uuid,'hourly',date_trunc('hour',$2::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',$3)
ON CONFLICT(monitor_id,bucket_kind,bucket_start) DO UPDATE SET reason=EXCLUDED.reason,invalidated_at=CURRENT_TIMESTAMP`, monitorID, scheduled, reason); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO monitor_rollup_invalidations(monitor_id,bucket_kind,bucket_start,reason)
VALUES($1::uuid,'daily',date_trunc('day',$2::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',$3)
ON CONFLICT(monitor_id,bucket_kind,bucket_start) DO UPDATE SET reason=EXCLUDED.reason,invalidated_at=CURRENT_TIMESTAMP`, monitorID, scheduled, reason); err != nil {
			return true, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO api_refresh_events(organization_id,environment_id,event_type,resource_type,resource_id) VALUES($1::uuid,$2::uuid,'check.accepted','check',$3::uuid)`, organizationID, environmentID, result.JobID); err != nil {
		return true, err
	}
	if err = tx.Commit(ctx); err != nil {
		return true, err
	}
	return true, c.source.AcknowledgeResult(ctx, delivery)
}

func (c *ResultConsumer) databaseReady(ctx context.Context) (bool, error) {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	var one int
	if err = tx.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		return false, err
	}
	return one == 1, nil
}

func (c *ResultConsumer) SweepDeadlines(ctx context.Context) (int64, error) {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `WITH expired AS(UPDATE check_jobs SET state='expired',completed_at=CURRENT_TIMESTAMP,last_safe_error='start_expired' WHERE state IN('pending','pending_publish','published','running') AND expires_at<CURRENT_TIMESTAMP AND NOT EXISTS(SELECT 1 FROM health_checks h WHERE h.job_id=check_jobs.id) RETURNING organization_id,environment_id,monitor_id,scheduled_at) INSERT INTO monitoring_coverage_gaps(organization_id,environment_id,monitor_id,scheduled_at,reason) SELECT organization_id,environment_id,monitor_id,scheduled_at,'expired' FROM expired ON CONFLICT DO NOTHING`)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
func (c *ResultConsumer) ReconcileJobDLQ(ctx context.Context, jobID, poolID string) error {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `UPDATE check_jobs SET state='dead',completed_at=CURRENT_TIMESTAMP,last_safe_error='job_dlq' WHERE id=$1::uuid AND worker_pool_id=$2 AND state<>'completed'`, jobID, poolID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		_, _ = tx.Exec(ctx, `INSERT INTO monitoring_coverage_gaps(organization_id,environment_id,monitor_id,scheduled_at,reason) SELECT organization_id,environment_id,monitor_id,scheduled_at,'dead' FROM check_jobs WHERE id=$1::uuid ON CONFLICT DO NOTHING`, jobID)
		_, _ = tx.Exec(ctx, `INSERT INTO monitoring_operational_events(event_type,job_id,worker_pool_id,safe_details) VALUES('job_dlq',$1::uuid,$2,'job receive limit')`, jobID, poolID)
	}
	return tx.Commit(ctx)
}
func (c *ResultConsumer) RecordResultDLQ(ctx context.Context, jobID, poolID string) error {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	_, err = tx.Exec(ctx, `INSERT INTO monitoring_operational_events(event_type,job_id,worker_pool_id,safe_details) VALUES('result_dlq',$1::uuid,$2,'recoverable result requires redrive')`, jobID, poolID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
