// Package monitor implements tenant-scoped monitor configuration and result
// reads. It does not execute network requests.
package monitor

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/destination"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
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

// NewService constructs a monitor service backed by PostgreSQL.
func NewService(db databaseConnection) *Service {
	return &Service{db: db}
}

func NewServiceWithHeaders(db databaseConnection, headers *secureheaders.Keyring) *Service {
	return &Service{db: db, headers: headers}
}

func NewServiceWithQueue(db databaseConnection, headers *secureheaders.Keyring, signingKey ed25519.PrivateKey, signingKeyID string) *Service {
	return &Service{db: db, headers: headers, signingKey: signingKey, signingKeyID: signingKeyID}
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

	monitorDetail := monitorFromGetRow(storedMonitor)
	monitorDetail.HeaderNames = s.headerNames(storedMonitor.HeadersCiphertext, storedMonitor.HeaderKeyVersion)
	return Detail{
		Monitor:       monitorDetail,
		State:         state,
		RecentResults: results,
	}, nil
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

// Update replaces all configurable values and increments the immutable
// monitor version used by future jobs. Header values are never returned.
func (s *Service) Update(ctx context.Context, userID, environmentID, monitorID string, input UpdateInput) (Monitor, error) {
	normalized, err := normalizeCreateInput(userID, environmentID, input)
	if err != nil || !uuidPattern.MatchString(monitorID) {
		return Monitor{}, ErrInvalidInput
	}
	tx, row, err := s.lockManaged(ctx, userID, environmentID, monitorID)
	if err != nil {
		return Monitor{}, err
	}
	defer tx.Rollback(context.Background())
	ciphertext, keyVersion, names, err := s.encryptHeaders(normalized.Headers)
	if err != nil {
		return Monitor{}, err
	}
	result := Monitor{}
	var pausedAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `UPDATE monitors SET name=$1,target_url=$2,method=$3,interval_seconds=$4::integer,
timeout_seconds=$5,expected_status_min=$6,expected_status_max=$7,headers_ciphertext=$8,
header_key_version=$9,worker_pool_id=$10,version=version+1,updated_at=CURRENT_TIMESTAMP,
next_check_at=CASE WHEN paused_at IS NULL THEN CURRENT_TIMESTAMP + mod(hashtextextended(id::text,0) & 2147483647,$4::bigint)*INTERVAL '1 second' ELSE next_check_at END
WHERE organization_id=$11::uuid AND environment_id=$12::uuid AND id=$13::uuid AND deleted_at IS NULL
RETURNING id::text,organization_id::text,environment_id::text,name,target_url,method,
interval_seconds,timeout_seconds,expected_status_min,expected_status_max,version,paused_at,
worker_pool_id,created_at,updated_at`, normalized.Name, normalized.TargetURL, normalized.Method,
		normalized.IntervalSeconds, normalized.TimeoutSeconds, normalized.ExpectedStatusMin, normalized.ExpectedStatusMax,
		ciphertext, nullableInt32(keyVersion), normalized.WorkerPoolID, row.OrganizationID, row.EnvironmentID, row.ID).Scan(
		&result.ID, &result.OrganizationID, &result.EnvironmentID, &result.Name, &result.TargetURL, &result.Method,
		&result.IntervalSeconds, &result.TimeoutSeconds, &result.ExpectedStatusMin, &result.ExpectedStatusMax,
		&result.Version, &pausedAt, &result.WorkerPoolID, &result.CreatedAt, &result.UpdatedAt)
	if err != nil {
		return Monitor{}, fmt.Errorf("update monitor: %w", err)
	}
	result.Paused = pausedAt.Valid
	result.HeaderNames = names
	if _, err = tx.Exec(ctx, `UPDATE monitor_schedule_periods SET ends_at=CURRENT_TIMESTAMP WHERE monitor_id=$1::uuid AND ends_at IS NULL`, row.ID); err != nil {
		return Monitor{}, fmt.Errorf("close monitor schedule period: %w", err)
	}
	if !result.Paused {
		if _, err = tx.Exec(ctx, `INSERT INTO monitor_schedule_periods(organization_id,environment_id,monitor_id,monitor_version,interval_seconds,worker_pool_id,starts_at,first_slot_at)
SELECT organization_id,environment_id,id,version,interval_seconds,worker_pool_id,CURRENT_TIMESTAMP,next_check_at FROM monitors WHERE id=$1::uuid`, row.ID); err != nil {
			return Monitor{}, fmt.Errorf("record monitor schedule period: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Monitor{}, fmt.Errorf("commit monitor update: %w", err)
	}
	return result, nil
}

func (s *Service) Pause(ctx context.Context, userID, environmentID, monitorID string) (Monitor, error) {
	return s.setPaused(ctx, userID, environmentID, monitorID, true)
}
func (s *Service) Resume(ctx context.Context, userID, environmentID, monitorID string) (Monitor, error) {
	return s.setPaused(ctx, userID, environmentID, monitorID, false)
}
func (s *Service) setPaused(ctx context.Context, userID, environmentID, monitorID string, paused bool) (Monitor, error) {
	tx, row, err := s.lockManaged(ctx, userID, environmentID, monitorID)
	if err != nil {
		return Monitor{}, err
	}
	defer tx.Rollback(context.Background())
	result := Monitor{}
	var pausedAt pgtype.Timestamptz
	var ciphertext []byte
	var keyVersion pgtype.Int4
	err = tx.QueryRow(ctx, `UPDATE monitors SET paused_at=CASE WHEN $1 THEN CURRENT_TIMESTAMP ELSE NULL END,
next_check_at=CASE WHEN $1 THEN next_check_at ELSE CURRENT_TIMESTAMP + mod(hashtextextended(id::text,0) & 2147483647,interval_seconds::bigint)*INTERVAL '1 second' END,version=version+1,updated_at=CURRENT_TIMESTAMP
WHERE organization_id=$2::uuid AND environment_id=$3::uuid AND id=$4::uuid AND deleted_at IS NULL
RETURNING id::text,organization_id::text,environment_id::text,name,target_url,method,interval_seconds,
timeout_seconds,expected_status_min,expected_status_max,version,paused_at,worker_pool_id,headers_ciphertext,
header_key_version,created_at,updated_at`, paused, row.OrganizationID, row.EnvironmentID, row.ID).Scan(&result.ID, &result.OrganizationID, &result.EnvironmentID, &result.Name, &result.TargetURL, &result.Method, &result.IntervalSeconds, &result.TimeoutSeconds, &result.ExpectedStatusMin, &result.ExpectedStatusMax, &result.Version, &pausedAt, &result.WorkerPoolID, &ciphertext, &keyVersion, &result.CreatedAt, &result.UpdatedAt)
	if err != nil {
		return Monitor{}, fmt.Errorf("change monitor state: %w", err)
	}
	result.Paused = pausedAt.Valid
	result.HeaderNames = s.headerNames(ciphertext, keyVersion)
	if _, err = tx.Exec(ctx, `UPDATE monitor_schedule_periods SET ends_at=CURRENT_TIMESTAMP WHERE monitor_id=$1::uuid AND ends_at IS NULL`, row.ID); err != nil {
		return Monitor{}, fmt.Errorf("close monitor schedule period: %w", err)
	}
	if !paused {
		if _, err = tx.Exec(ctx, `INSERT INTO monitor_schedule_periods(organization_id,environment_id,monitor_id,monitor_version,interval_seconds,worker_pool_id,starts_at,first_slot_at)
SELECT organization_id,environment_id,id,version,interval_seconds,worker_pool_id,CURRENT_TIMESTAMP,next_check_at FROM monitors WHERE id=$1::uuid`, row.ID); err != nil {
			return Monitor{}, fmt.Errorf("record monitor schedule period: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Monitor{}, fmt.Errorf("commit monitor state: %w", err)
	}
	return result, nil
}

func (s *Service) Delete(ctx context.Context, userID, environmentID, monitorID string) error {
	tx, row, err := s.lockManaged(ctx, userID, environmentID, monitorID)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `UPDATE monitors SET deleted_at=CURRENT_TIMESTAMP,version=version+1,updated_at=CURRENT_TIMESTAMP WHERE organization_id=$1::uuid AND environment_id=$2::uuid AND id=$3::uuid AND deleted_at IS NULL`, row.OrganizationID, row.EnvironmentID, row.ID)
	if err != nil {
		return fmt.Errorf("delete monitor: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrMonitorNotFound
	}
	if _, err = tx.Exec(ctx, `UPDATE monitor_schedule_periods SET ends_at=CURRENT_TIMESTAMP WHERE monitor_id=$1::uuid AND ends_at IS NULL`, row.ID); err != nil {
		return fmt.Errorf("close monitor schedule period: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *Service) TestNow(ctx context.Context, userID, environmentID, monitorID string) (string, error) {
	if len(s.signingKey) != ed25519.PrivateKeySize || s.signingKeyID == "" || s.headers == nil {
		return "", ErrQueueUnavailable
	}
	tx, row, err := s.lockManaged(ctx, userID, environmentID, monitorID)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(742019205)`); err != nil {
		return "", err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM check_jobs WHERE job_type='manual_test' AND state IN ('pending','pending_publish','published','running')`).Scan(&count); err != nil {
		return "", err
	}
	if count >= 10 {
		return "", ErrManualQueueFull
	}
	var scheduledPressure bool
	if err = tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM monitors WHERE paused_at IS NULL AND deleted_at IS NULL AND next_check_at < CURRENT_TIMESTAMP - INTERVAL '30 seconds')
		OR (SELECT count(*) >= 900 FROM check_jobs WHERE job_type='scheduled' AND state IN ('pending','pending_publish','published','running'))`).Scan(&scheduledPressure); err != nil {
		return "", err
	}
	if scheduledPressure {
		return "", ErrManualQueueFull
	}
	var encryptionKeyID, queueURL string
	var workerPublic []byte
	var networkPolicy int
	err = tx.QueryRow(ctx, `SELECT encryption_key_id,encryption_public_key,network_policy_version,job_queue_url FROM worker_pools WHERE id=$1 AND enabled AND lifecycle_state='active' AND schema_min <= $2 AND schema_max >= $2 FOR SHARE`, row.WorkerPoolID, envelope.SchemaVersion).Scan(&encryptionKeyID, &workerPublic, &networkPolicy, &queueURL)
	if errors.Is(err, pgx.ErrNoRows) || len(workerPublic) != 32 || encryptionKeyID == "" || queueURL == "" {
		return "", ErrQueueUnavailable
	}
	if err != nil {
		return "", err
	}
	workerKey, err := ecdh.X25519().NewPublicKey(workerPublic)
	if err != nil {
		return "", ErrQueueUnavailable
	}
	headers, err := s.headers.Decrypt(row.Headers, row.HeaderVersion.Int32)
	if err != nil {
		return "", ErrQueueUnavailable
	}
	var id string
	var scheduledAt time.Time
	if err = tx.QueryRow(ctx, `SELECT gen_random_uuid()::text,CURRENT_TIMESTAMP`).Scan(&id, &scheduledAt); err != nil {
		return "", err
	}
	expiresAt := scheduledAt.Add(2 * time.Minute)
	body, attrs, err := envelope.SealJob(envelope.Job{SchemaVersion: envelope.SchemaVersion, JobID: id, JobType: "manual_test", WorkerPoolID: row.WorkerPoolID, NetworkPolicyVersion: networkPolicy, ScheduledAt: scheduledAt, ExpiresAt: expiresAt, TargetURL: row.TargetURL, Method: row.Method, TimeoutSeconds: row.Timeout, ExpectedStatusMin: row.Min, ExpectedStatusMax: row.Max, Headers: headers, Limits: envelope.RequestLimits{MaxResponseBytes: 65536, MaxHeaderBytes: 32768, MaxRedirects: 3}, PlatformKeyID: s.signingKeyID, WorkerEncryptionKeyID: encryptionKeyID}, s.signingKey, workerKey)
	if err != nil {
		return "", ErrQueueUnavailable
	}
	hash, err := hex.DecodeString(attrs.SnapshotHash)
	if err != nil {
		return "", err
	}
	err = tx.QueryRow(ctx, `INSERT INTO check_jobs(id,organization_id,environment_id,monitor_id,job_type,state,scheduled_at,monitor_version,worker_pool_id,snapshot_hash,expires_at) VALUES($1::uuid,$2::uuid,$3::uuid,$4::uuid,'manual_test','pending_publish',$5,$6,$7,$8,$9) RETURNING id::text`, id, row.OrganizationID, row.EnvironmentID, row.ID, scheduledAt, row.Version, row.WorkerPoolID, hash, expiresAt).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create manual job: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO check_dispatch_outbox(job_id,worker_pool_id,queue_url,message_body,schema_version,platform_key_id,worker_encryption_key_id,snapshot_hash,message_deduplication_id,message_group_id,expires_at) VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8,$1,$1,$9)`, id, row.WorkerPoolID, queueURL, body, envelope.SchemaVersion, s.signingKeyID, encryptionKeyID, hash, expiresAt); err != nil {
		return "", fmt.Errorf("create manual dispatch: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

type managedMonitor struct {
	ID, OrganizationID, EnvironmentID, WorkerPoolID string
	TargetURL, Method                               string
	Version                                         int64
	Timeout                                         int32
	Min, Max                                        int16
	Headers                                         []byte
	HeaderVersion                                   pgtype.Int4
}

func (s *Service) lockManaged(ctx context.Context, userID, environmentID, monitorID string) (pgx.Tx, managedMonitor, error) {
	if !uuidPattern.MatchString(userID) || !uuidPattern.MatchString(environmentID) || !uuidPattern.MatchString(monitorID) {
		return nil, managedMonitor{}, ErrMonitorNotFound
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, managedMonitor{}, err
	}
	var row managedMonitor
	var role string
	err = tx.QueryRow(ctx, `SELECT m.id::text,m.organization_id::text,m.environment_id::text,m.version,m.worker_pool_id,m.target_url,m.method,m.timeout_seconds,m.expected_status_min,m.expected_status_max,m.headers_ciphertext,m.header_key_version,om.role FROM monitors m JOIN org_members om ON om.organization_id=m.organization_id AND om.user_id=$1::uuid WHERE m.environment_id=$2::uuid AND m.id=$3::uuid AND m.deleted_at IS NULL FOR UPDATE OF m`, userID, environmentID, monitorID).Scan(&row.ID, &row.OrganizationID, &row.EnvironmentID, &row.Version, &row.WorkerPoolID, &row.TargetURL, &row.Method, &row.Timeout, &row.Min, &row.Max, &row.Headers, &row.HeaderVersion, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		tx.Rollback(ctx)
		return nil, row, ErrMonitorNotFound
	}
	if err != nil {
		tx.Rollback(ctx)
		return nil, row, err
	}
	if !authorization.Allows(authorization.Role(role), authorization.PermissionMonitorsManage) {
		tx.Rollback(ctx)
		return nil, row, ErrForbidden
	}
	return tx, row, nil
}
func (s *Service) headerNames(ciphertext []byte, version pgtype.Int4) []string {
	if len(ciphertext) == 0 || !version.Valid || s.headers == nil {
		return []string{}
	}
	headers, err := s.headers.Decrypt(ciphertext, version.Int32)
	if err != nil {
		return []string{}
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func nullableInt32(value int32) any {
	if value == 0 {
		return nil
	}
	return value
}
func (s *Service) encryptHeaders(headers map[string]string) ([]byte, int32, []string, error) {
	if len(headers) == 0 {
		return nil, 0, []string{}, nil
	}
	if s.headers == nil {
		return nil, 0, nil, ErrInvalidInput
	}
	ciphertext, version, err := s.headers.Encrypt(headers)
	if err != nil {
		return nil, 0, nil, ErrInvalidInput
	}
	normalized, _ := secureheaders.Normalize(headers)
	names := make([]string, 0, len(normalized))
	for name := range normalized {
		names = append(names, name)
	}
	sort.Strings(names)
	return ciphertext, version, names, nil
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
