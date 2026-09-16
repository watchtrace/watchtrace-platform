package reliability

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
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
	queries := database.New(tx)
	if err := queries.EnsureMonitorReliabilityState(ctx, monitorID); err != nil {
		return false, err
	}
	state, err := queries.LockMonitorReliabilityState(ctx, monitorID)
	if err != nil {
		return false, err
	}
	current := stateSnapshot{
		display: state.DisplayState, observed: state.ObservedState,
		failures: int(state.ConsecutiveFailures), successes: int(state.ConsecutiveSuccesses),
		lastObservedAt: optionalTime(state.LastObservedScheduledAt), lastObservedJob: optionalString(state.LastObservedJobID),
		lastEvaluatedAt: optionalTime(state.LastEvaluatedScheduledAt), lastEvaluatedJob: optionalString(state.LastEvaluatedJobID),
	}
	failureLimit, recoveryLimit := failureThreshold, recoveryThreshold
	thresholds, thresholdErr := queries.GetMonitorAlertThresholds(ctx, monitorID)
	if thresholdErr == nil {
		failureLimit, recoveryLimit = int(thresholds.FailureThreshold), int(thresholds.RecoveryThreshold)
	}
	if thresholdErr != nil && !errors.Is(thresholdErr, pgx.ErrNoRows) {
		return false, thresholdErr
	}

	baseline := current
	correction := accepted != nil && current.lastObservedAt != nil && current.lastObservedJob != nil &&
		!orderAfter(accepted.scheduled, accepted.jobID, *current.lastObservedAt, *current.lastObservedJob)
	if correction {
		baseline = stateSnapshot{display: "unknown", observed: "unknown"}
		previous, previousErr := queries.GetReliabilityBaselineBefore(ctx, database.GetReliabilityBaselineBeforeParams{MonitorID: monitorID, ScheduledAt: databaseTimestamp(accepted.scheduled), JobID: accepted.jobID})
		if previousErr == nil {
			scheduledAt, jobID := previous.ScheduledAt.Time, previous.JobID
			baseline.observed, baseline.failures, baseline.successes = previous.ObservedState, int(previous.ConsecutiveFailures), int(previous.ConsecutiveSuccesses)
			baseline.lastObservedAt, baseline.lastObservedJob = &scheduledAt, &jobID
		}
		if previousErr != nil && !errors.Is(previousErr, pgx.ErrNoRows) {
			return false, previousErr
		}
		if err = queries.DeleteReliabilityEvaluationsFrom(ctx, database.DeleteReliabilityEvaluationsFromParams{MonitorID: monitorID, ScheduledAt: databaseTimestamp(accepted.scheduled), JobID: accepted.jobID}); err != nil {
			return false, err
		}
	}

	type observedResult struct {
		jobID       string
		scheduledAt time.Time
		succeeded   bool
	}
	results := []observedResult{}
	if correction {
		rows, queryErr := queries.ListScheduledHealthChecksFrom(ctx, database.ListScheduledHealthChecksFromParams{MonitorID: monitorID, ScheduledAt: databaseTimestamp(accepted.scheduled), JobID: accepted.jobID})
		if queryErr != nil {
			return false, queryErr
		}
		for _, row := range rows {
			results = append(results, observedResult{jobID: row.JobID, scheduledAt: row.ScheduledAt.Time, succeeded: row.Succeeded})
		}
	} else if current.lastObservedAt != nil && current.lastObservedJob != nil {
		rows, queryErr := queries.ListScheduledHealthChecksAfter(ctx, database.ListScheduledHealthChecksAfterParams{MonitorID: monitorID, ScheduledAt: databaseTimestamp(*current.lastObservedAt), JobID: *current.lastObservedJob})
		if queryErr != nil {
			return false, queryErr
		}
		for _, row := range rows {
			results = append(results, observedResult{jobID: row.JobID, scheduledAt: row.ScheduledAt.Time, succeeded: row.Succeeded})
		}
	} else {
		rows, queryErr := queries.ListAllScheduledHealthChecks(ctx, monitorID)
		if queryErr != nil {
			return false, queryErr
		}
		for _, row := range rows {
			results = append(results, observedResult{jobID: row.JobID, scheduledAt: row.ScheduledAt.Time, succeeded: row.Succeeded})
		}
	}
	observed, failures, successes := baseline.observed, baseline.failures, baseline.successes
	lastObservedAt, lastObservedJob := baseline.lastObservedAt, baseline.lastObservedJob
	for _, result := range results {
		observed, failures, successes = advanceObservedState(observed, failures, successes, result.succeeded, failureLimit, recoveryLimit)
		if err = queries.UpsertMonitorResultEvaluation(ctx, database.UpsertMonitorResultEvaluationParams{MonitorID: monitorID, JobID: result.jobID, ScheduledAt: databaseTimestamp(result.scheduledAt), Succeeded: result.succeeded, ObservedState: observed, ConsecutiveFailures: int16(failures), ConsecutiveSuccesses: int16(successes)}); err != nil {
			return false, err
		}
		scheduledCopy, jobCopy := result.scheduledAt.UTC(), result.jobID
		lastObservedAt, lastObservedJob = &scheduledCopy, &jobCopy
	}

	var newestExpected *time.Time
	newest, newestErr := queries.GetNewestExpectedMonitorSlot(ctx, database.GetNewestExpectedMonitorSlotParams{EvaluatedAt: databaseTimestamp(now), MonitorID: monitorID})
	if newestErr == nil {
		newestTime := newest.Time
		newestExpected = &newestTime
	} else if !errors.Is(newestErr, pgx.ErrNoRows) {
		return false, newestErr
	}
	display := "unknown"
	var evaluatedJob *string
	if newestExpected != nil {
		var evaluatedState, job string
		evaluation, evaluationErr := queries.GetMonitorEvaluationAtSlot(ctx, database.GetMonitorEvaluationAtSlotParams{MonitorID: monitorID, ScheduledAt: databaseTimestamp(*newestExpected)})
		if evaluationErr == nil {
			evaluatedState, job = evaluation.ObservedState, evaluation.JobID
			display = evaluatedState
			evaluatedJob = &job
		} else if !errors.Is(evaluationErr, pgx.ErrNoRows) {
			return false, evaluationErr
		}
	}
	if observed == "" {
		observed = "unknown"
	}
	err = queries.UpdateMonitorReliabilityState(ctx, database.UpdateMonitorReliabilityStateParams{DisplayState: display, ObservedState: observed, ConsecutiveFailures: int16(failures), ConsecutiveSuccesses: int16(successes), LastObservedScheduledAt: optionalDatabaseTimestamp(lastObservedAt), LastObservedJobID: optionalDatabaseString(lastObservedJob), NewestExpectedScheduledAt: optionalDatabaseTimestamp(newestExpected), LastEvaluatedJobID: optionalDatabaseString(evaluatedJob), MonitorID: monitorID})
	if err != nil {
		return false, err
	}
	if newestExpected != nil {
		err = queries.UpsertMonitorEvaluationPosition(ctx, database.UpsertMonitorEvaluationPositionParams{MonitorID: monitorID, LastScheduledAt: databaseTimestamp(*newestExpected), LastJobID: optionalDatabaseString(evaluatedJob)})
		if err != nil {
			return false, err
		}
	}
	if correction {
		err = queries.InsertMonitorStateCorrection(ctx, database.InsertMonitorStateCorrectionParams{MonitorID: monitorID, AcceptedJobID: accepted.jobID, CorrectedFrom: databaseTimestamp(accepted.scheduled), PreviousDisplayState: current.display, CorrectedDisplayState: display})
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
	ids, err := database.New(tx).ListMonitorsDueForStateRefresh(ctx, database.ListMonitorsDueForStateRefreshParams{EvaluatedAt: databaseTimestamp(now.UTC()), ResultLimit: int32(limit)})
	if err != nil {
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
