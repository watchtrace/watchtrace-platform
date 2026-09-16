// Package operations exposes bounded, non-sensitive platform health and maintenance state.
package operations

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/watchtrace/watchtrace-platform/internal/fifo"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
}
type Service struct {
	db        DB
	now       func() time.Time
	sqs       SQSClient
	queueURLs QueueURLs
}

func New(db DB) *Service { return &Service{db: db, now: time.Now} }

func NewWithSQS(db DB, client SQSClient, urls QueueURLs) *Service {
	return &Service{db: db, now: time.Now, sqs: client, queueURLs: urls}
}

type Maintenance struct {
	Task          string     `json:"task"`
	LastStartedAt *time.Time `json:"last_started_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt *time.Time `json:"last_failure_at,omitempty"`
	LastSafeError *string    `json:"last_safe_error,omitempty"`
	RowsAffected  int64      `json:"rows_affected"`
}
type Health struct {
	GeneratedAt                  time.Time       `json:"generated_at"`
	DatabaseDelayMilliseconds    int64           `json:"database_delay_ms"`
	SchedulerDelaySeconds        int64           `json:"scheduler_delay_seconds"`
	ResultConsumerDelaySeconds   int64           `json:"result_consumer_delay_seconds"`
	MissedChecks                 int64           `json:"missed_checks"`
	CompletedChecks              int64           `json:"completed_checks_24h"`
	FailedChecks                 int64           `json:"failed_checks_24h"`
	NotificationPending          int64           `json:"notification_pending"`
	NotificationOldestAgeSeconds int64           `json:"notification_oldest_age_seconds"`
	Queue                        fifo.Metrics    `json:"job_ledger"`
	Transport                    TransportHealth `json:"sqs"`
	Maintenance                  []Maintenance   `json:"maintenance"`
	Disk                         DiskHealth      `json:"disk"`
}

func (s *Service) Read(ctx context.Context) (Health, error) {
	started := s.now()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Health{}, err
	}
	defer tx.Rollback(context.Background())
	h := Health{GeneratedAt: started.UTC(), Maintenance: []Maintenance{}, Disk: readDiskHealth()}
	queries := database.New(tx)
	if h.SchedulerDelaySeconds, err = queries.GetSchedulerDelaySeconds(ctx); err != nil {
		return h, err
	}
	if h.ResultConsumerDelaySeconds, err = queries.GetResultConsumerDelaySeconds(ctx); err != nil {
		return h, err
	}
	if h.MissedChecks, err = queries.CountRecentCoverageGaps(ctx); err != nil {
		return h, err
	}
	checkCounts, err := queries.GetRecentCheckCounts(ctx)
	if err != nil {
		return h, err
	}
	h.CompletedChecks, h.FailedChecks = checkCounts.CompletedChecks, checkCounts.FailedChecks
	if h.NotificationPending, err = queries.CountPendingNotifications(ctx); err != nil {
		return h, err
	}
	oldest, err := queries.GetOldestPendingNotification(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return h, err
	}
	if err == nil && oldest.Valid {
		h.NotificationOldestAgeSeconds = int64(started.Sub(oldest.Time).Seconds())
		if h.NotificationOldestAgeSeconds < 0 {
			h.NotificationOldestAgeSeconds = 0
		}
	}
	maintenance, err := queries.ListMaintenanceStatuses(ctx)
	if err != nil {
		return h, err
	}
	for _, row := range maintenance {
		h.Maintenance = append(h.Maintenance, Maintenance{
			Task: row.TaskName, LastStartedAt: optionalTime(row.LastStartedAt),
			LastSuccessAt: optionalTime(row.LastSuccessAt), LastFailureAt: optionalTime(row.LastFailureAt),
			LastSafeError: optionalText(row.LastSafeError), RowsAffected: row.RowsAffected,
		})
	}
	if err = tx.Commit(ctx); err != nil {
		return h, err
	}
	h.DatabaseDelayMilliseconds = time.Since(started).Milliseconds()
	h.Queue, err = fifo.ReadMetrics(ctx, s.db, started)
	if err != nil {
		return h, err
	}
	h.Transport, err = ReadTransportHealth(ctx, s.sqs, s.queueURLs)
	return h, err
}
func (s *Service) Record(ctx context.Context, task string, started time.Time, count int64, runErr error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var safe pgtype.Text
	var success, failure pgtype.Timestamptz
	if runErr == nil {
		success = timestamp(s.now().UTC())
	} else {
		failure = timestamp(s.now().UTC())
		safe = pgtype.Text{String: "maintenance_failed", Valid: true}
	}
	if count < 0 {
		return errors.New("invalid maintenance count")
	}
	rowsAffected, err := database.New(tx).UpdateMaintenanceStatus(ctx, database.UpdateMaintenanceStatusParams{
		TaskName: task, LastStartedAt: timestamp(started.UTC()), RowsAffected: count,
		LastSuccessAt: success, LastFailureAt: failure, LastSafeError: safe,
	})
	if err != nil {
		return err
	}
	if rowsAffected != 1 {
		return errors.New("unknown maintenance task")
	}
	return tx.Commit(ctx)
}
func (s *Service) CleanupExpired(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	queries := database.New(tx)
	cutoff := timestamp(now.UTC())
	counts := make([]int64, 4)
	if counts[0], err = queries.DeleteExpiredUserActionTokens(ctx, cutoff); err != nil {
		return 0, err
	}
	if counts[1], err = queries.DeleteExpiredOrganizationInvitations(ctx, cutoff); err != nil {
		return 0, err
	}
	if counts[2], err = queries.DeleteOldNotificationDeliveries(ctx, cutoff); err != nil {
		return 0, err
	}
	if counts[3], err = queries.DeleteOldRefreshEvents(ctx, cutoff); err != nil {
		return 0, err
	}
	var count int64
	for _, n := range counts {
		count += n
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func optionalTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func optionalText(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
