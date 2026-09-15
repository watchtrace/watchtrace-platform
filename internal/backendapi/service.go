// Package backendapi implements the bounded customer-facing Phase 1 read model.
package backendapi

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/incident"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
	"github.com/watchtrace/watchtrace-platform/internal/reliability"
)

var (
	ErrNotFound     = errors.New("resource not found")
	ErrForbidden    = errors.New("permission denied")
	ErrInvalidQuery = errors.New("invalid bounded query")
)

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
}
type Service struct {
	db          DB
	incidents   *incident.Service
	reliability *reliability.Service
}

func New(db DB) *Service {
	return &Service{db: db, incidents: incident.NewService(db), reliability: reliability.New(db)}
}

type PageQuery struct {
	Limit                   int
	From, To                time.Time
	Cursor, Status, JobType string
}
type Check struct {
	JobID                     string    `json:"job_id"`
	JobType                   string    `json:"job_type"`
	ScheduledAt               time.Time `json:"scheduled_at"`
	StartedAt                 time.Time `json:"started_at"`
	CompletedAt               time.Time `json:"completed_at"`
	Succeeded                 bool      `json:"succeeded"`
	StatusCode                *int16    `json:"status_code"`
	ErrorCategory             *string   `json:"error_category"`
	TotalDurationMicroseconds int64     `json:"total_duration_us"`
}
type CheckPage struct {
	Items      []Check `json:"items"`
	NextCursor *string `json:"next_cursor"`
}
type Report struct {
	From                       time.Time  `json:"from"`
	To                         time.Time  `json:"to"`
	Expected                   int64      `json:"expected"`
	Observed                   int64      `json:"observed"`
	Successful                 int64      `json:"successful"`
	Unknown                    int64      `json:"unknown"`
	ObservedUptime             *float64   `json:"observed_uptime"`
	Coverage                   *float64   `json:"coverage"`
	AverageLatencyMilliseconds *float64   `json:"average_latency_ms"`
	Fresh                      bool       `json:"fresh"`
	CorrectedAt                *time.Time `json:"corrected_at"`
}
type StateCounts struct {
	Healthy  int64 `json:"healthy"`
	Degraded int64 `json:"degraded"`
	Down     int64 `json:"down"`
	Unknown  int64 `json:"unknown"`
}
type Dashboard struct {
	States        StateCounts `json:"states"`
	Reliability   Report      `json:"reliability"`
	OpenIncidents int64       `json:"open_incidents"`
	GeneratedAt   time.Time   `json:"generated_at"`
}
type IncidentEvent struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	ActorUserID *string   `json:"actor_user_id"`
	SourceJobID *string   `json:"source_job_id"`
	Reason      *string   `json:"reason"`
	OccurredAt  time.Time `json:"occurred_at"`
}
type Delivery struct {
	ID             string     `json:"id"`
	Transition     string     `json:"transition"`
	State          string     `json:"state"`
	Attempts       int16      `json:"attempts"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	ProviderStatus *string    `json:"provider_status"`
	AcceptedAt     *time.Time `json:"accepted_at"`
	FailedAt       *time.Time `json:"failed_at"`
}
type IncidentSummary struct {
	Incident   incident.Incident `json:"incident"`
	Events     []IncidentEvent   `json:"events"`
	Deliveries []Delivery        `json:"deliveries"`
}
type IncidentPage struct {
	Items      []incident.Incident `json:"items"`
	NextCursor *string             `json:"next_cursor"`
}

func normalizeQuery(q PageQuery, maxWindow time.Duration) (PageQuery, error) {
	if q.Limit == 0 {
		q.Limit = 50
	}
	if q.Limit < 1 || q.Limit > 100 {
		return q, ErrInvalidQuery
	}
	q.From, q.To = q.From.UTC(), q.To.UTC()
	if q.From.IsZero() {
		q.From = time.Now().UTC().Add(-24 * time.Hour)
	}
	if q.To.IsZero() {
		q.To = time.Now().UTC()
	}
	if !q.To.After(q.From) || q.To.Sub(q.From) > maxWindow {
		return q, ErrInvalidQuery
	}
	return q, nil
}
func (s *Service) authorizeEnvironment(ctx context.Context, tx pgx.Tx, userID, environmentID string, permission authorization.Permission) (string, authorization.Role, error) {
	row, err := database.New(tx).GetAccessibleEnvironmentOrganization(ctx, database.GetAccessibleEnvironmentOrganizationParams{UserID: userID, EnvironmentID: environmentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	role := authorization.Role(row.Role)
	if !authorization.Allows(role, permission) {
		return "", "", ErrForbidden
	}
	return row.OrganizationID, role, nil
}
func authorizeMonitor(ctx context.Context, tx pgx.Tx, userID, environmentID, monitorID string) (string, error) {
	row, err := database.New(tx).AuthorizeBackendMonitor(ctx, database.AuthorizeBackendMonitorParams{UserID: userID, EnvironmentID: environmentID, MonitorID: monitorID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if !authorization.Allows(authorization.Role(row.Role), authorization.PermissionMonitorsRead) {
		return "", ErrForbidden
	}
	return row.OrganizationID, nil
}
