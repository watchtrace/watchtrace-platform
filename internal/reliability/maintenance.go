package reliability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

func (s *Service) RepairInvalidated(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("invalid rollup repair limit")
	}
	repaired := 0
	for repaired < limit {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return repaired, err
		}
		invalidation, err := database.New(tx).GetNextRepairableRollupInvalidation(ctx)
		tx.Rollback(context.Background())
		if errors.Is(err, pgx.ErrNoRows) {
			return repaired, nil
		}
		if err != nil {
			return repaired, err
		}
		if invalidation.BucketKind == "hourly" {
			_, err = s.RollupHour(ctx, invalidation.BucketStart.Time)
		} else {
			_, err = s.RollupDay(ctx, invalidation.BucketStart.Time)
		}
		if err != nil {
			return repaired, err
		}
		repaired++
	}
	return repaired, nil
}

func (s *Service) AdvanceRollups(ctx context.Context, now time.Time, maxHours, maxDays int) error {
	if maxHours < 0 || maxHours > 168 || maxDays < 0 || maxDays > 90 {
		return errors.New("invalid rollup catch-up bounds")
	}
	now = now.UTC()
	for index := 0; index < maxHours; index++ {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return err
		}
		throughValue, err := database.New(tx).LockHourlyRollupCheckpoint(ctx)
		tx.Rollback(context.Background())
		if err != nil {
			return err
		}
		next := throughValue.Time.UTC().Add(time.Hour)
		if !next.Before(now.Truncate(time.Hour)) {
			break
		}
		if _, err = s.RollupHour(ctx, next); err != nil {
			return err
		}
		tx, err = s.db.Begin(ctx)
		if err != nil {
			return err
		}
		err = database.New(tx).AdvanceHourlyRollupCheckpoint(ctx, databaseTimestamp(next))
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			tx.Rollback(context.Background())
		}
		if err != nil {
			return err
		}
	}
	for index := 0; index < maxDays; index++ {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return err
		}
		throughValue, err := database.New(tx).LockDailyRollupCheckpoint(ctx)
		tx.Rollback(context.Background())
		if err != nil {
			return err
		}
		next := throughValue.Time.UTC().AddDate(0, 0, 1)
		if !next.Before(now.Truncate(24 * time.Hour)) {
			break
		}
		_, err = s.RollupDay(ctx, next)
		if errors.Is(err, errHourlyRollupsPending) {
			break
		}
		if err != nil {
			return err
		}
		tx, err = s.db.Begin(ctx)
		if err != nil {
			return err
		}
		err = database.New(tx).AdvanceDailyRollupCheckpoint(ctx, next.Format("2006-01-02"))
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			tx.Rollback(context.Background())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ApplyRetention(ctx context.Context, now time.Time) (int64, error) {
	now = now.UTC()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	queries := database.New(tx)
	var total int64
	counts := make([]int64, 5)
	if counts[0], err = queries.DeleteRetainedHealthChecks(ctx, databaseTimestamp(now.Add(-7*24*time.Hour))); err != nil {
		return 0, err
	}
	if counts[1], err = queries.DeleteRetainedCoverageGaps(ctx, databaseTimestamp(now.Add(-7*24*time.Hour))); err != nil {
		return 0, err
	}
	if counts[2], err = queries.DeleteRetainedHourlyRollups(ctx, databaseTimestamp(now.Add(-90*24*time.Hour))); err != nil {
		return 0, err
	}
	if counts[3], err = queries.DeleteRetainedDailyRollups(ctx, now.AddDate(-1, 0, 0).Format("2006-01-02")); err != nil {
		return 0, err
	}
	if counts[4], err = queries.DeleteRetainedCheckJobs(ctx, database.DeleteRetainedCheckJobsParams{CompletedBefore: databaseTimestamp(now.Add(-48 * time.Hour)), NonterminalBefore: databaseTimestamp(now.Add(-7 * 24 * time.Hour))}); err != nil {
		return 0, err
	}
	for _, count := range counts {
		total += count
	}
	if err = queries.RecordRetentionCheckpoint(ctx, databaseTimestamp(now)); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return total, nil
}

func (s *Service) Maintain(ctx context.Context, now time.Time) error {
	now = now.UTC()
	if err := s.AdvanceRollups(ctx, now, 24, 7); err != nil {
		return fmt.Errorf("advance rollups: %w", err)
	}
	if _, err := s.RollupHour(ctx, now); err != nil {
		return fmt.Errorf("refresh current hour: %w", err)
	}
	if _, err := s.RollupDay(ctx, now); err != nil && !errors.Is(err, errHourlyRollupsPending) {
		return fmt.Errorf("refresh current day: %w", err)
	}
	if _, err := s.RepairInvalidated(ctx, 24); err != nil {
		return fmt.Errorf("repair invalidated rollups: %w", err)
	}
	if _, err := s.RefreshDueStates(ctx, now, 1000); err != nil {
		return fmt.Errorf("refresh monitor states: %w", err)
	}
	if _, err := s.ApplyRetention(ctx, now); err != nil {
		return fmt.Errorf("apply retention: %w", err)
	}
	return nil
}
