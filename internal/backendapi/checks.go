package backendapi

import (
	"context"
	"time"
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
	rows, err := tx.Query(ctx, `SELECT job_id::text,job_type,scheduled_at,started_at,completed_at,succeeded,status_code,error_category,total_duration_microseconds FROM health_checks WHERE monitor_id=$1::uuid AND scheduled_at>=$2 AND scheduled_at<$3 AND ($4='' OR job_type=$4) AND ($5::timestamptz IS NULL OR (scheduled_at,job_id)<($5,$6::uuid)) ORDER BY scheduled_at DESC,job_id DESC LIMIT $7`, monitorID, q.From, q.To, q.JobType, nullableTime(cursorTime), nullableString(cursorID), q.Limit+1)
	if err != nil {
		return CheckPage{}, err
	}
	defer rows.Close()
	items := []Check{}
	for rows.Next() {
		var v Check
		if err = rows.Scan(&v.JobID, &v.JobType, &v.ScheduledAt, &v.StartedAt, &v.CompletedAt, &v.Succeeded, &v.StatusCode, &v.ErrorCategory, &v.TotalDurationMicroseconds); err != nil {
			return CheckPage{}, err
		}
		if v.JobType == "manual_test" {
			v.JobType = "manual"
		}
		items = append(items, v)
	}
	if err = rows.Err(); err != nil {
		return CheckPage{}, err
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
	var average *float64
	if err = tx.QueryRow(ctx, `SELECT avg(total_duration_microseconds)::float8/1000 FROM health_checks WHERE monitor_id=$1::uuid AND job_type='scheduled' AND scheduled_at>=$2 AND scheduled_at<$3`, monitorID, q.From, q.To).Scan(&average); err != nil {
		return report, err
	}
	report.AverageLatencyMilliseconds = average
	var invalidated *time.Time
	if err = tx.QueryRow(ctx, `SELECT min(invalidated_at) FROM monitor_rollup_invalidations WHERE monitor_id=$1::uuid AND bucket_start<$3 AND bucket_start+CASE bucket_kind WHEN 'hourly' THEN INTERVAL '1 hour' ELSE INTERVAL '1 day' END>$2`, monitorID, q.From, q.To).Scan(&invalidated); err != nil {
		return report, err
	}
	if invalidated != nil {
		report.Fresh = false
		report.CorrectedAt = invalidated
	}
	return report, nil
}
