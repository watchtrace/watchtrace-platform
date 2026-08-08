// Package monitor implements tenant-scoped monitor configuration and result
// reads. It does not execute network requests.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/destination"
	"github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

const (
	DefaultIntervalSeconds   = 300
	DefaultTimeoutSeconds    = 5
	DefaultExpectedStatusMin = 200
	DefaultExpectedStatusMax = 299

	maximumMonitorNameBytes = 120
	maximumTargetURLBytes   = 2048
	maximumMonitorsPerOrg   = 100
)

var (
	// ErrInvalidInput indicates that monitor configuration is malformed or
	// outside the bounded Phase 1 values.
	ErrInvalidInput = errors.New("invalid monitor input")
	// ErrEnvironmentNotFound covers both an unknown environment and one outside
	// the authenticated user's organizations to avoid tenant enumeration.
	ErrEnvironmentNotFound = errors.New("environment not found")
	// ErrMonitorLimitReached indicates that the organization already has its
	// documented maximum number of monitors.
	ErrMonitorLimitReached = errors.New("organization monitor limit reached")
	// ErrMonitorNotFound covers an unknown monitor and one outside the
	// authorized organization and environment to avoid tenant enumeration.
	ErrMonitorNotFound = errors.New("monitor not found")
)

var allowedIntervals = map[int32]struct{}{
	60: {}, 120: {}, 300: {}, 600: {}, 1800: {},
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

type databaseConnection interface {
	database.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

// CreateInput contains the configurable fields of an initial GET monitor.
// Zero-valued interval, timeout, and status fields receive documented defaults.
type CreateInput struct {
	Name              string
	TargetURL         string
	IntervalSeconds   int32
	TimeoutSeconds    int32
	ExpectedStatusMin int16
	ExpectedStatusMax int16
}

// Monitor is the safe API representation of stored monitor configuration.
type Monitor struct {
	ID                string
	OrganizationID    string
	EnvironmentID     string
	Name              string
	TargetURL         string
	Method            string
	IntervalSeconds   int32
	TimeoutSeconds    int32
	ExpectedStatusMin int16
	ExpectedStatusMax int16
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// State is the shared customer-facing monitor status vocabulary. P1-401 will
// add the durable consecutive-failure state machine and the down transition.
type State string

const (
	StateUnknown  State = "unknown"
	StateHealthy  State = "healthy"
	StateDegraded State = "degraded"
	StateDown     State = "down"
)

// CheckResult is the bounded, body-free representation of one stored check.
type CheckResult struct {
	JobID                     string
	JobType                   string
	ScheduledAt               time.Time
	StartedAt                 time.Time
	CompletedAt               time.Time
	Succeeded                 bool
	StatusCode                *int16
	ErrorCategory             *string
	TotalDurationMicroseconds int64
}

// Detail combines monitor configuration with its current state and recent
// stored results.
type Detail struct {
	Monitor       Monitor
	State         State
	RecentResults []CheckResult
}

// Service creates and reads tenant-scoped monitors.
type Service struct {
	db databaseConnection
}

// NewService constructs a monitor service backed by PostgreSQL.
func NewService(db databaseConnection) *Service {
	return &Service{db: db}
}

// Create adds one GET monitor after locking its organization so concurrent
// requests cannot exceed the per-organization monitor limit.
func (s *Service) Create(
	ctx context.Context,
	userID string,
	environmentID string,
	input CreateInput,
) (Monitor, error) {
	normalized, err := normalizeCreateInput(userID, environmentID, input)
	if err != nil {
		return Monitor{}, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Monitor{}, fmt.Errorf("begin monitor transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	queries := database.New(tx)
	organizationID, err := queries.LockEnvironmentForMonitorCreation(ctx, database.LockEnvironmentForMonitorCreationParams{
		UserID:        userID,
		EnvironmentID: environmentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Monitor{}, ErrEnvironmentNotFound
	}
	if err != nil {
		return Monitor{}, fmt.Errorf("authorize monitor environment: %w", err)
	}

	monitorCount, err := queries.CountOrganizationMonitors(ctx, organizationID)
	if err != nil {
		return Monitor{}, fmt.Errorf("count organization monitors: %w", err)
	}
	if monitorCount >= maximumMonitorsPerOrg {
		return Monitor{}, ErrMonitorLimitReached
	}

	created, err := queries.CreateMonitor(ctx, database.CreateMonitorParams{
		OrganizationID:    organizationID,
		EnvironmentID:     environmentID,
		Name:              normalized.Name,
		TargetUrl:         normalized.TargetURL,
		IntervalSeconds:   normalized.IntervalSeconds,
		TimeoutSeconds:    normalized.TimeoutSeconds,
		ExpectedStatusMin: normalized.ExpectedStatusMin,
		ExpectedStatusMax: normalized.ExpectedStatusMax,
	})
	if err != nil {
		return Monitor{}, fmt.Errorf("create monitor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Monitor{}, fmt.Errorf("commit monitor transaction: %w", err)
	}

	return monitorFromCreateRow(created), nil
}

// List returns only monitors from an environment in an organization where the
// authenticated user currently has a membership.
func (s *Service) List(ctx context.Context, userID, environmentID string) ([]Monitor, error) {
	if !uuidPattern.MatchString(userID) || !uuidPattern.MatchString(environmentID) {
		return nil, ErrEnvironmentNotFound
	}

	queries := database.New(s.db)
	organizationID, err := queries.GetAccessibleEnvironmentOrganization(
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

	rows, err := queries.ListEnvironmentMonitors(ctx, database.ListEnvironmentMonitorsParams{
		OrganizationID: organizationID,
		EnvironmentID:  environmentID,
	})
	if err != nil {
		return nil, fmt.Errorf("list environment monitors: %w", err)
	}

	monitors := make([]Monitor, 0, len(rows))
	for _, row := range rows {
		monitors = append(monitors, monitorFromListRow(row))
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
	organizationID, err := queries.GetAccessibleEnvironmentOrganization(
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
	latestSucceeded, err := queries.GetLatestScheduledMonitorResult(
		ctx,
		database.GetLatestScheduledMonitorResultParams{
			OrganizationID: organizationID,
			EnvironmentID:  environmentID,
			MonitorID:      monitorID,
		},
	)
	if err == nil {
		state = stateFromLatestScheduledResult(latestSucceeded)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, fmt.Errorf("get latest scheduled monitor result: %w", err)
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

	return Detail{
		Monitor:       monitorFromGetRow(storedMonitor),
		State:         state,
		RecentResults: results,
	}, nil
}

func normalizeCreateInput(userID, environmentID string, input CreateInput) (CreateInput, error) {
	normalized := input
	normalized.Name = strings.TrimSpace(input.Name)
	normalized.TargetURL = strings.TrimSpace(input.TargetURL)
	if normalized.IntervalSeconds == 0 {
		normalized.IntervalSeconds = DefaultIntervalSeconds
	}
	if normalized.TimeoutSeconds == 0 {
		normalized.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if normalized.ExpectedStatusMin == 0 {
		normalized.ExpectedStatusMin = DefaultExpectedStatusMin
	}
	if normalized.ExpectedStatusMax == 0 {
		normalized.ExpectedStatusMax = DefaultExpectedStatusMax
	}

	if !uuidPattern.MatchString(userID) || !uuidPattern.MatchString(environmentID) ||
		normalized.Name == "" || len(normalized.Name) > maximumMonitorNameBytes ||
		len(normalized.TargetURL) > maximumTargetURLBytes ||
		destination.ValidateURL(normalized.TargetURL) != nil {
		return CreateInput{}, ErrInvalidInput
	}
	if _, ok := allowedIntervals[normalized.IntervalSeconds]; !ok {
		return CreateInput{}, ErrInvalidInput
	}
	if normalized.TimeoutSeconds < 1 || normalized.TimeoutSeconds > 10 ||
		normalized.ExpectedStatusMin < 100 || normalized.ExpectedStatusMin > 599 ||
		normalized.ExpectedStatusMax < 100 || normalized.ExpectedStatusMax > 599 ||
		normalized.ExpectedStatusMin > normalized.ExpectedStatusMax {
		return CreateInput{}, ErrInvalidInput
	}

	return normalized, nil
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
