// Package reliability computes coverage-aware monitoring summaries, ordered
// monitor state, repeatable rollups, and bounded retention.
package reliability

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
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
	row, err := database.New(tx).GetMonitorReliabilityReport(ctx, database.GetMonitorReliabilityReportParams{FromAt: databaseTimestamp(from), ToAt: databaseTimestamp(to), MonitorID: monitorID})
	if err != nil {
		return Report{}, err
	}
	report := Report{Expected: row.Expected, Observed: row.Observed, Successful: row.Successful}
	return report.Normalize(), nil
}
