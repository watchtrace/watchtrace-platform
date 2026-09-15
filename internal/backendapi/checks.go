package backendapi

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

func (s *Service) ListChecks(ctx context.Context, userID, environmentID, monitorID string, q PageQuery) (CheckPage, error) {
	q, err := normalizeQuery(q, 31*24*time.Hour)
	if err != nil {
		return CheckPage{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CheckPage{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = authorizeMonitor(ctx, tx, userID, environmentID, monitorID); err != nil {
		return CheckPage{}, err
	}
	var cursorTime time.Time
	var cursorID string
	if q.Cursor != "" {
		cursorTime, cursorID, err = decodeCursor(q.Cursor)
		if err != nil {
			return CheckPage{}, ErrInvalidQuery
		}
	}
	rows, err := database.New(tx).ListBackendChecks(ctx, database.ListBackendChecksParams{
		MonitorID: monitorID, FromAt: databaseTimestamp(q.From), ToAt: databaseTimestamp(q.To),
		JobType: q.JobType, HasCursor: q.Cursor != "", CursorAt: databaseTimestamp(cursorTime),
		CursorID: cursorID, ResultLimit: int32(q.Limit + 1),
	})
	if err != nil {
		return CheckPage{}, err
	}
	items := make([]Check, 0, len(rows))
	for _, row := range rows {
		v := Check{JobID: row.JobID, JobType: row.JobType, ScheduledAt: row.ScheduledAt.Time,
			StartedAt: row.StartedAt.Time, CompletedAt: row.CompletedAt.Time, Succeeded: row.Succeeded,
			StatusCode: optionalInt16(row.StatusCode), ErrorCategory: optionalText(row.ErrorCategory),
			TotalDurationMicroseconds: row.TotalDurationMicroseconds}
		if v.JobType == "manual_test" {
			v.JobType = "manual"
		}
		items = append(items, v)
	}
	page := CheckPage{Items: items}
	if len(items) > q.Limit {
		last := items[q.Limit-1]
		cursor := encodeCursor(last.ScheduledAt, last.JobID)
		page.NextCursor = &cursor
		page.Items = items[:q.Limit]
	}
	return page, nil
}

func (s *Service) MonitorReport(ctx context.Context, userID, environmentID, monitorID string, q PageQuery) (Report, error) {
	q, err := normalizeQuery(q, 366*24*time.Hour)
	if err != nil {
		return Report{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Report{}, err
	}
	if _, err = authorizeMonitor(ctx, tx, userID, environmentID, monitorID); err != nil {
		tx.Rollback(context.Background())
		return Report{}, err
	}
	tx.Rollback(context.Background())
	base, err := s.reliability.Report(ctx, monitorID, q.From, q.To)
	if err != nil {
		return Report{}, err
	}
	report := Report{From: q.From, To: q.To, Expected: base.Expected, Observed: base.Observed, Successful: base.Successful, Unknown: base.Unknown, ObservedUptime: base.ObservedUptime, Coverage: base.Coverage, Fresh: true}
	tx, err = s.db.Begin(ctx)
	if err != nil {
		return report, err
	}
	defer tx.Rollback(context.Background())
	queries := database.New(tx)
	latency, err := queries.GetMonitorLatencyTotals(ctx, database.GetMonitorLatencyTotalsParams{MonitorID: monitorID, FromAt: databaseTimestamp(q.From), ToAt: databaseTimestamp(q.To)})
	if err != nil {
		return report, err
	}
	if latency.SampleCount > 0 {
		average := float64(latency.TotalDurationUs) / float64(latency.SampleCount) / 1000
		report.AverageLatencyMilliseconds = &average
	}
	invalidated, err := queries.GetFirstMonitorRollupInvalidation(ctx, database.GetFirstMonitorRollupInvalidationParams{MonitorID: monitorID, ToAt: databaseTimestamp(q.To), FromAt: databaseTimestamp(q.From)})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return report, err
	}
	if err == nil {
		report.Fresh = false
		report.CorrectedAt = &invalidated.Time
	}
	return report, nil
}
