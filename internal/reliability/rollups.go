package reliability

import (
	"context"
	"time"
)

func (s *Service) RollupHour(ctx context.Context, bucket time.Time) (int64, error) {
	bucket = bucket.UTC().Truncate(time.Hour)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `INSERT INTO monitor_rollups_hourly(
 organization_id,environment_id,monitor_id,bucket_start,expected_checks,observed_checks,
 successful_checks,unknown_checks,total_duration_microseconds,updated_at)
WITH expected_slots AS (
 SELECT p.organization_id,p.environment_id,p.monitor_id,slot
 FROM monitor_schedule_periods p
 CROSS JOIN LATERAL generate_series(
   p.first_slot_at + GREATEST(0,ceil(EXTRACT(EPOCH FROM ($1-p.first_slot_at))/p.interval_seconds)::bigint) * make_interval(secs=>p.interval_seconds),
   LEAST(COALESCE(p.ends_at,LEAST($1+INTERVAL '1 hour',CURRENT_TIMESTAMP)),LEAST($1+INTERVAL '1 hour',CURRENT_TIMESTAMP))-INTERVAL '1 microsecond',
   make_interval(secs=>p.interval_seconds)) AS slots(slot)
 WHERE p.starts_at<LEAST($1+INTERVAL '1 hour',CURRENT_TIMESTAMP)
   AND COALESCE(p.ends_at,LEAST($1+INTERVAL '1 hour',CURRENT_TIMESTAMP))>$1
   AND slots.slot>=GREATEST($1,p.starts_at)
), aggregate AS (
 SELECT e.organization_id,e.environment_id,e.monitor_id,count(*)::int expected_checks,
        count(h.job_id)::int observed_checks,
        count(h.job_id) FILTER(WHERE h.succeeded)::int successful_checks,
        COALESCE(sum(h.total_duration_microseconds),0)::bigint total_duration_microseconds
 FROM expected_slots e
 LEFT JOIN health_checks h
   ON h.monitor_id=e.monitor_id AND h.job_type='scheduled' AND h.scheduled_at=e.slot
 GROUP BY e.organization_id,e.environment_id,e.monitor_id
)
SELECT organization_id,environment_id,monitor_id,$1,expected_checks,observed_checks,successful_checks,
       GREATEST(expected_checks-observed_checks,0),total_duration_microseconds,CURRENT_TIMESTAMP
FROM aggregate
ON CONFLICT(monitor_id,bucket_start) DO UPDATE SET
 expected_checks=EXCLUDED.expected_checks,observed_checks=EXCLUDED.observed_checks,
 successful_checks=EXCLUDED.successful_checks,unknown_checks=EXCLUDED.unknown_checks,
 total_duration_microseconds=EXCLUDED.total_duration_microseconds,updated_at=CURRENT_TIMESTAMP`, bucket)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM monitor_rollup_invalidations WHERE bucket_kind='hourly' AND bucket_start=$1`, bucket); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Service) RollupDay(ctx context.Context, day time.Time) (int64, error) {
	date := day.UTC().Format("2006-01-02")
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	var hourlyPending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM monitor_rollup_invalidations
 WHERE bucket_kind='hourly' AND bucket_start >= $1::date AND bucket_start < $1::date+INTERVAL '1 day')`, date).Scan(&hourlyPending); err != nil {
		return 0, err
	}
	if hourlyPending {
		return 0, errHourlyRollupsPending
	}
	tag, err := tx.Exec(ctx, `INSERT INTO monitor_rollups_daily(
 organization_id,environment_id,monitor_id,bucket_start,expected_checks,observed_checks,
 successful_checks,unknown_checks,total_duration_microseconds,updated_at)
SELECT organization_id,environment_id,monitor_id,$1::date,sum(expected_checks),sum(observed_checks),
       sum(successful_checks),sum(unknown_checks),sum(total_duration_microseconds),CURRENT_TIMESTAMP
FROM monitor_rollups_hourly
WHERE bucket_start >= $1::date AND bucket_start < $1::date+INTERVAL '1 day'
GROUP BY organization_id,environment_id,monitor_id
ON CONFLICT(monitor_id,bucket_start) DO UPDATE SET
 expected_checks=EXCLUDED.expected_checks,observed_checks=EXCLUDED.observed_checks,
 successful_checks=EXCLUDED.successful_checks,unknown_checks=EXCLUDED.unknown_checks,
 total_duration_microseconds=EXCLUDED.total_duration_microseconds,updated_at=CURRENT_TIMESTAMP`, date)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM monitor_rollup_invalidations WHERE bucket_kind='daily' AND bucket_start=$1::date`, date); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
