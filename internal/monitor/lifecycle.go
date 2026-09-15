package monitor

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
)

// Update replaces all configurable values and increments the immutable
// monitor version used by future jobs. Header values are never returned.
func (s *Service) Update(ctx context.Context, userID, environmentID, monitorID string, input UpdateInput) (Monitor, error) {
	normalized, err := normalizeCreateInput(userID, environmentID, input)
	if err != nil || !uuidPattern.MatchString(monitorID) {
		return Monitor{}, ErrInvalidInput
	}
	tx, row, err := s.lockManaged(ctx, userID, environmentID, monitorID)
	if err != nil {
		return Monitor{}, err
	}
	defer tx.Rollback(context.Background())
	ciphertext, keyVersion, names, err := s.encryptHeaders(normalized.Headers)
	if err != nil {
		return Monitor{}, err
	}
	result := Monitor{}
	var pausedAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `UPDATE monitors SET name=$1,target_url=$2,method=$3,interval_seconds=$4::integer,
timeout_seconds=$5,expected_status_min=$6,expected_status_max=$7,headers_ciphertext=$8,
header_key_version=$9,worker_pool_id=$10,version=version+1,updated_at=CURRENT_TIMESTAMP,
next_check_at=CASE WHEN paused_at IS NULL THEN CURRENT_TIMESTAMP + mod(hashtextextended(id::text,0) & 2147483647,$4::bigint)*INTERVAL '1 second' ELSE next_check_at END
WHERE organization_id=$11::uuid AND environment_id=$12::uuid AND id=$13::uuid AND deleted_at IS NULL
RETURNING id::text,organization_id::text,environment_id::text,name,target_url,method,
interval_seconds,timeout_seconds,expected_status_min,expected_status_max,version,paused_at,
worker_pool_id,created_at,updated_at`, normalized.Name, normalized.TargetURL, normalized.Method,
		normalized.IntervalSeconds, normalized.TimeoutSeconds, normalized.ExpectedStatusMin, normalized.ExpectedStatusMax,
		ciphertext, nullableInt32(keyVersion), normalized.WorkerPoolID, row.OrganizationID, row.EnvironmentID, row.ID).Scan(
		&result.ID, &result.OrganizationID, &result.EnvironmentID, &result.Name, &result.TargetURL, &result.Method,
		&result.IntervalSeconds, &result.TimeoutSeconds, &result.ExpectedStatusMin, &result.ExpectedStatusMax,
		&result.Version, &pausedAt, &result.WorkerPoolID, &result.CreatedAt, &result.UpdatedAt)
	if err != nil {
		return Monitor{}, fmt.Errorf("update monitor: %w", err)
	}
	result.Paused = pausedAt.Valid
	result.HeaderNames = names
	if _, err = tx.Exec(ctx, `UPDATE monitor_schedule_periods SET ends_at=CURRENT_TIMESTAMP WHERE monitor_id=$1::uuid AND ends_at IS NULL`, row.ID); err != nil {
		return Monitor{}, fmt.Errorf("close monitor schedule period: %w", err)
	}
	if !result.Paused {
		if _, err = tx.Exec(ctx, `INSERT INTO monitor_schedule_periods(organization_id,environment_id,monitor_id,monitor_version,interval_seconds,worker_pool_id,starts_at,first_slot_at)
SELECT organization_id,environment_id,id,version,interval_seconds,worker_pool_id,CURRENT_TIMESTAMP,next_check_at FROM monitors WHERE id=$1::uuid`, row.ID); err != nil {
			return Monitor{}, fmt.Errorf("record monitor schedule period: %w", err)
		}
	}
	if err = recordRefresh(ctx, tx, result.OrganizationID, result.EnvironmentID, "monitor.changed", "monitor", result.ID); err != nil {
		return Monitor{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Monitor{}, fmt.Errorf("commit monitor update: %w", err)
	}
	return result, nil
}

func (s *Service) Pause(ctx context.Context, userID, environmentID, monitorID string) (Monitor, error) {
	return s.setPaused(ctx, userID, environmentID, monitorID, true)
}
func (s *Service) Resume(ctx context.Context, userID, environmentID, monitorID string) (Monitor, error) {
	return s.setPaused(ctx, userID, environmentID, monitorID, false)
}
func (s *Service) setPaused(ctx context.Context, userID, environmentID, monitorID string, paused bool) (Monitor, error) {
	tx, row, err := s.lockManaged(ctx, userID, environmentID, monitorID)
	if err != nil {
		return Monitor{}, err
	}
	defer tx.Rollback(context.Background())
	result := Monitor{}
	var pausedAt pgtype.Timestamptz
	var ciphertext []byte
	var keyVersion pgtype.Int4
	err = tx.QueryRow(ctx, `UPDATE monitors SET paused_at=CASE WHEN $1 THEN CURRENT_TIMESTAMP ELSE NULL END,
next_check_at=CASE WHEN $1 THEN next_check_at ELSE CURRENT_TIMESTAMP + mod(hashtextextended(id::text,0) & 2147483647,interval_seconds::bigint)*INTERVAL '1 second' END,version=version+1,updated_at=CURRENT_TIMESTAMP
WHERE organization_id=$2::uuid AND environment_id=$3::uuid AND id=$4::uuid AND deleted_at IS NULL
RETURNING id::text,organization_id::text,environment_id::text,name,target_url,method,interval_seconds,
timeout_seconds,expected_status_min,expected_status_max,version,paused_at,worker_pool_id,headers_ciphertext,
header_key_version,created_at,updated_at`, paused, row.OrganizationID, row.EnvironmentID, row.ID).Scan(&result.ID, &result.OrganizationID, &result.EnvironmentID, &result.Name, &result.TargetURL, &result.Method, &result.IntervalSeconds, &result.TimeoutSeconds, &result.ExpectedStatusMin, &result.ExpectedStatusMax, &result.Version, &pausedAt, &result.WorkerPoolID, &ciphertext, &keyVersion, &result.CreatedAt, &result.UpdatedAt)
	if err != nil {
		return Monitor{}, fmt.Errorf("change monitor state: %w", err)
	}
	result.Paused = pausedAt.Valid
	result.HeaderNames = s.headerNames(ciphertext, keyVersion)
	if _, err = tx.Exec(ctx, `UPDATE monitor_schedule_periods SET ends_at=CURRENT_TIMESTAMP WHERE monitor_id=$1::uuid AND ends_at IS NULL`, row.ID); err != nil {
		return Monitor{}, fmt.Errorf("close monitor schedule period: %w", err)
	}
	if !paused {
		if _, err = tx.Exec(ctx, `INSERT INTO monitor_schedule_periods(organization_id,environment_id,monitor_id,monitor_version,interval_seconds,worker_pool_id,starts_at,first_slot_at)
SELECT organization_id,environment_id,id,version,interval_seconds,worker_pool_id,CURRENT_TIMESTAMP,next_check_at FROM monitors WHERE id=$1::uuid`, row.ID); err != nil {
			return Monitor{}, fmt.Errorf("record monitor schedule period: %w", err)
		}
	}
	if err = recordRefresh(ctx, tx, result.OrganizationID, result.EnvironmentID, "monitor.changed", "monitor", result.ID); err != nil {
		return Monitor{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Monitor{}, fmt.Errorf("commit monitor state: %w", err)
	}
	return result, nil
}

func (s *Service) Delete(ctx context.Context, userID, environmentID, monitorID string) error {
	tx, row, err := s.lockManaged(ctx, userID, environmentID, monitorID)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `UPDATE monitors SET deleted_at=CURRENT_TIMESTAMP,version=version+1,updated_at=CURRENT_TIMESTAMP WHERE organization_id=$1::uuid AND environment_id=$2::uuid AND id=$3::uuid AND deleted_at IS NULL`, row.OrganizationID, row.EnvironmentID, row.ID)
	if err != nil {
		return fmt.Errorf("delete monitor: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrMonitorNotFound
	}
	if _, err = tx.Exec(ctx, `UPDATE monitor_schedule_periods SET ends_at=CURRENT_TIMESTAMP WHERE monitor_id=$1::uuid AND ends_at IS NULL`, row.ID); err != nil {
		return fmt.Errorf("close monitor schedule period: %w", err)
	}
	if err = recordRefresh(ctx, tx, row.OrganizationID, row.EnvironmentID, "monitor.changed", "monitor", row.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) TestNow(ctx context.Context, userID, environmentID, monitorID string) (string, error) {
	tx, row, err := s.lockManaged(ctx, userID, environmentID, monitorID)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(742019205)`); err != nil {
		return "", err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM check_jobs WHERE job_type='manual_test' AND state IN ('pending','pending_publish','published','running')`).Scan(&count); err != nil {
		return "", err
	}
	if count >= 10 {
		return "", ErrManualQueueFull
	}
	var scheduledPressure bool
	if err = tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM monitors WHERE paused_at IS NULL AND deleted_at IS NULL AND next_check_at < CURRENT_TIMESTAMP - INTERVAL '30 seconds')
		OR (SELECT count(*) >= 900 FROM check_jobs WHERE job_type='scheduled' AND state IN ('pending','pending_publish','published','running'))`).Scan(&scheduledPressure); err != nil {
		return "", err
	}
	if scheduledPressure {
		return "", ErrManualQueueFull
	}
	var encryptionKeyID, queueURL string
	var workerPublic []byte
	var networkPolicy int
	err = tx.QueryRow(ctx, `SELECT encryption_key_id,encryption_public_key,network_policy_version,job_queue_url FROM worker_pools WHERE id=$1 AND enabled AND lifecycle_state='active' AND schema_min <= $2 AND schema_max >= $2 FOR SHARE`, row.WorkerPoolID, envelope.SchemaVersion).Scan(&encryptionKeyID, &workerPublic, &networkPolicy, &queueURL)
	if errors.Is(err, pgx.ErrNoRows) || len(workerPublic) != 32 || encryptionKeyID == "" || queueURL == "" {
		return "", ErrQueueUnavailable
	}
	if err != nil {
		return "", err
	}
	workerKey, err := ecdh.X25519().NewPublicKey(workerPublic)
	if err != nil {
		return "", ErrQueueUnavailable
	}
	headers, err := s.headers.Decrypt(row.Headers, row.HeaderVersion.Int32)
	if err != nil {
		return "", ErrQueueUnavailable
	}
	var id string
	var scheduledAt time.Time
	if err = tx.QueryRow(ctx, `SELECT gen_random_uuid()::text,CURRENT_TIMESTAMP`).Scan(&id, &scheduledAt); err != nil {
		return "", err
	}
	expiresAt := scheduledAt.Add(2 * time.Minute)
	sealed, attrs, err := envelope.SealJob(envelope.Job{SchemaVersion: envelope.SchemaVersion, JobID: id, JobType: "manual_test", WorkerPoolID: row.WorkerPoolID, NetworkPolicyVersion: networkPolicy, ScheduledAt: scheduledAt, ExpiresAt: expiresAt, TargetURL: row.TargetURL, Method: row.Method, TimeoutSeconds: row.Timeout, ExpectedStatusMin: row.Min, ExpectedStatusMax: row.Max, Headers: headers, Limits: envelope.RequestLimits{MaxResponseBytes: 65536, MaxHeaderBytes: 32768, MaxRedirects: 3}, PlatformKeyID: s.signingKeyID, WorkerEncryptionKeyID: encryptionKeyID}, s.signingKey, workerKey)
	if err != nil {
		return "", ErrQueueUnavailable
	}
	body := []byte(base64.StdEncoding.EncodeToString(sealed))
	hash, err := hex.DecodeString(attrs.SnapshotHash)
	if err != nil {
		return "", err
	}
	err = tx.QueryRow(ctx, `INSERT INTO check_jobs(id,organization_id,environment_id,monitor_id,job_type,state,scheduled_at,monitor_version,worker_pool_id,snapshot_hash,expires_at) VALUES($1::uuid,$2::uuid,$3::uuid,$4::uuid,'manual_test','pending_publish',$5,$6,$7,$8,$9) RETURNING id::text`, id, row.OrganizationID, row.EnvironmentID, row.ID, scheduledAt, row.Version, row.WorkerPoolID, hash, expiresAt).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create manual job: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO check_dispatch_outbox(job_id,worker_pool_id,queue_url,message_body,schema_version,platform_key_id,worker_encryption_key_id,snapshot_hash,message_deduplication_id,message_group_id,expires_at) VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8,$1,$1,$9)`, id, row.WorkerPoolID, queueURL, body, envelope.SchemaVersion, s.signingKeyID, encryptionKeyID, hash, expiresAt); err != nil {
		return "", fmt.Errorf("create manual dispatch: %w", err)
	}
	if err = recordRefresh(ctx, tx, row.OrganizationID, row.EnvironmentID, "monitor.changed", "monitor", row.ID); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

func recordRefresh(ctx context.Context, tx pgx.Tx, organizationID, environmentID, eventType, resourceType, resourceID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO api_refresh_events(organization_id,environment_id,event_type,resource_type,resource_id) VALUES($1::uuid,$2::uuid,$3,$4,$5::uuid)`, organizationID, environmentID, eventType, resourceType, resourceID)
	return err
}

type managedMonitor struct {
	ID, OrganizationID, EnvironmentID, WorkerPoolID string
	TargetURL, Method                               string
	Version                                         int64
	Timeout                                         int32
	Min, Max                                        int16
	Headers                                         []byte
	HeaderVersion                                   pgtype.Int4
}

func (s *Service) lockManaged(ctx context.Context, userID, environmentID, monitorID string) (pgx.Tx, managedMonitor, error) {
	if !uuidPattern.MatchString(userID) || !uuidPattern.MatchString(environmentID) || !uuidPattern.MatchString(monitorID) {
		return nil, managedMonitor{}, ErrMonitorNotFound
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, managedMonitor{}, err
	}
	var row managedMonitor
	var role string
	err = tx.QueryRow(ctx, `SELECT m.id::text,m.organization_id::text,m.environment_id::text,m.version,m.worker_pool_id,m.target_url,m.method,m.timeout_seconds,m.expected_status_min,m.expected_status_max,m.headers_ciphertext,m.header_key_version,om.role FROM monitors m JOIN org_members om ON om.organization_id=m.organization_id AND om.user_id=$1::uuid WHERE m.environment_id=$2::uuid AND m.id=$3::uuid AND m.deleted_at IS NULL FOR UPDATE OF m`, userID, environmentID, monitorID).Scan(&row.ID, &row.OrganizationID, &row.EnvironmentID, &row.Version, &row.WorkerPoolID, &row.TargetURL, &row.Method, &row.Timeout, &row.Min, &row.Max, &row.Headers, &row.HeaderVersion, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		tx.Rollback(ctx)
		return nil, row, ErrMonitorNotFound
	}
	if err != nil {
		tx.Rollback(ctx)
		return nil, row, err
	}
	if !authorization.Allows(authorization.Role(role), authorization.PermissionMonitorsManage) {
		tx.Rollback(ctx)
		return nil, row, ErrForbidden
	}
	return tx, row, nil
}
