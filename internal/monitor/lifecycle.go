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
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
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
	queries := database.New(tx)
	ciphertext, keyVersion, names, err := s.encryptHeaders(normalized.Headers)
	if err != nil {
		return Monitor{}, err
	}
	updated, err := queries.UpdateManagedMonitor(ctx, database.UpdateManagedMonitorParams{
		Name: normalized.Name, TargetUrl: normalized.TargetURL, Method: normalized.Method,
		IntervalSeconds: normalized.IntervalSeconds, TimeoutSeconds: normalized.TimeoutSeconds,
		ExpectedStatusMin: normalized.ExpectedStatusMin, ExpectedStatusMax: normalized.ExpectedStatusMax,
		HeadersCiphertext: ciphertext, HeaderKeyVersion: nullableInt32(keyVersion),
		WorkerPoolID: normalized.WorkerPoolID, OrganizationID: row.OrganizationID,
		EnvironmentID: row.EnvironmentID, MonitorID: row.ID,
	})
	if err != nil {
		return Monitor{}, fmt.Errorf("update monitor: %w", err)
	}
	result := Monitor{ID: updated.ID, OrganizationID: updated.OrganizationID, EnvironmentID: updated.EnvironmentID,
		Name: updated.Name, TargetURL: updated.TargetUrl, Method: updated.Method,
		IntervalSeconds: updated.IntervalSeconds, TimeoutSeconds: updated.TimeoutSeconds,
		ExpectedStatusMin: updated.ExpectedStatusMin, ExpectedStatusMax: updated.ExpectedStatusMax,
		Version: updated.Version, Paused: updated.PausedAt.Valid, WorkerPoolID: updated.WorkerPoolID,
		CreatedAt: updated.CreatedAt.Time, UpdatedAt: updated.UpdatedAt.Time}
	result.HeaderNames = names
	if err = queries.CloseMonitorSchedulePeriod(ctx, row.ID); err != nil {
		return Monitor{}, fmt.Errorf("close monitor schedule period: %w", err)
	}
	if !result.Paused {
		if err = queries.OpenMonitorSchedulePeriod(ctx, row.ID); err != nil {
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
	queries := database.New(tx)
	updated, err := queries.SetManagedMonitorPaused(ctx, database.SetManagedMonitorPausedParams{
		Paused: paused, OrganizationID: row.OrganizationID,
		EnvironmentID: row.EnvironmentID, MonitorID: row.ID,
	})
	if err != nil {
		return Monitor{}, fmt.Errorf("change monitor state: %w", err)
	}
	result := Monitor{ID: updated.ID, OrganizationID: updated.OrganizationID, EnvironmentID: updated.EnvironmentID,
		Name: updated.Name, TargetURL: updated.TargetUrl, Method: updated.Method,
		IntervalSeconds: updated.IntervalSeconds, TimeoutSeconds: updated.TimeoutSeconds,
		ExpectedStatusMin: updated.ExpectedStatusMin, ExpectedStatusMax: updated.ExpectedStatusMax,
		Version: updated.Version, Paused: updated.PausedAt.Valid, WorkerPoolID: updated.WorkerPoolID,
		CreatedAt: updated.CreatedAt.Time, UpdatedAt: updated.UpdatedAt.Time}
	result.HeaderNames = s.headerNames(updated.HeadersCiphertext, updated.HeaderKeyVersion)
	if err = queries.CloseMonitorSchedulePeriod(ctx, row.ID); err != nil {
		return Monitor{}, fmt.Errorf("close monitor schedule period: %w", err)
	}
	if !paused {
		if err = queries.OpenMonitorSchedulePeriod(ctx, row.ID); err != nil {
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
	queries := database.New(tx)
	rowsAffected, err := queries.SoftDeleteMonitor(ctx, database.SoftDeleteMonitorParams{OrganizationID: row.OrganizationID, EnvironmentID: row.EnvironmentID, MonitorID: row.ID})
	if err != nil {
		return fmt.Errorf("delete monitor: %w", err)
	}
	if rowsAffected != 1 {
		return ErrMonitorNotFound
	}
	if err = queries.CloseMonitorSchedulePeriod(ctx, row.ID); err != nil {
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
	queries := database.New(tx)
	if err = queries.AcquireManualTestQueueLock(ctx); err != nil {
		return "", err
	}
	count, err := queries.CountActiveManualTestJobs(ctx)
	if err != nil {
		return "", err
	}
	if count >= 10 {
		return "", ErrManualQueueFull
	}
	scheduledPressure, err := queries.HasScheduledQueuePressure(ctx)
	if err != nil {
		return "", err
	}
	if scheduledPressure.Bool {
		return "", ErrManualQueueFull
	}
	workerPool, err := queries.GetManualDispatchWorkerPool(ctx, database.GetManualDispatchWorkerPoolParams{WorkerPoolID: row.WorkerPoolID, SchemaVersion: envelope.SchemaVersion})
	if errors.Is(err, pgx.ErrNoRows) || len(workerPool.EncryptionPublicKey) != 32 || !workerPool.EncryptionKeyID.Valid || workerPool.EncryptionKeyID.String == "" || !workerPool.JobQueueUrl.Valid || workerPool.JobQueueUrl.String == "" {
		return "", ErrQueueUnavailable
	}
	if err != nil {
		return "", err
	}
	workerKey, err := ecdh.X25519().NewPublicKey(workerPool.EncryptionPublicKey)
	if err != nil {
		return "", ErrQueueUnavailable
	}
	headers, err := s.headers.Decrypt(row.Headers, row.HeaderVersion.Int32)
	if err != nil {
		return "", ErrQueueUnavailable
	}
	id, err := queries.NewManualJobID(ctx)
	if err != nil {
		return "", err
	}
	databaseTime, err := queries.GetDatabaseTime(ctx)
	if err != nil {
		return "", err
	}
	scheduledAt := databaseTime.Time
	expiresAt := scheduledAt.Add(2 * time.Minute)
	sealed, attrs, err := envelope.SealJob(envelope.Job{SchemaVersion: envelope.SchemaVersion, JobID: id, JobType: "manual_test", WorkerPoolID: row.WorkerPoolID, NetworkPolicyVersion: int(workerPool.NetworkPolicyVersion), ScheduledAt: scheduledAt, ExpiresAt: expiresAt, TargetURL: row.TargetURL, Method: row.Method, TimeoutSeconds: row.Timeout, ExpectedStatusMin: row.Min, ExpectedStatusMax: row.Max, Headers: headers, Limits: envelope.RequestLimits{MaxResponseBytes: 65536, MaxHeaderBytes: 32768, MaxRedirects: 3}, PlatformKeyID: s.signingKeyID, WorkerEncryptionKeyID: workerPool.EncryptionKeyID.String}, s.signingKey, workerKey)
	if err != nil {
		return "", ErrQueueUnavailable
	}
	body := []byte(base64.StdEncoding.EncodeToString(sealed))
	hash, err := hex.DecodeString(attrs.SnapshotHash)
	if err != nil {
		return "", err
	}
	id, err = queries.CreateManualCheckJob(ctx, database.CreateManualCheckJobParams{JobID: id, OrganizationID: row.OrganizationID, EnvironmentID: row.EnvironmentID, MonitorID: row.ID, ScheduledAt: timestamp(scheduledAt), MonitorVersion: row.Version, WorkerPoolID: row.WorkerPoolID, SnapshotHash: hash, ExpiresAt: timestamp(expiresAt)})
	if err != nil {
		return "", fmt.Errorf("create manual job: %w", err)
	}
	if err = queries.CreateManualDispatchOutbox(ctx, database.CreateManualDispatchOutboxParams{JobID: id, WorkerPoolID: row.WorkerPoolID, QueueUrl: workerPool.JobQueueUrl.String, MessageBody: body, SchemaVersion: envelope.SchemaVersion, PlatformKeyID: s.signingKeyID, WorkerEncryptionKeyID: workerPool.EncryptionKeyID.String, SnapshotHash: hash, ExpiresAt: timestamp(expiresAt)}); err != nil {
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
	return database.New(tx).InsertMonitorRefreshEvent(ctx, database.InsertMonitorRefreshEventParams{OrganizationID: organizationID, EnvironmentID: environmentID, EventType: eventType, ResourceType: resourceType, ResourceID: resourceID})
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
	locked, err := database.New(tx).LockManagedMonitor(ctx, database.LockManagedMonitorParams{UserID: userID, EnvironmentID: environmentID, MonitorID: monitorID})
	if errors.Is(err, pgx.ErrNoRows) {
		tx.Rollback(ctx)
		return nil, managedMonitor{}, ErrMonitorNotFound
	}
	if err != nil {
		tx.Rollback(ctx)
		return nil, managedMonitor{}, err
	}
	row := managedMonitor{ID: locked.ID, OrganizationID: locked.OrganizationID, EnvironmentID: locked.EnvironmentID, Version: locked.Version, WorkerPoolID: locked.WorkerPoolID, TargetURL: locked.TargetUrl, Method: locked.Method, Timeout: locked.TimeoutSeconds, Min: locked.ExpectedStatusMin, Max: locked.ExpectedStatusMax, Headers: locked.HeadersCiphertext, HeaderVersion: locked.HeaderKeyVersion}
	if !authorization.Allows(authorization.Role(locked.Role), authorization.PermissionMonitorsManage) {
		tx.Rollback(ctx)
		return nil, row, ErrForbidden
	}
	return tx, row, nil
}

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
