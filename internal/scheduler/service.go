// Package scheduler creates durable PostgreSQL jobs for due monitors.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

const (
	// DefaultBatchSize bounds ordinary scheduler transactions while matching
	// the initial target dispatch rate.
	DefaultBatchSize = 20
	// MaximumBatchSize prevents a caller from holding an unbounded set of
	// monitor row locks in one transaction.
	MaximumBatchSize = 100
)

var ErrInvalidBatchSize = errors.New("invalid scheduler batch size")

type databaseConnection interface {
	database.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

// Service atomically turns due monitor schedules into durable check jobs.
type Service struct {
	db databaseConnection
}

// NewService constructs a PostgreSQL-backed scheduler service.
func NewService(db databaseConnection) *Service {
	return &Service{db: db}
}

// ScheduleDue locks at most batchSize due monitors, inserts their stable queue
// rows, and advances their schedules in one short transaction. It returns the
// number of newly inserted jobs; an already-present idempotency key advances
// the schedule without counting a second job.
func (service *Service) ScheduleDue(ctx context.Context, batchSize int) (int, error) {
	if batchSize < 1 || batchSize > MaximumBatchSize {
		return 0, ErrInvalidBatchSize
	}

	tx, err := service.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin scheduler transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	queries := database.New(tx)
	due, err := queries.LockDueMonitors(ctx, int32(batchSize))
	if err != nil {
		return 0, fmt.Errorf("lock due monitors: %w", err)
	}

	createdJobs := 0
	for _, monitor := range due {
		if !monitor.NextCheckAt.Valid || !monitor.SchedulerTime.Valid {
			return 0, errors.New("due monitor has an invalid schedule timestamp")
		}

		created, err := queries.CreateScheduledCheckJob(ctx, database.CreateScheduledCheckJobParams{
			OrganizationID: monitor.OrganizationID,
			EnvironmentID:  monitor.EnvironmentID,
			MonitorID:      monitor.MonitorID,
			ScheduledAt:    monitor.NextCheckAt,
		})
		if err != nil {
			return 0, fmt.Errorf("create scheduled check job: %w", err)
		}
		if created < 0 || created > 1 {
			return 0, fmt.Errorf("create scheduled check job affected %d rows", created)
		}

		nextCheckAt, err := nextScheduleAfter(
			monitor.NextCheckAt.Time,
			monitor.SchedulerTime.Time,
			monitor.IntervalSeconds,
		)
		if err != nil {
			return 0, err
		}
		advanced, err := queries.AdvanceMonitorSchedule(ctx, database.AdvanceMonitorScheduleParams{
			NextCheckAt: pgtype.Timestamptz{
				Time:  nextCheckAt,
				Valid: true,
			},
			OrganizationID: monitor.OrganizationID,
			EnvironmentID:  monitor.EnvironmentID,
			MonitorID:      monitor.MonitorID,
			ScheduledAt:    monitor.NextCheckAt,
		})
		if err != nil {
			return 0, fmt.Errorf("advance monitor schedule: %w", err)
		}
		if advanced != 1 {
			return 0, fmt.Errorf("advance monitor schedule affected %d rows", advanced)
		}
		createdJobs += int(created)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit scheduler transaction: %w", err)
	}
	return createdJobs, nil
}

func nextScheduleAfter(scheduledAt, schedulerTime time.Time, intervalSeconds int32) (time.Time, error) {
	if intervalSeconds <= 0 {
		return time.Time{}, errors.New("due monitor has an invalid interval")
	}

	interval := time.Duration(intervalSeconds) * time.Second
	if scheduledAt.After(schedulerTime) {
		return scheduledAt, nil
	}
	elapsed := schedulerTime.Sub(scheduledAt)
	periods := elapsed/interval + 1
	next := scheduledAt.Add(periods * interval)
	if !next.After(schedulerTime) {
		next = next.Add(interval)
	}
	return next, nil
}
