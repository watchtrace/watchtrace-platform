// Package monitor implements tenant-scoped monitor configuration and result
// reads. It does not execute network requests.
package monitor

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/destination"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
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
	ErrMonitorNotFound  = errors.New("monitor not found")
	ErrManualQueueFull  = errors.New("manual check queue limit reached")
	ErrQueueUnavailable = errors.New("monitor queue unavailable")
	ErrForbidden        = errors.New("permission denied")
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
	Method            string
	Headers           map[string]string
	WorkerPoolID      string
}

type UpdateInput = CreateInput

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
	Version           int64
	Paused            bool
	WorkerPoolID      string
	HeaderNames       []string
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
	db           databaseConnection
	headers      *secureheaders.Keyring
	signingKey   ed25519.PrivateKey
	signingKeyID string
}

// Config contains the cryptographic dependencies needed by every monitor use
// case, including encrypted headers and manual test dispatch.
type Config struct {
	Headers      *secureheaders.Keyring
	SigningKey   ed25519.PrivateKey
	SigningKeyID string
}

// NewService constructs a fully configured monitor service.
func NewService(db databaseConnection, config Config) (*Service, error) {
	config.SigningKeyID = strings.TrimSpace(config.SigningKeyID)
	if db == nil || config.Headers == nil || len(config.SigningKey) != ed25519.PrivateKeySize || config.SigningKeyID == "" {
		return nil, errors.New("monitor: database, header keys, signing key, and signing key ID are required")
	}
	return &Service{
		db:           db,
		headers:      config.Headers,
		signingKey:   append(ed25519.PrivateKey(nil), config.SigningKey...),
		signingKeyID: config.SigningKeyID,
	}, nil
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
	authorized, err := queries.LockEnvironmentForMonitorCreation(ctx, database.LockEnvironmentForMonitorCreationParams{
		UserID:        userID,
		EnvironmentID: environmentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Monitor{}, ErrEnvironmentNotFound
	}
	if err != nil {
		return Monitor{}, fmt.Errorf("authorize monitor environment: %w", err)
	}
	if !authorization.Allows(authorization.Role(authorized.Role), authorization.PermissionMonitorsManage) {
		return Monitor{}, ErrForbidden
	}
	organizationID := authorized.OrganizationID
	ciphertext, keyVersion, headerNames, err := s.encryptHeaders(normalized.Headers)
	if err != nil {
		return Monitor{}, err
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
	if _, err := tx.Exec(ctx, `UPDATE monitors SET method=$1, headers_ciphertext=$2,
header_key_version=$3, worker_pool_id=$4,
next_check_at=CURRENT_TIMESTAMP + mod(hashtextextended(id::text,0) & 2147483647,interval_seconds::bigint)*INTERVAL '1 second'
WHERE id=$5::uuid`, normalized.Method,
		ciphertext, nullableInt32(keyVersion), normalized.WorkerPoolID, created.ID); err != nil {
		return Monitor{}, fmt.Errorf("store secure monitor configuration: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO monitor_schedule_periods(organization_id,environment_id,monitor_id,monitor_version,interval_seconds,worker_pool_id,starts_at,first_slot_at)
SELECT organization_id,environment_id,id,version,interval_seconds,worker_pool_id,CURRENT_TIMESTAMP,next_check_at FROM monitors WHERE id=$1::uuid`, created.ID); err != nil {
		return Monitor{}, fmt.Errorf("record monitor schedule period: %w", err)
	}
	if err = recordRefresh(ctx, tx, organizationID, environmentID, "monitor.changed", "monitor", created.ID); err != nil {
		return Monitor{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Monitor{}, fmt.Errorf("commit monitor transaction: %w", err)
	}

	result := monitorFromCreateRow(created)
	result.Method = normalized.Method
	result.Version = 1
	result.WorkerPoolID = normalized.WorkerPoolID
	result.HeaderNames = headerNames
	return result, nil
}

func normalizeCreateInput(userID, environmentID string, input CreateInput) (CreateInput, error) {
	normalized := input
	normalized.Name = strings.TrimSpace(input.Name)
	normalized.TargetURL = strings.TrimSpace(input.TargetURL)
	normalized.Method = strings.ToUpper(strings.TrimSpace(input.Method))
	if normalized.Method == "" {
		normalized.Method = "GET"
	}
	normalized.WorkerPoolID = strings.TrimSpace(input.WorkerPoolID)
	if normalized.WorkerPoolID == "" {
		normalized.WorkerPoolID = "hosted"
	}
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
		destination.ValidateURL(normalized.TargetURL) != nil || (normalized.Method != "GET" && normalized.Method != "HEAD") ||
		!regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`).MatchString(normalized.WorkerPoolID) {
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
