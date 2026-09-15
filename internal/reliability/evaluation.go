package reliability

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

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
	failureLimit, recoveryLimit := failureThreshold, recoveryThreshold
	thresholdErr := tx.QueryRow(ctx, `SELECT failure_threshold,recovery_threshold FROM alert_rules
WHERE monitor_id=$1::uuid AND rule_key='consecutive_failures' AND enabled`, monitorID).Scan(&failureLimit, &recoveryLimit)
	if thresholdErr != nil && !errors.Is(thresholdErr, pgx.ErrNoRows) {
		return false, thresholdErr
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
		observed, failures, successes = advanceObservedState(observed, failures, successes, result.succeeded, failureLimit, recoveryLimit)
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

func advanceObservedState(state string, failures, successes int, succeeded bool, failureLimit, recoveryLimit int) (string, int, int) {
	if succeeded {
		failures = 0
		if state == "down" {
			successes++
			if successes >= recoveryLimit {
				return "healthy", 0, 0
			}
			return "down", 0, successes
		}
		return "healthy", 0, 0
	}
	successes = 0
	if failures < failureLimit {
		failures++
	}
	if failures >= failureLimit {
		return "down", failureLimit, 0
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
	rows, err := tx.Query(ctx, `SELECT DISTINCT m.id::text AS monitor_id FROM monitors m
JOIN monitor_schedule_periods p ON p.monitor_id=m.id
WHERE m.deleted_at IS NULL AND p.starts_at<=$1
ORDER BY monitor_id LIMIT $2`, now.UTC(), limit)
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
