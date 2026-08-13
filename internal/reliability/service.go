// Package reliability computes coverage-aware monitoring summaries and retention.
package reliability

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
}
type Service struct{ db DB }

func New(db DB) *Service { return &Service{db} }

type Report struct {
	Expected, Observed, Successful, Unknown int64
	ObservedUptime, Coverage                *float64
}

func (r Report) Normalize() Report {
	if r.Observed > 0 {
		v := float64(r.Successful) / float64(r.Observed)
		r.ObservedUptime = &v
	}
	if r.Expected > 0 {
		v := float64(r.Observed) / float64(r.Expected)
		r.Coverage = &v
	}
	return r
}
func (s *Service) RollupHour(ctx context.Context, bucket time.Time) (int64, error) {
	bucket = bucket.UTC().Truncate(time.Hour)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `INSERT INTO monitor_rollups_hourly(organization_id,environment_id,monitor_id,bucket_start,expected_checks,observed_checks,successful_checks,unknown_checks,total_duration_microseconds,updated_at)
WITH expected_slots AS (
 SELECT p.organization_id,p.environment_id,p.monitor_id,slot
 FROM monitor_schedule_periods p
 CROSS JOIN LATERAL generate_series(
   p.first_slot_at + GREATEST(0,ceil(EXTRACT(EPOCH FROM ($1-p.first_slot_at))/p.interval_seconds)::bigint) * make_interval(secs=>p.interval_seconds),
	   LEAST(COALESCE(p.ends_at,LEAST($1+INTERVAL '1 hour',CURRENT_TIMESTAMP)),LEAST($1+INTERVAL '1 hour',CURRENT_TIMESTAMP))-INTERVAL '1 microsecond',
   make_interval(secs=>p.interval_seconds)) AS slots(slot)
	 WHERE p.starts_at<LEAST($1+INTERVAL '1 hour',CURRENT_TIMESTAMP) AND COALESCE(p.ends_at,LEAST($1+INTERVAL '1 hour',CURRENT_TIMESTAMP))>$1 AND slots.slot>=GREATEST($1,p.starts_at)
), aggregate AS (
 SELECT e.organization_id,e.environment_id,e.monitor_id,count(*)::int expected_checks,
   count(h.job_id)::int observed_checks,count(h.job_id) FILTER(WHERE h.succeeded)::int successful_checks,
   COALESCE(sum(h.total_duration_microseconds),0)::bigint total_duration_microseconds
 FROM expected_slots e LEFT JOIN health_checks h ON h.monitor_id=e.monitor_id AND h.job_type='scheduled' AND h.scheduled_at=e.slot
 GROUP BY e.organization_id,e.environment_id,e.monitor_id
)
SELECT organization_id,environment_id,monitor_id,$1,expected_checks,observed_checks,successful_checks,
expected_checks-observed_checks,total_duration_microseconds,CURRENT_TIMESTAMP FROM aggregate
ON CONFLICT(monitor_id,bucket_start) DO UPDATE SET expected_checks=EXCLUDED.expected_checks,observed_checks=EXCLUDED.observed_checks,successful_checks=EXCLUDED.successful_checks,unknown_checks=EXCLUDED.unknown_checks,total_duration_microseconds=EXCLUDED.total_duration_microseconds,updated_at=CURRENT_TIMESTAMP`, bucket)
	if err != nil {
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
	tag, err := tx.Exec(ctx, `INSERT INTO monitor_rollups_daily(organization_id,environment_id,monitor_id,bucket_start,expected_checks,observed_checks,successful_checks,unknown_checks,total_duration_microseconds,updated_at)
SELECT organization_id,environment_id,monitor_id,$1::date,sum(expected_checks),sum(observed_checks),sum(successful_checks),sum(unknown_checks),sum(total_duration_microseconds),CURRENT_TIMESTAMP FROM monitor_rollups_hourly WHERE bucket_start >= $1::date AND bucket_start < $1::date+INTERVAL '1 day' GROUP BY organization_id,environment_id,monitor_id
ON CONFLICT(monitor_id,bucket_start) DO UPDATE SET expected_checks=EXCLUDED.expected_checks,observed_checks=EXCLUDED.observed_checks,successful_checks=EXCLUDED.successful_checks,unknown_checks=EXCLUDED.unknown_checks,total_duration_microseconds=EXCLUDED.total_duration_microseconds,updated_at=CURRENT_TIMESTAMP`, date)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
func (s *Service) Report(ctx context.Context, monitorID string, from, to time.Time) (Report, error) {
	if now := time.Now().UTC(); to.After(now) {
		to = now
	}
	if !to.After(from) {
		return Report{}, errors.New("invalid report window")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback(context.Background())
	var r Report
	err = tx.QueryRow(ctx, `WITH expected_slots AS (
 SELECT slot FROM monitor_schedule_periods p
 CROSS JOIN LATERAL generate_series(
   p.first_slot_at + GREATEST(0,ceil(EXTRACT(EPOCH FROM ($2-p.first_slot_at))/p.interval_seconds)::bigint) * make_interval(secs=>p.interval_seconds),
   LEAST(COALESCE(p.ends_at,$3),$3)-INTERVAL '1 microsecond',make_interval(secs=>p.interval_seconds)) AS slots(slot)
 WHERE p.monitor_id=$1::uuid AND p.starts_at<$3 AND COALESCE(p.ends_at,$3)>$2 AND slots.slot>=GREATEST($2,p.starts_at)
), aggregate AS (
 SELECT count(*)::bigint expected,count(h.job_id)::bigint observed,count(h.job_id) FILTER(WHERE h.succeeded)::bigint successful
 FROM expected_slots e LEFT JOIN health_checks h ON h.monitor_id=$1::uuid AND h.job_type='scheduled' AND h.scheduled_at=e.slot
)
SELECT expected,observed,successful,expected-observed FROM aggregate`, monitorID, from.UTC(), to.UTC()).Scan(&r.Expected, &r.Observed, &r.Successful, &r.Unknown)
	if err != nil {
		return Report{}, err
	}
	return r.Normalize(), nil
}
func (s *Service) ApplyRetention(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	var total int64
	commands := []struct {
		sql string
		arg any
	}{{`DELETE FROM health_checks h WHERE h.scheduled_at<$1 AND EXISTS(SELECT 1 FROM monitor_rollups_hourly r WHERE r.monitor_id=h.monitor_id AND r.bucket_start=date_trunc('hour',h.scheduled_at))`, now.UTC().Add(-7 * 24 * time.Hour)}, {`DELETE FROM monitoring_coverage_gaps g WHERE g.scheduled_at<$1 AND EXISTS(SELECT 1 FROM monitor_rollups_hourly r WHERE r.monitor_id=g.monitor_id AND r.bucket_start=date_trunc('hour',g.scheduled_at))`, now.UTC().Add(-7 * 24 * time.Hour)}, {`DELETE FROM monitor_rollups_hourly h WHERE h.bucket_start<$1 AND EXISTS(SELECT 1 FROM monitor_rollups_daily d WHERE d.monitor_id=h.monitor_id AND d.bucket_start=h.bucket_start::date)`, now.UTC().Add(-90 * 24 * time.Hour)}, {`DELETE FROM monitor_rollups_daily WHERE bucket_start<$1::date`, now.UTC().AddDate(-1, 0, 0)}}
	for _, command := range commands {
		tag, e := tx.Exec(ctx, command.sql, command.arg)
		if e != nil {
			return 0, e
		}
		total += tag.RowsAffected()
	}
	_, err = tx.Exec(ctx, `DELETE FROM check_jobs j WHERE ((state='completed' AND completed_at<$1) OR (state IN('dead','expired','cancelled','quarantined') AND completed_at<$2)) AND NOT EXISTS(SELECT 1 FROM health_checks h WHERE h.job_id=j.id)`, now.UTC().Add(-48*time.Hour), now.UTC().Add(-7*24*time.Hour))
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return total, nil
}

var _ = errors.New
