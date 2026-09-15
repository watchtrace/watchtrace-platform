// Package reliability computes coverage-aware monitoring summaries, ordered
// monitor state, repeatable rollups, and bounded retention.
package reliability

import (
	"context"
	"errors"
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
