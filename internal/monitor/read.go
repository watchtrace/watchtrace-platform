package monitor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

// List returns only monitors from an environment in an organization where the
// authenticated user currently has a membership.
func (s *Service) List(ctx context.Context, userID, environmentID string) ([]Monitor, error) {
	if !uuidPattern.MatchString(userID) || !uuidPattern.MatchString(environmentID) {
		return nil, ErrEnvironmentNotFound
	}

	queries := database.New(s.db)
	authorized, err := queries.GetAccessibleEnvironmentOrganization(
		ctx,
		database.GetAccessibleEnvironmentOrganizationParams{
			UserID:        userID,
			EnvironmentID: environmentID,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrEnvironmentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("authorize monitor list: %w", err)
	}
	if !authorization.Allows(authorization.Role(authorized.Role), authorization.PermissionMonitorsRead) {
		return nil, ErrEnvironmentNotFound
	}
	organizationID := authorized.OrganizationID

	rows, err := queries.ListEnvironmentMonitors(ctx, database.ListEnvironmentMonitorsParams{
		OrganizationID: organizationID,
		EnvironmentID:  environmentID,
	})
	if err != nil {
		return nil, fmt.Errorf("list environment monitors: %w", err)
	}

	monitors := make([]Monitor, 0, len(rows))
	for _, row := range rows {
		item := monitorFromListRow(row)
		item.HeaderNames = s.headerNames(row.HeadersCiphertext, row.HeaderKeyVersion)
		monitors = append(monitors, item)
	}
	return monitors, nil
}

// Get returns one monitor and at most 20 recent results. Authorization first
// resolves the caller's organization from the environment; every subsequent
// query remains qualified by organization, environment, and monitor.
func (s *Service) Get(ctx context.Context, userID, environmentID, monitorID string) (Detail, error) {
	if !uuidPattern.MatchString(userID) || !uuidPattern.MatchString(environmentID) {
		return Detail{}, ErrEnvironmentNotFound
	}
	if !uuidPattern.MatchString(monitorID) {
		return Detail{}, ErrMonitorNotFound
	}

	queries := database.New(s.db)
	authorized, err := queries.GetAccessibleEnvironmentOrganization(
		ctx,
		database.GetAccessibleEnvironmentOrganizationParams{
			UserID:        userID,
			EnvironmentID: environmentID,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, ErrEnvironmentNotFound
	}
	if err != nil {
		return Detail{}, fmt.Errorf("authorize monitor read: %w", err)
	}
	if !authorization.Allows(authorization.Role(authorized.Role), authorization.PermissionMonitorsRead) {
		return Detail{}, ErrEnvironmentNotFound
	}
	organizationID := authorized.OrganizationID

	storedMonitor, err := queries.GetEnvironmentMonitor(ctx, database.GetEnvironmentMonitorParams{
		OrganizationID: organizationID,
		EnvironmentID:  environmentID,
		MonitorID:      monitorID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, ErrMonitorNotFound
	}
	if err != nil {
		return Detail{}, fmt.Errorf("get environment monitor: %w", err)
	}

	state := StateUnknown
	durable, err := queries.GetDurableMonitorState(ctx, database.GetDurableMonitorStateParams{
		OrganizationID: organizationID, EnvironmentID: environmentID, MonitorID: monitorID,
	})
	if err == nil && durable.LastObservedScheduledAt.Valid {
		state = State(durable.DisplayState)
	} else if err == nil || errors.Is(err, pgx.ErrNoRows) {
		// Compatibility for results created before the ordered evaluator first
		// runs. Once an evaluation exists, an explicit unknown state caused by
		// a missing newest slot must not be replaced by the latest observation.
		latestSucceeded, latestErr := queries.GetLatestScheduledMonitorResult(
			ctx,
			database.GetLatestScheduledMonitorResultParams{
				OrganizationID: organizationID,
				EnvironmentID:  environmentID,
				MonitorID:      monitorID,
			},
		)
		if latestErr == nil {
			state = stateFromLatestScheduledResult(latestSucceeded)
		} else if !errors.Is(latestErr, pgx.ErrNoRows) {
			return Detail{}, fmt.Errorf("get latest scheduled monitor result: %w", latestErr)
		}
	} else {
		return Detail{}, fmt.Errorf("get durable monitor state: %w", err)
	}

	rows, err := queries.ListRecentMonitorResults(ctx, database.ListRecentMonitorResultsParams{
		OrganizationID: organizationID,
		EnvironmentID:  environmentID,
		MonitorID:      monitorID,
	})
	if err != nil {
		return Detail{}, fmt.Errorf("list recent monitor results: %w", err)
	}
	results := make([]CheckResult, 0, len(rows))
	for _, row := range rows {
		results = append(results, checkResultFromRow(row))
	}

	monitorDetail := monitorFromGetRow(storedMonitor)
	monitorDetail.HeaderNames = s.headerNames(storedMonitor.HeadersCiphertext, storedMonitor.HeaderKeyVersion)
	return Detail{
		Monitor:       monitorDetail,
		State:         state,
		RecentResults: results,
	}, nil
}

func monitorFromCreateRow(row database.CreateMonitorRow) Monitor {
	return Monitor{
		ID:                row.ID,
		OrganizationID:    row.OrganizationID,
		EnvironmentID:     row.EnvironmentID,
		Name:              row.Name,
		TargetURL:         row.TargetUrl,
		Method:            row.Method,
		IntervalSeconds:   row.IntervalSeconds,
		TimeoutSeconds:    row.TimeoutSeconds,
		ExpectedStatusMin: row.ExpectedStatusMin,
		ExpectedStatusMax: row.ExpectedStatusMax,
		Version:           row.Version,
		Paused:            row.PausedAt.Valid,
		WorkerPoolID:      row.WorkerPoolID,
		CreatedAt:         row.CreatedAt.Time,
		UpdatedAt:         row.UpdatedAt.Time,
	}
}

func monitorFromListRow(row database.ListEnvironmentMonitorsRow) Monitor {
	return Monitor{
		ID:                row.ID,
		OrganizationID:    row.OrganizationID,
		EnvironmentID:     row.EnvironmentID,
		Name:              row.Name,
		TargetURL:         row.TargetUrl,
		Method:            row.Method,
		IntervalSeconds:   row.IntervalSeconds,
		TimeoutSeconds:    row.TimeoutSeconds,
		ExpectedStatusMin: row.ExpectedStatusMin,
		ExpectedStatusMax: row.ExpectedStatusMax,
		Version:           row.Version,
		Paused:            row.PausedAt.Valid,
		WorkerPoolID:      row.WorkerPoolID,
		CreatedAt:         row.CreatedAt.Time,
		UpdatedAt:         row.UpdatedAt.Time,
	}
}

func monitorFromGetRow(row database.GetEnvironmentMonitorRow) Monitor {
	return Monitor{
		ID:                row.ID,
		OrganizationID:    row.OrganizationID,
		EnvironmentID:     row.EnvironmentID,
		Name:              row.Name,
		TargetURL:         row.TargetUrl,
		Method:            row.Method,
		IntervalSeconds:   row.IntervalSeconds,
		TimeoutSeconds:    row.TimeoutSeconds,
		ExpectedStatusMin: row.ExpectedStatusMin,
		ExpectedStatusMax: row.ExpectedStatusMax,
		Version:           row.Version,
		Paused:            row.PausedAt.Valid,
		WorkerPoolID:      row.WorkerPoolID,
		CreatedAt:         row.CreatedAt.Time,
		UpdatedAt:         row.UpdatedAt.Time,
	}
}

func stateFromLatestScheduledResult(succeeded bool) State {
	if succeeded {
		return StateHealthy
	}
	return StateDegraded
}

func checkResultFromRow(row database.ListRecentMonitorResultsRow) CheckResult {
	var statusCode *int16
	if row.StatusCode.Valid {
		value := row.StatusCode.Int16
		statusCode = &value
	}
	var errorCategory *string
	if row.ErrorCategory.Valid {
		value := row.ErrorCategory.String
		errorCategory = &value
	}
	return CheckResult{
		JobID:                     row.JobID,
		JobType:                   row.JobType,
		ScheduledAt:               row.ScheduledAt.Time,
		StartedAt:                 row.StartedAt.Time,
		CompletedAt:               row.CompletedAt.Time,
		Succeeded:                 row.Succeeded,
		StatusCode:                statusCode,
		ErrorCategory:             errorCategory,
		TotalDurationMicroseconds: row.TotalDurationMicroseconds,
	}
}
