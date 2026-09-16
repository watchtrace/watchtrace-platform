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
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
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
	queries := database.New(tx)
	outstanding, err := queries.CountActiveScheduledJobs(ctx)
	if err != nil {
		return 0, err
	}
	databaseNow, err := queries.GetDatabaseTime(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := queries.ListDueScheduledMonitors(ctx, int32(batch))
	if err != nil {
		return 0, err
	}
	due := []dueMonitor{}
	for _, row := range rows {
		due = append(due, dueMonitor{
			ID: row.ID, OrganizationID: row.OrganizationID, EnvironmentID: row.EnvironmentID,
			Version: row.Version, TargetURL: row.TargetUrl, Method: row.Method,
			Interval: row.IntervalSeconds, Timeout: row.TimeoutSeconds,
			Min: row.ExpectedStatusMin, Max: row.ExpectedStatusMax,
			Next: row.NextCheckAt.Time, Now: databaseNow.Time,
			Headers: row.HeadersCiphertext, HeaderVersion: row.HeaderKeyVersion,
			WorkerPoolID: row.WorkerPoolID, NetworkPolicy: int(row.NetworkPolicyVersion),
			EncryptionKeyID: row.EncryptionKeyID.String, WorkerPublic: row.EncryptionPublicKey,
			QueueURL: row.JobQueueUrl.String, SchemaMin: int(row.SchemaMin), SchemaMax: int(row.SchemaMax),
		})
	}
	created := 0
	for _, m := range due {
		next, err := nextSchedule(m.Next, m.Now, m.Interval)
		if err != nil {
			return 0, err
		}
		if outstanding >= GlobalScheduledLimit {
			if err = queries.InsertMonitoringCoverageGap(ctx, database.InsertMonitoringCoverageGapParams{OrganizationID: m.OrganizationID, EnvironmentID: m.EnvironmentID, MonitorID: m.ID, ScheduledAt: databaseTimestamp(m.Next), Reason: "admission_limit"}); err != nil {
				return 0, err
			}
			_ = queries.InsertMonitoringOperationalEvent(ctx, database.InsertMonitoringOperationalEventParams{EventType: "admission_limit", WorkerPoolID: m.WorkerPoolID, SafeDetails: "scheduled queue limit"})
			if err = queries.UpdateMonitorNextCheck(ctx, database.UpdateMonitorNextCheckParams{NextCheckAt: databaseTimestamp(next), MonitorID: m.ID, PreviousCheckAt: databaseTimestamp(m.Next)}); err != nil {
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
		jobID, err := queries.NewScheduledJobID(ctx)
		if err != nil {
			return 0, err
		}
		scheduledAt := m.Next
		if scheduledAt.Add(JobStartWindow).Before(m.Now) {
			if err = queries.InsertMonitoringCoverageGap(ctx, database.InsertMonitoringCoverageGapParams{OrganizationID: m.OrganizationID, EnvironmentID: m.EnvironmentID, MonitorID: m.ID, ScheduledAt: databaseTimestamp(scheduledAt), Reason: "missed"}); err != nil {
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
		rowsAffected, err := queries.CreateScheduledCheckJob(ctx, database.CreateScheduledCheckJobParams{JobID: jobID, OrganizationID: m.OrganizationID, EnvironmentID: m.EnvironmentID, MonitorID: m.ID, ScheduledAt: databaseTimestamp(scheduledAt), MonitorVersion: m.Version, WorkerPoolID: m.WorkerPoolID, SnapshotHash: hash, ExpiresAt: databaseTimestamp(expires)})
		if err != nil {
			return 0, err
		}
		if rowsAffected == 1 {
			if err = queries.CreateScheduledDispatchOutbox(ctx, database.CreateScheduledDispatchOutboxParams{JobID: jobID, WorkerPoolID: m.WorkerPoolID, QueueUrl: m.QueueURL, MessageBody: body, SchemaVersion: int16(schemaVersion), PlatformKeyID: s.platformKeyID, WorkerEncryptionKeyID: m.EncryptionKeyID, SnapshotHash: hash, ExpiresAt: databaseTimestamp(expires)}); err != nil {
				return 0, err
			}
			created++
			outstanding++
		}
		if err = queries.UpdateMonitorNextCheck(ctx, database.UpdateMonitorNextCheckParams{NextCheckAt: databaseTimestamp(next), MonitorID: m.ID, PreviousCheckAt: databaseTimestamp(m.Next)}); err != nil {
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
