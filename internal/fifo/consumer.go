package fifo

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/incident"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
	"github.com/watchtrace/watchtrace-platform/internal/quarantine"
	"github.com/watchtrace/watchtrace-platform/internal/reliability"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
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

// NewResultConsumer constructs a result consumer with all dependencies needed
// to safely preserve conflicting valid results.
func NewResultConsumer(db DB, source ResultSource, sealer *quarantine.Sealer) (*ResultConsumer, error) {
	if db == nil || source == nil || sealer == nil {
		return nil, errors.New("fifo: result database, source, and quarantine sealer are required")
	}
	return &ResultConsumer{db: db, source: source, now: time.Now, sealer: sealer}, nil
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
	queries := database.New(tx)
	job, err := queries.LockResultCheckJob(ctx, database.LockResultCheckJobParams{ResultKeyID: peeked.ResultKeyID, JobID: peeked.JobID})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, envelope.ErrInvalid
	}
	if err != nil {
		return true, err
	}
	poolID, hash := job.WorkerPoolID, job.SnapshotHash
	jobType, organizationID, environmentID, monitorID := job.JobType, job.OrganizationID, job.EnvironmentID, job.MonitorID
	scheduled, expires := job.ScheduledAt.Time, job.ExpiresAt.Time
	result, err := envelope.VerifyResult(delivery.Body, ed25519.PublicKey(job.ResultPublicKey))
	if err != nil || result.WorkerPoolID != poolID || result.SnapshotHash != fmt.Sprintf("%x", hash) || !result.ScheduledAt.Equal(scheduled) || result.StartedAt.Before(scheduled.Add(-5*time.Second)) || result.StartedAt.After(expires.Add(5*time.Second)) || result.CompletedAt.After(c.now().UTC().Add(5*time.Second)) {
		return true, envelope.ErrInvalid
	}
	existing, existingErr := queries.GetAcceptedHealthCheck(ctx, result.JobID)
	if existingErr == nil {
		if fmt.Sprintf("%x", existing.SnapshotHash) != result.SnapshotHash || existing.ExecutionAttemptID != result.AttemptID || existing.ResultID != result.ResultID {
			encrypted, err := c.sealer.Seal(delivery.Body, []byte("result:"+result.ResultID))
			if err != nil {
				return true, err
			}
			_ = queries.InsertResultConflict(ctx, database.InsertResultConflictParams{JobID: result.JobID, ResultID: result.ResultID, WorkerPoolID: poolID, SnapshotHash: hash, EncryptedPayload: encrypted})
			_ = queries.InsertConflictingResultQuarantine(ctx, database.InsertConflictingResultQuarantineParams{JobID: result.JobID, ResultID: result.ResultID, WorkerPoolID: databaseText(poolID), SnapshotHash: hash, EncryptedPayload: encrypted})
			_ = queries.InsertMonitoringOperationalEvent(ctx, database.InsertMonitoringOperationalEventParams{EventType: "result_conflict", JobID: result.JobID, WorkerPoolID: poolID, SafeDetails: "conflicting valid result"})
			if err = tx.Commit(ctx); err != nil {
				return true, err
			}
			return true, c.source.AcknowledgeResult(ctx, delivery)
		}
		_ = queries.MarkDispatchRepaired(ctx, result.JobID)
		if err = tx.Commit(ctx); err != nil {
			return true, err
		}
		return true, c.source.AcknowledgeResult(ctx, delivery)
	} else if !errors.Is(existingErr, pgx.ErrNoRows) {
		return true, existingErr
	}
	err = queries.InsertAcceptedHealthCheck(ctx, database.InsertAcceptedHealthCheckParams{JobID: result.JobID, ResultID: result.ResultID, OrganizationID: organizationID, EnvironmentID: environmentID, MonitorID: monitorID, JobType: jobType, ScheduledAt: databaseTimestamp(scheduled), StartedAt: databaseTimestamp(result.StartedAt), CompletedAt: databaseTimestamp(result.CompletedAt), Succeeded: result.Succeeded, StatusCode: optionalInt16(result.StatusCode), ErrorCategory: optionalText(result.ErrorCategory), TotalDurationMicroseconds: result.TotalMicros, SnapshotHash: hash, WorkerPoolID: databaseText(poolID), WorkerID: databaseText(result.WorkerID), ExecutionAttemptID: result.AttemptID, DnsDurationMicroseconds: optionalInt64(result.DNSMicros), ConnectDurationMicroseconds: optionalInt64(result.ConnectMicros), TlsDurationMicroseconds: optionalInt64(result.TLSMicros), FirstByteDurationMicroseconds: optionalInt64(result.FirstByteMicros)})
	if err != nil {
		return true, err
	}
	err = queries.CompleteCheckJob(ctx, database.CompleteCheckJobParams{StartedAt: databaseTimestamp(result.StartedAt), CompletedAt: databaseTimestamp(result.CompletedAt), WorkerID: databaseText(result.WorkerID), ExecutionAttemptID: result.AttemptID, JobID: result.JobID})
	if err != nil {
		return true, err
	}
	err = queries.MarkDispatchRepaired(ctx, result.JobID)
	if err != nil {
		return true, err
	}
	err = queries.DeleteRecoveredCoverageGaps(ctx, database.DeleteRecoveredCoverageGapsParams{MonitorID: monitorID, ScheduledAt: databaseTimestamp(scheduled)})
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
			if err = queries.InsertMonitoringOperationalEvent(ctx, database.InsertMonitoringOperationalEventParams{EventType: "late_correction", JobID: result.JobID, WorkerPoolID: poolID, SafeDetails: details}); err != nil {
				return true, err
			}
		}
		if err = queries.UpsertHourlyRollupInvalidation(ctx, database.UpsertHourlyRollupInvalidationParams{MonitorID: monitorID, ScheduledAt: databaseTimestamp(scheduled), Reason: reason}); err != nil {
			return true, err
		}
		if err = queries.UpsertDailyRollupInvalidation(ctx, database.UpsertDailyRollupInvalidationParams{MonitorID: monitorID, ScheduledAt: databaseTimestamp(scheduled), Reason: reason}); err != nil {
			return true, err
		}
	}
	if err = queries.InsertAcceptedCheckRefreshEvent(ctx, database.InsertAcceptedCheckRefreshEventParams{OrganizationID: organizationID, EnvironmentID: environmentID, JobID: result.JobID}); err != nil {
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
	one, err := database.New(tx).DatabasePing(ctx)
	if err != nil {
		return false, err
	}
	return one == 1, nil
}
