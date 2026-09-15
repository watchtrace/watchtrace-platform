package backendapi

import (
	"context"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/reliability"
)

func (s *Service) Dashboard(ctx context.Context, userID, environmentID string, q PageQuery) (Dashboard, error) {
	q, err := normalizeQuery(q, 31*24*time.Hour)
	if err != nil {
		return Dashboard{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Dashboard{}, err
	}
	defer tx.Rollback(context.Background())
	if _, _, err = s.authorizeEnvironment(ctx, tx, userID, environmentID, authorization.PermissionMonitorsRead); err != nil {
		return Dashboard{}, err
	}
	var d Dashboard
	d.GeneratedAt = time.Now().UTC()
	err = tx.QueryRow(ctx, `SELECT count(*)FILTER(WHERE COALESCE(r.display_state,'unknown')='healthy'),count(*)FILTER(WHERE COALESCE(r.display_state,'unknown')='degraded'),count(*)FILTER(WHERE COALESCE(r.display_state,'unknown')='down'),count(*)FILTER(WHERE COALESCE(r.display_state,'unknown')='unknown') FROM monitors m LEFT JOIN monitor_reliability_states r ON r.monitor_id=m.id WHERE m.environment_id=$1::uuid AND m.deleted_at IS NULL`, environmentID).Scan(&d.States.Healthy, &d.States.Degraded, &d.States.Down, &d.States.Unknown)
	if err != nil {
		return d, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM incidents WHERE environment_id=$1::uuid AND status='open'`, environmentID).Scan(&d.OpenIncidents); err != nil {
		return d, err
	}
	err = tx.QueryRow(ctx, `WITH expected AS (SELECT p.monitor_id,slot FROM monitor_schedule_periods p CROSS JOIN LATERAL generate_series(p.first_slot_at+GREATEST(0,ceil(EXTRACT(EPOCH FROM ($2-p.first_slot_at))/p.interval_seconds)::bigint)*make_interval(secs=>p.interval_seconds),LEAST(COALESCE(p.ends_at,$3),$3)-INTERVAL '1 microsecond',make_interval(secs=>p.interval_seconds)) slot WHERE p.environment_id=$1::uuid AND p.starts_at<$3 AND COALESCE(p.ends_at,$3)>$2 AND slot>=GREATEST($2,p.starts_at)),a AS(SELECT count(*)::bigint expected,count(h.job_id)::bigint observed,count(h.job_id)FILTER(WHERE h.succeeded)::bigint successful,avg(h.total_duration_microseconds)::float8/1000 latency FROM expected e LEFT JOIN health_checks h ON h.monitor_id=e.monitor_id AND h.job_type='scheduled' AND h.scheduled_at=e.slot) SELECT expected,observed,successful,latency FROM a`, environmentID, q.From, q.To).Scan(&d.Reliability.Expected, &d.Reliability.Observed, &d.Reliability.Successful, &d.Reliability.AverageLatencyMilliseconds)
	if err != nil {
		return d, err
	}
	normalized := reliability.Report{Expected: d.Reliability.Expected, Observed: d.Reliability.Observed, Successful: d.Reliability.Successful}.Normalize()
	d.Reliability.From = q.From
	d.Reliability.To = q.To
	d.Reliability.Unknown = normalized.Unknown
	d.Reliability.ObservedUptime = normalized.ObservedUptime
	d.Reliability.Coverage = normalized.Coverage
	d.Reliability.Fresh = true
	var invalidated *time.Time
	if err = tx.QueryRow(ctx, `SELECT min(i.invalidated_at) FROM monitor_rollup_invalidations i JOIN monitors m ON m.id=i.monitor_id WHERE m.environment_id=$1::uuid AND i.bucket_start<$3 AND i.bucket_start+CASE i.bucket_kind WHEN 'hourly' THEN INTERVAL '1 hour' ELSE INTERVAL '1 day' END>$2`, environmentID, q.From, q.To).Scan(&invalidated); err != nil {
		return d, err
	}
	if invalidated != nil {
		d.Reliability.Fresh = false
		d.Reliability.CorrectedAt = invalidated
	}
	return d, nil
}
