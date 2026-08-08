// Package monitor implements the initial tenant-scoped monitor configuration
// API. It does not execute network requests.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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

// Service creates and lists tenant-scoped monitors.
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
		!validStoredTargetURL(normalized.TargetURL) {
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

func validStoredTargetURL(value string) bool {
	if value == "" {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.Opaque == "" &&
		parsed.User == nil && parsed.Fragment == ""
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
