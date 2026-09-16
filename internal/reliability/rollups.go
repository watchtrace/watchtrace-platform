package reliability

import (
	"context"
	"time"

	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

func (s *Service) RollupHour(ctx context.Context, bucket time.Time) (int64, error) {
	bucket = bucket.UTC().Truncate(time.Hour)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	queries := database.New(tx)
	count, err := queries.UpsertHourlyMonitorRollups(ctx, databaseTimestamp(bucket))
	if err != nil {
		return 0, err
	}
	if err = queries.DeleteHourlyRollupInvalidations(ctx, databaseTimestamp(bucket)); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Service) RollupDay(ctx context.Context, day time.Time) (int64, error) {
	date := day.UTC().Format("2006-01-02")
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	queries := database.New(tx)
	hourlyPending, err := queries.HasHourlyRollupInvalidationsForDay(ctx, date)
	if err != nil {
		return 0, err
	}
	if hourlyPending {
		return 0, errHourlyRollupsPending
	}
	count, err := queries.UpsertDailyMonitorRollups(ctx, date)
	if err != nil {
		return 0, err
	}
	if err = queries.DeleteDailyRollupInvalidations(ctx, date); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}
