// Package checker claims durable monitor jobs, executes their HTTP requests,
// and stores one final result per stable job ID.
package checker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/watchtrace/watchtrace-platform/internal/destination"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

const (
	// LeaseDuration remains safely above the maximum ten-second Phase 1
	// monitor timeout.
	LeaseDuration = 60 * time.Second
	// responseDiscardLimit bounds bytes read from a response. Response data is
	// never returned from this package or written to PostgreSQL.
	responseDiscardLimit = 64 * 1024
	userAgent            = "WatchTrace-Phase1/1.0"
)

var (
	// ErrInvalidWorkerID indicates an unsafe or unbounded worker identity.
	ErrInvalidWorkerID = errors.New("invalid checker worker ID")
	// ErrLeaseLost indicates that a job is no longer owned by the completing
	// worker. The result transaction is rolled back in this case.
	ErrLeaseLost = errors.New("checker job lease is no longer current")
)

var workerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type databaseConnection interface {
	database.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Service owns the initial single-job worker path.
type Service struct {
	db     databaseConnection
	client httpDoer
}

// NewService constructs a checker with the guarded production HTTP client.
func NewService(db databaseConnection) *Service {
	return &Service{db: db, client: destination.NewHTTPClient(nil, nil)}
}

// NewServiceWithNetwork constructs a checker with controlled DNS and dial
// dependencies while retaining every destination validation layer. It is used
// by deterministic security and PostgreSQL integration tests.
func NewServiceWithNetwork(
	db databaseConnection,
	resolver destination.Resolver,
	dialer destination.ContextDialer,
) *Service {
	return &Service{db: db, client: destination.NewHTTPClient(resolver, dialer)}
}

// newServiceWithHTTPClient constructs a checker around a controlled client.
// It is intentionally unexported so production composition cannot
// accidentally bypass destination.NewHTTPClient.
func newServiceWithHTTPClient(db databaseConnection, client httpDoer) *Service {
	return &Service{db: db, client: client}
}

// RunNext claims at most one pending job, executes it outside the claim
// transaction, and atomically stores its result with the completed job state.
// The bool reports whether a job was claimed.
func (service *Service) RunNext(ctx context.Context, workerID string) (bool, error) {
	job, err := service.claimNext(ctx, workerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	result, err := service.execute(ctx, job)
	if err != nil {
		// Cancellation by the checker process is an internal interruption. The
		// running row and lease remain durable for later recovery work.
		return true, err
	}
	if err := service.complete(ctx, job, result); err != nil {
		return true, err
	}
	return true, nil
}

type claimedJob struct {
	ID                string
	OrganizationID    string
	EnvironmentID     string
	MonitorID         string
	JobType           string
	ScheduledAt       time.Time
	LeaseToken        string
	TargetURL         string
	Method            string
	TimeoutSeconds    int32
	ExpectedStatusMin int16
	ExpectedStatusMax int16
}

func (service *Service) claimNext(ctx context.Context, workerID string) (claimedJob, error) {
	if !workerIDPattern.MatchString(workerID) {
		return claimedJob{}, ErrInvalidWorkerID
	}

	row, err := database.New(service.db).ClaimPendingCheckJob(
		ctx,
		database.ClaimPendingCheckJobParams{
			LeaseOwner:   pgtype.Text{String: workerID, Valid: true},
			LeaseSeconds: int32(LeaseDuration / time.Second),
		},
	)
	if err != nil {
		return claimedJob{}, fmt.Errorf("claim pending check job: %w", err)
	}
	if !row.ScheduledAt.Valid || !row.StartedAt.Valid ||
		row.LeaseToken == "" || !row.LeaseExpiresAt.Valid {
		return claimedJob{}, errors.New("claimed check job has invalid lifecycle data")
	}

	return claimedJob{
		ID:                row.JobID,
		OrganizationID:    row.OrganizationID,
		EnvironmentID:     row.EnvironmentID,
		MonitorID:         row.MonitorID,
		JobType:           row.JobType,
		ScheduledAt:       row.ScheduledAt.Time,
		LeaseToken:        row.LeaseToken,
		TargetURL:         row.TargetUrl,
		Method:            row.Method,
		TimeoutSeconds:    row.TimeoutSeconds,
		ExpectedStatusMin: row.ExpectedStatusMin,
		ExpectedStatusMax: row.ExpectedStatusMax,
	}, nil
}

type checkResult struct {
	StartedAt              time.Time
	CompletedAt            time.Time
	Succeeded              bool
	StatusCode             int16
	ErrorCategory          string
	TotalDurationMicrosecs int64
}

func (service *Service) execute(ctx context.Context, job claimedJob) (checkResult, error) {
	startedAt := time.Now()
	requestContext, cancel := context.WithTimeout(
		ctx,
		time.Duration(job.TimeoutSeconds)*time.Second,
	)
	defer cancel()

	request, err := http.NewRequestWithContext(requestContext, job.Method, job.TargetURL, nil)
	if err != nil {
		return finishedResult(startedAt, 0, "invalid_target"), nil
	}
	request.Header.Set("User-Agent", userAgent)

	response, requestErr := service.client.Do(request)
	if requestErr != nil {
		if ctx.Err() != nil {
			return checkResult{}, fmt.Errorf("checker request interrupted: %w", ctx.Err())
		}
		return finishedResult(startedAt, 0, categorizeRequestError(requestErr)), nil
	}
	if response == nil {
		return finishedResult(startedAt, 0, "http_protocol"), nil
	}

	statusCode := int16(response.StatusCode)
	bodyErr := discardResponseBody(response.Body)
	if ctx.Err() != nil {
		return checkResult{}, fmt.Errorf("checker response interrupted: %w", ctx.Err())
	}
	if bodyErr != nil {
		return finishedResult(startedAt, statusCode, "response_body"), nil
	}
	if statusCode < job.ExpectedStatusMin || statusCode > job.ExpectedStatusMax {
		return finishedResult(startedAt, statusCode, "unexpected_status"), nil
	}
	return finishedResult(startedAt, statusCode, ""), nil
}

func finishedResult(startedAt time.Time, statusCode int16, category string) checkResult {
	completedAt := time.Now()
	duration := completedAt.Sub(startedAt)
	if duration < 0 {
		duration = 0
	}
	return checkResult{
		StartedAt:              startedAt,
		CompletedAt:            completedAt,
		Succeeded:              category == "",
		StatusCode:             statusCode,
		ErrorCategory:          category,
		TotalDurationMicrosecs: duration.Microseconds(),
	}
}

func discardResponseBody(body io.ReadCloser) error {
	if body == nil {
		return nil
	}
	_, readErr := io.CopyN(io.Discard, body, responseDiscardLimit)
	closeErr := body.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	return closeErr
}

func categorizeRequestError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, destination.ErrInvalidTarget):
		return "invalid_target"
	case errors.Is(err, destination.ErrUnsafeTarget):
		return "unsafe_target"
	case errors.Is(err, destination.ErrResolutionFailed):
		return "dns"
	case errors.Is(err, destination.ErrConnectionFailed):
		return "connection"
	}

	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return "dns"
	}
	var certificateVerificationError *tls.CertificateVerificationError
	var unknownAuthorityError x509.UnknownAuthorityError
	var hostnameError x509.HostnameError
	var certificateInvalidError x509.CertificateInvalidError
	var recordHeaderError tls.RecordHeaderError
	if errors.As(err, &certificateVerificationError) ||
		errors.As(err, &unknownAuthorityError) ||
		errors.As(err, &hostnameError) ||
		errors.As(err, &certificateInvalidError) ||
		errors.As(err, &recordHeaderError) {
		return "tls"
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return "timeout"
		}
		return "connection"
	}
	return "http_protocol"
}

func (service *Service) complete(ctx context.Context, job claimedJob, result checkResult) error {
	tx, err := service.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin check result transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	queries := database.New(tx)
	locked, err := queries.LockCheckJobForCompletion(ctx, database.LockCheckJobForCompletionParams{JobID: job.ID, OrganizationID: job.OrganizationID, EnvironmentID: job.EnvironmentID, MonitorID: job.MonitorID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lock check job for completion: %w", err)
	}
	if locked.State == "completed" {
		exists, err := queries.HealthCheckExists(ctx, database.HealthCheckExistsParams{JobID: job.ID, OrganizationID: job.OrganizationID, EnvironmentID: job.EnvironmentID, MonitorID: job.MonitorID})
		if err != nil {
			return fmt.Errorf("verify completed check result: %w", err)
		}
		if exists {
			return nil
		}
		return ErrLeaseLost
	}
	if locked.State != "running" || locked.LeaseToken != job.LeaseToken {
		return ErrLeaseLost
	}

	statusCode := pgtype.Int2{}
	if result.StatusCode != 0 {
		statusCode = pgtype.Int2{Int16: result.StatusCode, Valid: true}
	}
	errorCategory := pgtype.Text{}
	if strings.TrimSpace(result.ErrorCategory) != "" {
		errorCategory = pgtype.Text{String: result.ErrorCategory, Valid: true}
	}

	inserted, err := queries.InsertHealthCheck(ctx, database.InsertHealthCheckParams{
		JobID:                     locked.JobID,
		OrganizationID:            locked.OrganizationID,
		EnvironmentID:             locked.EnvironmentID,
		MonitorID:                 locked.MonitorID,
		JobType:                   locked.JobType,
		ScheduledAt:               locked.ScheduledAt,
		StartedAt:                 pgtype.Timestamptz{Time: result.StartedAt, Valid: true},
		CompletedAt:               pgtype.Timestamptz{Time: result.CompletedAt, Valid: true},
		Succeeded:                 result.Succeeded,
		StatusCode:                statusCode,
		ErrorCategory:             errorCategory,
		TotalDurationMicroseconds: result.TotalDurationMicrosecs,
	})
	if err != nil {
		return fmt.Errorf("insert health check result: %w", err)
	}
	if inserted < 0 || inserted > 1 {
		return fmt.Errorf("insert health check result affected %d rows", inserted)
	}

	completed, err := queries.CompleteCheckJob(ctx, database.CompleteCheckJobParams{
		CompletedAt:    pgtype.Timestamptz{Time: result.CompletedAt, Valid: true},
		JobID:          locked.JobID,
		LeaseToken:     job.LeaseToken,
		OrganizationID: locked.OrganizationID,
		EnvironmentID:  locked.EnvironmentID,
		MonitorID:      locked.MonitorID,
	})
	if err != nil {
		return fmt.Errorf("complete check job: %w", err)
	}
	if completed != 1 {
		return ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit check result transaction: %w", err)
	}
	return nil
}
