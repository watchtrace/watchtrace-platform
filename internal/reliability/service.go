// Package reliability computes coverage-aware monitoring summaries, ordered
// monitor state, repeatable rollups, and bounded retention.
package reliability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	failureThreshold  = 3
	recoveryThreshold = 2
	correctionWindow  = 10 * time.Minute
)

var errHourlyRollupsPending = errors.New("hourly rollups pending")

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
}

type Service struct{ db DB }

func New(db DB) *Service { return &Service{db: db} }

type Report struct {
	Expected, Observed, Successful, Unknown int64
	ObservedUptime, Coverage                *float64
}

func (r Report) Normalize() Report {
	if r.Observed < 0 {
		r.Observed = 0
	}
	if r.Successful < 0 {
		r.Successful = 0
	}
	if r.Successful > r.Observed {
		r.Successful = r.Observed
	}
	if r.Expected < 0 {
		r.Expected = 0
	}
	if r.Observed > r.Expected {
		r.Observed = r.Expected
	}
	r.Unknown = r.Expected - r.Observed
	if r.Observed > 0 {
		value := float64(r.Successful) / float64(r.Observed)
		r.ObservedUptime = &value
	} else {
		r.ObservedUptime = nil
	}
	if r.Expected > 0 {
		value := float64(r.Observed) / float64(r.Expected)
		r.Coverage = &value
	} else {
		r.Coverage = nil
	}
	return r
}

func (s *Service) Report(ctx context.Context, monitorID string, from, to time.Time) (Report, error) {
	if now := time.Now().UTC(); to.After(now) {
		to = now
	}
	from, to = from.UTC(), to.UTC()
	if !to.After(from) {
		return Report{}, errors.New("invalid report window")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback(context.Background())
	var report Report
	err = tx.QueryRow(ctx, `WITH expected_slots AS (
 SELECT slot
 FROM monitor_schedule_periods p
 CROSS JOIN LATERAL generate_series(
   p.first_slot_at + GREATEST(0,ceil(EXTRACT(EPOCH FROM ($2-p.first_slot_at))/p.interval_seconds)::bigint) * make_interval(secs=>p.interval_seconds),
   LEAST(COALESCE(p.ends_at,$3),$3)-INTERVAL '1 microsecond',
   make_interval(secs=>p.interval_seconds)) AS slots(slot)
 WHERE p.monitor_id=$1::uuid
   AND p.starts_at<$3
   AND COALESCE(p.ends_at,$3)>$2
   AND slots.slot>=GREATEST($2,p.starts_at)
), aggregate AS (
 SELECT count(*)::bigint expected,
        count(h.job_id)::bigint observed,
        count(h.job_id) FILTER(WHERE h.succeeded)::bigint successful
 FROM expected_slots e
 LEFT JOIN health_checks h
   ON h.monitor_id=$1::uuid
  AND h.job_type='scheduled'
  AND h.scheduled_at=e.slot
)
SELECT expected,observed,successful FROM aggregate`, monitorID, from, to).
		Scan(&report.Expected, &report.Observed, &report.Successful)
	if err != nil {
		return Report{}, err
	}
	return report.Normalize(), nil
}

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

type acceptedSlot struct {
	jobID     string
	scheduled time.Time
}

type stateSnapshot struct {
	display, observed                 string
	failures, successes               int
	lastObservedAt, lastEvaluatedAt   *time.Time
	lastObservedJob, lastEvaluatedJob *string
}

// EvaluateAcceptedTx updates ordered state inside the result-acceptance
// transaction. Results beyond the ten-minute correction window remain raw
// reporting corrections and do not rewrite current state.
func EvaluateAcceptedTx(ctx context.Context, tx pgx.Tx, monitorID, jobID string, scheduled, expires, now time.Time) (bool, error) {
	if now.UTC().After(expires.UTC().Add(correctionWindow)) {
		return false, nil
	}
	return evaluateTx(ctx, tx, monitorID, &acceptedSlot{jobID: jobID, scheduled: scheduled.UTC()}, now.UTC())
}

func (s *Service) EvaluateAccepted(ctx context.Context, monitorID, jobID string, scheduled, expires, now time.Time) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	corrected, err := EvaluateAcceptedTx(ctx, tx, monitorID, jobID, scheduled, expires, now)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return corrected, nil
}

func (s *Service) EvaluateMonitor(ctx context.Context, monitorID string, now time.Time) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = evaluateTx(ctx, tx, monitorID, nil, now.UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func evaluateTx(ctx context.Context, tx pgx.Tx, monitorID string, accepted *acceptedSlot, now time.Time) (bool, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO monitor_reliability_states(monitor_id)
SELECT id FROM monitors WHERE id=$1::uuid ON CONFLICT DO NOTHING`, monitorID); err != nil {
		return false, err
	}
	var current stateSnapshot
	err := tx.QueryRow(ctx, `SELECT display_state,observed_state,consecutive_failures,consecutive_successes,
 last_observed_scheduled_at,last_observed_job_id::text,last_evaluated_scheduled_at,last_evaluated_job_id::text
FROM monitor_reliability_states WHERE monitor_id=$1::uuid FOR UPDATE`, monitorID).Scan(
		&current.display, &current.observed, &current.failures, &current.successes,
		&current.lastObservedAt, &current.lastObservedJob, &current.lastEvaluatedAt, &current.lastEvaluatedJob)
	if err != nil {
		return false, err
	}

	baseline := current
	correction := accepted != nil && current.lastObservedAt != nil && current.lastObservedJob != nil &&
		!orderAfter(accepted.scheduled, accepted.jobID, *current.lastObservedAt, *current.lastObservedJob)
	if correction {
		baseline = stateSnapshot{display: "unknown", observed: "unknown"}
		previousErr := tx.QueryRow(ctx, `SELECT observed_state,consecutive_failures,consecutive_successes,scheduled_at,job_id::text
FROM monitor_result_evaluations
WHERE monitor_id=$1::uuid AND (scheduled_at,job_id)<($2,$3::uuid)
ORDER BY scheduled_at DESC,job_id DESC LIMIT 1`, monitorID, accepted.scheduled, accepted.jobID).Scan(
			&baseline.observed, &baseline.failures, &baseline.successes, &baseline.lastObservedAt, &baseline.lastObservedJob)
		if previousErr != nil && !errors.Is(previousErr, pgx.ErrNoRows) {
			return false, previousErr
		}
		if _, err = tx.Exec(ctx, `DELETE FROM monitor_result_evaluations
WHERE monitor_id=$1::uuid AND (scheduled_at,job_id)>=($2,$3::uuid)`, monitorID, accepted.scheduled, accepted.jobID); err != nil {
			return false, err
		}
	}

	query := `SELECT job_id::text,scheduled_at,succeeded FROM health_checks
WHERE monitor_id=$1::uuid AND job_type='scheduled'`
	args := []any{monitorID}
	if correction {
		query += ` AND (scheduled_at,job_id)>=($2,$3::uuid)`
		args = append(args, accepted.scheduled, accepted.jobID)
	} else if current.lastObservedAt != nil && current.lastObservedJob != nil {
		query += ` AND (scheduled_at,job_id)>($2,$3::uuid)`
		args = append(args, *current.lastObservedAt, *current.lastObservedJob)
	}
	query += ` ORDER BY scheduled_at,job_id`
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return false, err
	}
	type observedResult struct {
		jobID       string
		scheduledAt time.Time
		succeeded   bool
	}
	results := []observedResult{}
	for rows.Next() {
		var result observedResult
		if err = rows.Scan(&result.jobID, &result.scheduledAt, &result.succeeded); err != nil {
			rows.Close()
			return false, err
		}
		results = append(results, result)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, err
	}
	observed, failures, successes := baseline.observed, baseline.failures, baseline.successes
	lastObservedAt, lastObservedJob := baseline.lastObservedAt, baseline.lastObservedJob
	for _, result := range results {
		observed, failures, successes = advanceObservedState(observed, failures, successes, result.succeeded)
		if _, err = tx.Exec(ctx, `INSERT INTO monitor_result_evaluations(
 monitor_id,job_id,scheduled_at,succeeded,observed_state,consecutive_failures,consecutive_successes)
VALUES($1::uuid,$2::uuid,$3,$4,$5,$6,$7)
ON CONFLICT(job_id) DO UPDATE SET observed_state=EXCLUDED.observed_state,
 consecutive_failures=EXCLUDED.consecutive_failures,consecutive_successes=EXCLUDED.consecutive_successes,
 evaluated_at=CURRENT_TIMESTAMP`, monitorID, result.jobID, result.scheduledAt, result.succeeded, observed, failures, successes); err != nil {
			return false, err
		}
		scheduledCopy, jobCopy := result.scheduledAt.UTC(), result.jobID
		lastObservedAt, lastObservedJob = &scheduledCopy, &jobCopy
	}

	var newestExpected *time.Time
	err = tx.QueryRow(ctx, `SELECT max(slot) FROM monitor_schedule_periods p
CROSS JOIN LATERAL generate_series(
 p.first_slot_at,
 LEAST(COALESCE(p.ends_at-INTERVAL '1 microsecond',$2),$2),
 make_interval(secs=>p.interval_seconds)) AS slots(slot)
WHERE p.monitor_id=$1::uuid AND p.starts_at<=$2`, monitorID, now).Scan(&newestExpected)
	if err != nil {
		return false, err
	}
	display := "unknown"
	var evaluatedJob *string
	if newestExpected != nil {
		var evaluatedState, job string
		evaluationErr := tx.QueryRow(ctx, `SELECT e.observed_state,e.job_id::text
FROM monitor_result_evaluations e
WHERE e.monitor_id=$1::uuid AND e.scheduled_at=$2
ORDER BY e.job_id DESC LIMIT 1`, monitorID, *newestExpected).Scan(&evaluatedState, &job)
		if evaluationErr == nil {
			display = evaluatedState
			evaluatedJob = &job
		} else if !errors.Is(evaluationErr, pgx.ErrNoRows) {
			return false, evaluationErr
		}
	}
	if observed == "" {
		observed = "unknown"
	}
	_, err = tx.Exec(ctx, `UPDATE monitor_reliability_states SET
 display_state=$2,observed_state=$3,consecutive_failures=$4,consecutive_successes=$5,
 last_observed_scheduled_at=$6,last_observed_job_id=$7::uuid,newest_expected_scheduled_at=$8,
 last_evaluated_scheduled_at=$8,last_evaluated_job_id=$9::uuid,updated_at=CURRENT_TIMESTAMP
WHERE monitor_id=$1::uuid`, monitorID, display, observed, failures, successes,
		lastObservedAt, lastObservedJob, newestExpected, evaluatedJob)
	if err != nil {
		return false, err
	}
	if newestExpected != nil {
		_, err = tx.Exec(ctx, `INSERT INTO monitor_evaluation_positions(monitor_id,last_scheduled_at,last_job_id,invalidated_from)
VALUES($1::uuid,$2,$3::uuid,NULL)
ON CONFLICT(monitor_id) DO UPDATE SET last_scheduled_at=EXCLUDED.last_scheduled_at,
 last_job_id=EXCLUDED.last_job_id,invalidated_from=NULL,updated_at=CURRENT_TIMESTAMP`, monitorID, *newestExpected, evaluatedJob)
		if err != nil {
			return false, err
		}
	}
	if correction {
		_, err = tx.Exec(ctx, `INSERT INTO monitor_state_correction_events(
 monitor_id,accepted_job_id,corrected_from,previous_display_state,corrected_display_state)
VALUES($1::uuid,$2::uuid,$3,$4,$5) ON CONFLICT(monitor_id,accepted_job_id) DO NOTHING`,
			monitorID, accepted.jobID, accepted.scheduled, current.display, display)
		if err != nil {
			return false, err
		}
	}
	return correction, nil
}

func advanceObservedState(state string, failures, successes int, succeeded bool) (string, int, int) {
	if succeeded {
		failures = 0
		if state == "down" {
			successes++
			if successes >= recoveryThreshold {
				return "healthy", 0, 0
			}
			return "down", 0, successes
		}
		return "healthy", 0, 0
	}
	successes = 0
	if failures < failureThreshold {
		failures++
	}
	if failures >= failureThreshold {
		return "down", failureThreshold, 0
	}
	return "degraded", failures, 0
}

func orderAfter(leftTime time.Time, leftID string, rightTime time.Time, rightID string) bool {
	if leftTime.After(rightTime) {
		return true
	}
	return leftTime.Equal(rightTime) && leftID > rightID
}

func (s *Service) RefreshDueStates(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("invalid state refresh limit")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT m.id::text FROM monitors m
JOIN monitor_schedule_periods p ON p.monitor_id=m.id
WHERE m.deleted_at IS NULL AND p.starts_at<=$1
ORDER BY m.id LIMIT $2`, now.UTC(), limit)
	if err != nil {
		tx.Rollback(context.Background())
		return 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			tx.Rollback(context.Background())
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		tx.Rollback(context.Background())
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err = s.EvaluateMonitor(ctx, id, now); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

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
