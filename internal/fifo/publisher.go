package fifo

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"time"
)

type SendInput struct {
	QueueURL                 string
	Body                     []byte
	DeduplicationID, GroupID string
	Attributes               envelope.Attributes
}
type Sender interface {
	Send(context.Context, SendInput) (string, error)
}
type Publisher struct {
	db     DB
	sender Sender
	now    func() time.Time
}

func NewPublisher(db DB, s Sender) *Publisher { return &Publisher{db: db, sender: s, now: time.Now} }

type outbox struct {
	JobID, Pool, Queue, Dedup, Group string
	PlatformKeyID, WorkerKeyID       string
	Body, Hash                       []byte
	Expiry                           time.Time
	Attempts                         int16
	Schema                           int16
	Token                            string
}

func (p *Publisher) PublishNext(ctx context.Context) (bool, error) {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	var row outbox
	err = tx.QueryRow(ctx, `WITH candidate AS(SELECT job_id FROM check_dispatch_outbox WHERE publish_state='pending' AND publish_attempts<3 AND next_attempt_at<=CURRENT_TIMESTAMP ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED) UPDATE check_dispatch_outbox o SET publish_state='publishing',publish_attempts=publish_attempts+1,publish_lease_token=gen_random_uuid(),publish_lease_expires_at=CURRENT_TIMESTAMP+INTERVAL '45 seconds',updated_at=CURRENT_TIMESTAMP FROM candidate WHERE o.job_id=candidate.job_id RETURNING o.job_id::text,o.worker_pool_id,o.queue_url,o.message_body,o.snapshot_hash,o.message_deduplication_id,o.message_group_id,o.expires_at,o.publish_attempts,o.schema_version,o.platform_key_id,o.worker_encryption_key_id,o.publish_lease_token::text`).Scan(&row.JobID, &row.Pool, &row.Queue, &row.Body, &row.Hash, &row.Dedup, &row.Group, &row.Expiry, &row.Attempts, &row.Schema, &row.PlatformKeyID, &row.WorkerKeyID, &row.Token)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if p.now().UTC().After(row.Expiry) {
		_, err = tx.Exec(ctx, `UPDATE check_dispatch_outbox SET publish_state='expired',publish_lease_token=NULL,publish_lease_expires_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE job_id=$1::uuid AND publish_lease_token=$2::uuid`, row.JobID, row.Token)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE check_jobs SET state='expired',completed_at=CURRENT_TIMESTAMP,last_safe_error='dispatch_expired' WHERE id=$1::uuid AND state IN('pending','pending_publish')`, row.JobID)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO monitoring_coverage_gaps(organization_id,environment_id,monitor_id,scheduled_at,reason) SELECT organization_id,environment_id,monitor_id,scheduled_at,'expired' FROM check_jobs WHERE id=$1::uuid ON CONFLICT DO NOTHING`, row.JobID)
		}
		if err != nil {
			return true, err
		}
		return true, tx.Commit(ctx)
	}
	if err = tx.Commit(ctx); err != nil {
		return true, err
	}
	attrs := envelope.Attributes{SchemaVersion: int(row.Schema), JobID: row.JobID, WorkerPoolID: row.Pool, SnapshotHash: fmtHash(row.Hash), ExpiresAt: row.Expiry, PlatformKeyID: row.PlatformKeyID, WorkerEncryptionKeyID: row.WorkerKeyID}
	messageID, sendErr := p.sender.Send(ctx, SendInput{QueueURL: row.Queue, Body: row.Body, DeduplicationID: row.Dedup, GroupID: row.Group, Attributes: attrs})
	tx, err = p.db.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(context.Background())
	if sendErr == nil {
		_, err = tx.Exec(ctx, `UPDATE check_dispatch_outbox SET publish_state='published',sqs_message_id=$1,published_at=CURRENT_TIMESTAMP,publish_lease_token=NULL,publish_lease_expires_at=NULL,last_safe_error=NULL,updated_at=CURRENT_TIMESTAMP WHERE job_id=$2::uuid AND publish_lease_token=$3::uuid`, messageID, row.JobID, row.Token)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE check_jobs SET state='published',sqs_message_id=$1 WHERE id=$2::uuid AND state IN('pending','pending_publish')`, messageID, row.JobID)
		}
	} else {
		state := "pending"
		delay := 5
		if row.Attempts == 2 {
			delay = 10
		}
		if row.Attempts >= 3 {
			state = "ambiguous"
			delay = 120
		}
		_, err = tx.Exec(ctx, `UPDATE check_dispatch_outbox SET publish_state=$1,next_attempt_at=CURRENT_TIMESTAMP+$2::int*INTERVAL '1 second',publish_lease_token=NULL,publish_lease_expires_at=NULL,last_safe_error='sqs_send_failed',updated_at=CURRENT_TIMESTAMP WHERE job_id=$3::uuid AND publish_lease_token=$4::uuid`, state, delay, row.JobID, row.Token)
	}
	if err != nil {
		return true, err
	}
	if err = tx.Commit(ctx); err != nil {
		return true, err
	}
	return true, sendErr
}
func fmtHash(b []byte) string { return fmt.Sprintf("%x", b) }
