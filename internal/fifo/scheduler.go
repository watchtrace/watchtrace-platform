// Package fifo implements the PostgreSQL/SQS FIFO control-plane path.
package fifo

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
)

const (
	MaxBatch             = 100
	GlobalScheduledLimit = 1000
	JobStartWindow       = 2 * time.Minute
)

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
}
type Scheduler struct {
	db            DB
	signer        ed25519.PrivateKey
	platformKeyID string
	headers       *secureheaders.Keyring
}

func NewScheduler(db DB, signer ed25519.PrivateKey, keyID string, headers *secureheaders.Keyring) (*Scheduler, error) {
	if db == nil || len(signer) != ed25519.PrivateKeySize || keyID == "" || headers == nil {
		return nil, errors.New("invalid FIFO scheduler configuration")
	}
	return &Scheduler{db: db, signer: signer, platformKeyID: keyID, headers: headers}, nil
}

type dueMonitor struct {
	ID, OrganizationID, EnvironmentID, TargetURL, Method, WorkerPoolID, EncryptionKeyID, QueueURL string
	Version                                                                                       int64
	Interval, Timeout                                                                             int32
	Min, Max                                                                                      int16
	Next, Now                                                                                     time.Time
	Headers                                                                                       []byte
	HeaderVersion                                                                                 pgtype.Int4
	WorkerPublic                                                                                  []byte
	NetworkPolicy                                                                                 int
	SchemaMin, SchemaMax                                                                          int
}

func (s *Scheduler) ScheduleDue(ctx context.Context, batch int) (int, error) {
	if batch < 1 || batch > MaxBatch {
		return 0, errors.New("invalid scheduler batch")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	var outstanding int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM check_jobs WHERE job_type='scheduled' AND state IN('pending','pending_publish','published','running')`).Scan(&outstanding); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT m.id::text,m.organization_id::text,m.environment_id::text,m.version,m.target_url,m.method,m.interval_seconds,m.timeout_seconds,m.expected_status_min,m.expected_status_max,m.next_check_at,CURRENT_TIMESTAMP,m.headers_ciphertext,m.header_key_version,m.worker_pool_id,wp.network_policy_version,wp.encryption_key_id,wp.encryption_public_key,wp.job_queue_url,wp.schema_min,wp.schema_max FROM monitors m JOIN worker_pools wp ON wp.id=m.worker_pool_id AND wp.enabled AND wp.lifecycle_state='active' WHERE m.next_check_at<=CURRENT_TIMESTAMP AND m.paused_at IS NULL AND m.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM check_jobs j WHERE j.monitor_id=m.id AND j.job_type='scheduled' AND j.state IN('pending','pending_publish','published','running')) ORDER BY m.next_check_at,m.id LIMIT $1 FOR UPDATE OF m SKIP LOCKED`, batch)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	due := []dueMonitor{}
	for rows.Next() {
		var m dueMonitor
		if err = rows.Scan(&m.ID, &m.OrganizationID, &m.EnvironmentID, &m.Version, &m.TargetURL, &m.Method, &m.Interval, &m.Timeout, &m.Min, &m.Max, &m.Next, &m.Now, &m.Headers, &m.HeaderVersion, &m.WorkerPoolID, &m.NetworkPolicy, &m.EncryptionKeyID, &m.WorkerPublic, &m.QueueURL, &m.SchemaMin, &m.SchemaMax); err != nil {
			return 0, err
		}
		due = append(due, m)
	}
	if rows.Err() != nil {
		return 0, rows.Err()
	}
	created := 0
	for _, m := range due {
		next, err := nextSchedule(m.Next, m.Now, m.Interval)
		if err != nil {
			return 0, err
		}
		if outstanding >= GlobalScheduledLimit {
			if _, err = tx.Exec(ctx, `INSERT INTO monitoring_coverage_gaps(organization_id,environment_id,monitor_id,scheduled_at,reason) VALUES($1::uuid,$2::uuid,$3::uuid,$4,'admission_limit') ON CONFLICT DO NOTHING`, m.OrganizationID, m.EnvironmentID, m.ID, m.Next); err != nil {
				return 0, err
			}
			_, _ = tx.Exec(ctx, `INSERT INTO monitoring_operational_events(event_type,job_id,worker_pool_id,safe_details) VALUES('admission_limit',NULL,$1,'scheduled queue limit')`, m.WorkerPoolID)
			if _, err = tx.Exec(ctx, `UPDATE monitors SET next_check_at=$1 WHERE id=$2::uuid AND next_check_at=$3`, next, m.ID, m.Next); err != nil {
				return 0, err
			}
			continue
		}
		if len(m.WorkerPublic) != 32 || m.EncryptionKeyID == "" || m.QueueURL == "" {
			return 0, errors.New("worker pool is not provisioned")
		}
		headers, err := s.headers.Decrypt(m.Headers, m.HeaderVersion.Int32)
		if err != nil && len(m.Headers) > 0 {
			return 0, errors.New("monitor headers unavailable")
		}
		var jobID string
		if err = tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&jobID); err != nil {
			return 0, err
		}
		scheduledAt := m.Next
		if scheduledAt.Add(JobStartWindow).Before(m.Now) {
			if _, err = tx.Exec(ctx, `INSERT INTO monitoring_coverage_gaps(organization_id,environment_id,monitor_id,scheduled_at,reason) VALUES($1::uuid,$2::uuid,$3::uuid,$4,'missed') ON CONFLICT DO NOTHING`, m.OrganizationID, m.EnvironmentID, m.ID, scheduledAt); err != nil {
				return 0, err
			}
			scheduledAt = m.Now
		}
		expires := scheduledAt.Add(JobStartWindow)
		workerPublic, err := ecdh.X25519().NewPublicKey(m.WorkerPublic)
		if err != nil {
			return 0, errors.New("invalid worker pool public key")
		}
		schemaVersion := envelope.SchemaVersion
		if m.SchemaMax < schemaVersion {
			schemaVersion = envelope.PreviousSchemaVersion
		}
		if schemaVersion < m.SchemaMin || schemaVersion > m.SchemaMax {
			return 0, errors.New("worker pool has no compatible protocol schema")
		}
		sealed, attrs, err := envelope.SealJob(envelope.Job{SchemaVersion: schemaVersion, JobID: jobID, JobType: "scheduled", WorkerPoolID: m.WorkerPoolID, NetworkPolicyVersion: m.NetworkPolicy, ScheduledAt: scheduledAt, ExpiresAt: expires, TargetURL: m.TargetURL, Method: m.Method, TimeoutSeconds: m.Timeout, ExpectedStatusMin: m.Min, ExpectedStatusMax: m.Max, Headers: headers, Limits: envelope.RequestLimits{MaxResponseBytes: 65536, MaxHeaderBytes: 32768, MaxRedirects: 3}, PlatformKeyID: s.platformKeyID, WorkerEncryptionKeyID: m.EncryptionKeyID}, s.signer, workerPublic)
		if err != nil {
			return 0, err
		}
		body := []byte(base64.StdEncoding.EncodeToString(sealed))
		hash, err := hex.DecodeString(attrs.SnapshotHash)
		if err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO check_jobs(id,organization_id,environment_id,monitor_id,job_type,state,scheduled_at,monitor_version,worker_pool_id,snapshot_hash,expires_at) VALUES($1::uuid,$2::uuid,$3::uuid,$4::uuid,'scheduled','pending_publish',$5,$6,$7,$8,$9) ON CONFLICT(monitor_id,scheduled_at) DO NOTHING`, jobID, m.OrganizationID, m.EnvironmentID, m.ID, scheduledAt, m.Version, m.WorkerPoolID, hash, expires)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 1 {
			if _, err = tx.Exec(ctx, `INSERT INTO check_dispatch_outbox(job_id,worker_pool_id,queue_url,message_body,schema_version,platform_key_id,worker_encryption_key_id,snapshot_hash,message_deduplication_id,message_group_id,expires_at) VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8,$1,$1,$9)`, jobID, m.WorkerPoolID, m.QueueURL, body, schemaVersion, s.platformKeyID, m.EncryptionKeyID, hash, expires); err != nil {
				return 0, err
			}
			created++
			outstanding++
		}
		if _, err = tx.Exec(ctx, `UPDATE monitors SET next_check_at=$1 WHERE id=$2::uuid AND next_check_at=$3`, next, m.ID, m.Next); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return created, nil
}
func nextSchedule(scheduled, now time.Time, seconds int32) (time.Time, error) {
	if seconds <= 0 {
		return time.Time{}, errors.New("invalid interval")
	}
	interval := time.Duration(seconds) * time.Second
	periods := now.Sub(scheduled)/interval + 1
	if periods < 1 {
		periods = 1
	}
	next := scheduled.Add(periods * interval)
	if !next.After(now) {
		next = next.Add(interval)
	}
	return next, nil
}
