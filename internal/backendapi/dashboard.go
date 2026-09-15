package backendapi

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
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
	queries := database.New(tx)
	states, err := queries.GetEnvironmentMonitorStateCounts(ctx, environmentID)
	if err != nil {
		return d, err
	}
	d.States = StateCounts{Healthy: states.Healthy, Degraded: states.Degraded, Down: states.Down, Unknown: states.Unknown}
	d.OpenIncidents, err = queries.CountOpenEnvironmentIncidents(ctx, environmentID)
	if err != nil {
		return d, err
	}
	result, err := queries.GetEnvironmentReliability(ctx, database.GetEnvironmentReliabilityParams{FromAt: databaseTimestamp(q.From), ToAt: databaseTimestamp(q.To), EnvironmentID: environmentID})
	if err != nil {
		return d, err
	}
	d.Reliability.Expected, d.Reliability.Observed, d.Reliability.Successful = result.Expected, result.Observed, result.Successful
	if result.LatencySamples > 0 {
		average := float64(result.LatencyTotalUs) / float64(result.LatencySamples) / 1000
		d.Reliability.AverageLatencyMilliseconds = &average
	}
	normalized := reliability.Report{Expected: d.Reliability.Expected, Observed: d.Reliability.Observed, Successful: d.Reliability.Successful}.Normalize()
	d.Reliability.From = q.From
	d.Reliability.To = q.To
	d.Reliability.Unknown = normalized.Unknown
	d.Reliability.ObservedUptime = normalized.ObservedUptime
	d.Reliability.Coverage = normalized.Coverage
	d.Reliability.Fresh = true
	invalidated, err := queries.GetFirstEnvironmentRollupInvalidation(ctx, database.GetFirstEnvironmentRollupInvalidationParams{EnvironmentID: environmentID, ToAt: databaseTimestamp(q.To), FromAt: databaseTimestamp(q.From)})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return d, err
	}
	if err == nil {
		d.Reliability.Fresh = false
		d.Reliability.CorrectedAt = &invalidated.Time
	}
	return d, nil
}
