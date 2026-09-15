package reliability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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
		var kind string
		var bucket time.Time
		err = tx.QueryRow(ctx, `SELECT bucket_kind,bucket_start
FROM monitor_rollup_invalidations i
WHERE bucket_kind='hourly' OR NOT EXISTS(
 SELECT 1 FROM monitor_rollup_invalidations h
 WHERE h.bucket_kind='hourly' AND h.bucket_start>=i.bucket_start AND h.bucket_start<i.bucket_start+INTERVAL '1 day')
ORDER BY CASE bucket_kind WHEN 'hourly' THEN 0 ELSE 1 END,bucket_start LIMIT 1`).Scan(&kind, &bucket)
		tx.Rollback(context.Background())
		if errors.Is(err, pgx.ErrNoRows) {
			return repaired, nil
		}
		if err != nil {
			return repaired, err
		}
		if kind == "hourly" {
			_, err = s.RollupHour(ctx, bucket)
		} else {
			_, err = s.RollupDay(ctx, bucket)
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
		var through time.Time
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT hourly_through FROM monitoring_rollup_checkpoint WHERE singleton FOR UPDATE`).Scan(&through)
		tx.Rollback(context.Background())
		if err != nil {
			return err
		}
		next := through.UTC().Add(time.Hour)
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
		_, err = tx.Exec(ctx, `UPDATE monitoring_rollup_checkpoint SET hourly_through=GREATEST(hourly_through,$1),updated_at=CURRENT_TIMESTAMP WHERE singleton`, next)
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
		var through time.Time
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT daily_through::timestamptz FROM monitoring_rollup_checkpoint WHERE singleton FOR UPDATE`).Scan(&through)
		tx.Rollback(context.Background())
		if err != nil {
			return err
		}
		next := through.UTC().AddDate(0, 0, 1)
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
		_, err = tx.Exec(ctx, `UPDATE monitoring_rollup_checkpoint SET daily_through=GREATEST(daily_through,$1::date),updated_at=CURRENT_TIMESTAMP WHERE singleton`, next)
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
	var total int64
	commands := []struct {
		sql string
		arg any
	}{
		{`DELETE FROM health_checks h WHERE h.scheduled_at<$1
AND EXISTS(SELECT 1 FROM monitor_rollups_hourly r WHERE r.monitor_id=h.monitor_id AND r.bucket_start=date_trunc('hour',h.scheduled_at))
AND NOT EXISTS(SELECT 1 FROM monitor_rollup_invalidations i WHERE i.monitor_id=h.monitor_id AND i.bucket_kind='hourly' AND i.bucket_start=date_trunc('hour',h.scheduled_at))`, now.Add(-7 * 24 * time.Hour)},
		{`DELETE FROM monitoring_coverage_gaps g WHERE g.scheduled_at<$1
AND EXISTS(SELECT 1 FROM monitor_rollups_hourly r WHERE r.monitor_id=g.monitor_id AND r.bucket_start=date_trunc('hour',g.scheduled_at))
AND NOT EXISTS(SELECT 1 FROM monitor_rollup_invalidations i WHERE i.monitor_id=g.monitor_id AND i.bucket_kind='hourly' AND i.bucket_start=date_trunc('hour',g.scheduled_at))`, now.Add(-7 * 24 * time.Hour)},
		{`DELETE FROM monitor_rollups_hourly h WHERE h.bucket_start<$1
AND EXISTS(SELECT 1 FROM monitor_rollups_daily d WHERE d.monitor_id=h.monitor_id AND d.bucket_start=h.bucket_start::date)
AND NOT EXISTS(SELECT 1 FROM monitor_rollup_invalidations i WHERE i.monitor_id=h.monitor_id AND i.bucket_kind='daily' AND i.bucket_start=h.bucket_start::date)`, now.Add(-90 * 24 * time.Hour)},
		{`DELETE FROM monitor_rollups_daily WHERE bucket_start<$1::date`, now.AddDate(-1, 0, 0)},
	}
	for _, command := range commands {
		tag, execErr := tx.Exec(ctx, command.sql, command.arg)
		if execErr != nil {
			return 0, execErr
		}
		total += tag.RowsAffected()
	}
	tag, err := tx.Exec(ctx, `DELETE FROM check_jobs j WHERE
 ((state='completed' AND completed_at<$1) OR
  (state IN('dead','expired','cancelled','quarantined') AND completed_at<$2))
AND NOT EXISTS(SELECT 1 FROM health_checks h WHERE h.job_id=j.id)`, now.Add(-48*time.Hour), now.Add(-7*24*time.Hour))
	if err != nil {
		return 0, err
	}
	total += tag.RowsAffected()
	if _, err = tx.Exec(ctx, `UPDATE monitoring_rollup_checkpoint SET last_retention_at=$1,updated_at=CURRENT_TIMESTAMP WHERE singleton`, now); err != nil {
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
