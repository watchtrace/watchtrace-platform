// Package incident owns threshold incident transitions and their durable timeline.
package incident

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/notification"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

var (
	ErrIncidentNotFound = errors.New("incident not found")
	ErrForbidden        = errors.New("incident permission denied")
	ErrAlreadyResolved  = errors.New("incident already resolved")
	ErrInvalidInput     = errors.New("invalid incident input")
)

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
}

type Incident struct {
	ID                   string     `json:"id"`
	OrganizationID       string     `json:"organization_id"`
	EnvironmentID        string     `json:"environment_id"`
	MonitorID            string     `json:"monitor_id"`
	Status               string     `json:"status"`
	StartedAt            time.Time  `json:"started_at"`
	OpenedAt             time.Time  `json:"opened_at"`
	AcknowledgedAt       *time.Time `json:"acknowledged_at"`
	AcknowledgedByUserID *string    `json:"acknowledged_by_user_id"`
	ResolvedAt           *time.Time `json:"resolved_at"`
	ResolvedByUserID     *string    `json:"resolved_by_user_id"`
	ResolutionKind       *string    `json:"resolution_kind"`
	ResolutionReason     *string    `json:"resolution_reason"`
}

type Service struct {
	db  DB
	now func() time.Time
}

type Option func(*Service)

func WithClock(now func() time.Time) Option {
	return func(service *Service) {
		if now != nil {
			service.now = now
		}
	}
}

func NewService(db DB, options ...Option) *Service {
	service := &Service{db: db, now: time.Now}
	for _, option := range options {
		option(service)
	}
	return service
}

// ApplyEvaluationTx applies the current ordered monitor state in the accepted
// result transaction. The monitor reliability row is already locked by the
// caller, while the partial unique index remains the final concurrency guard.
func ApplyEvaluationTx(ctx context.Context, tx pgx.Tx, monitorID, sourceJobID string, corrected bool, now time.Time) error {
	queries := database.New(tx)
	if err := queries.EnsureDefaultAlertRule(ctx, monitorID); err != nil {
		return err
	}
	rule, err := queries.GetEnabledMonitorAlertRule(ctx, monitorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	openID, hasOpen, err := openIncidentID(ctx, tx, monitorID, rule.RuleID)
	if err != nil {
		return err
	}
	if rule.ObservedState == "down" {
		if hasOpen {
			return nil
		}
		startedAt := now.UTC()
		evaluationTimes, evaluationErr := queries.ListRecentIncidentEvaluationTimes(ctx, database.ListRecentIncidentEvaluationTimesParams{MonitorID: monitorID, FailureThreshold: int32(rule.FailureThreshold)})
		if evaluationErr != nil {
			return evaluationErr
		}
		if len(evaluationTimes) > 0 {
			startedAt = evaluationTimes[len(evaluationTimes)-1].Time
		}
		return openIncident(ctx, tx, rule.OrganizationID, rule.EnvironmentID, monitorID, rule.RuleID, sourceJobID, startedAt, now.UTC())
	}
	if corrected && hasOpen {
		return resolveIncident(ctx, tx, openID, sourceJobID, "late_result_correction", nil,
			"late result correction invalidated the open threshold", now.UTC())
	}
	if rule.ObservedState == "healthy" && hasOpen {
		return resolveIncident(ctx, tx, openID, sourceJobID, "automatic_recovery", nil,
			"recovery threshold reached", now.UTC())
	}
	return nil
}

func openIncidentID(ctx context.Context, tx pgx.Tx, monitorID, ruleID string) (string, bool, error) {
	incidentID, err := database.New(tx).LockOpenIncident(ctx, database.LockOpenIncidentParams{MonitorID: monitorID, RuleID: ruleID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return incidentID, err == nil, err
}

func openIncident(ctx context.Context, tx pgx.Tx, organizationID, environmentID, monitorID, ruleID, sourceJobID string, startedAt, now time.Time) error {
	incidentID, err := database.New(tx).CreateOpenIncident(ctx, database.CreateOpenIncidentParams{OrganizationID: organizationID, EnvironmentID: environmentID, MonitorID: monitorID, RuleID: ruleID, StartedAt: databaseTimestamp(startedAt), OpenedAt: databaseTimestamp(now)})
	if errors.Is(err, pgx.ErrNoRows) {
		_, _, err = openIncidentID(ctx, tx, monitorID, ruleID)
		return err
	}
	if err != nil {
		return err
	}
	eventID, err := insertEvent(ctx, tx, organizationID, environmentID, incidentID,
		"opened", "opened", nil, sourceJobID, "failure threshold reached", now)
	if err != nil {
		return err
	}
	_, err = notification.EnqueueIncidentEventTx(ctx, tx, eventID, "opened")
	return err
}

func resolveIncident(ctx context.Context, tx pgx.Tx, incidentID, sourceJobID, kind string, actorUserID *string, reason string, now time.Time) error {
	queries := database.New(tx)
	rowsAffected, err := queries.ResolveOpenIncident(ctx, database.ResolveOpenIncidentParams{ResolvedAt: databaseTimestamp(now), ActorUserID: optionalString(actorUserID), ResolutionKind: pgtype.Text{String: kind, Valid: true}, ResolutionReason: reason, IncidentID: incidentID})
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return nil
	}
	tenant, err := queries.GetIncidentTenant(ctx, incidentID)
	if err != nil {
		return err
	}
	eventID, err := insertEvent(ctx, tx, tenant.OrganizationID, tenant.EnvironmentID, incidentID,
		"resolved", kind, actorUserID, sourceJobID, reason, now)
	if err != nil {
		return err
	}
	_, err = notification.EnqueueIncidentEventTx(ctx, tx, eventID, "resolved")
	return err
}

func insertEvent(ctx context.Context, tx pgx.Tx, organizationID, environmentID, incidentID, eventKey, eventType string, actorUserID *string, sourceJobID, reason string, now time.Time) (string, error) {
	queries := database.New(tx)
	eventID, err := queries.UpsertIncidentEvent(ctx, database.UpsertIncidentEventParams{
		OrganizationID: organizationID, EnvironmentID: environmentID, IncidentID: incidentID,
		EventKey: eventKey, EventType: eventType, ActorUserID: optionalString(actorUserID),
		SourceJobID: strings.TrimSpace(sourceJobID), SafeReason: reason, OccurredAt: databaseTimestamp(now),
	})
	if err != nil {
		return "", err
	}
	err = queries.InsertIncidentRefreshEvent(ctx, database.InsertIncidentRefreshEventParams{OrganizationID: organizationID, EnvironmentID: environmentID, IncidentID: incidentID})
	return eventID, err
}

func (service *Service) Acknowledge(ctx context.Context, userID, environmentID, incidentID, reason string) (Incident, error) {
	if !validActionInput(userID, environmentID, incidentID, reason) {
		return Incident{}, ErrInvalidInput
	}
	tx, err := service.db.Begin(ctx)
	if err != nil {
		return Incident{}, err
	}
	defer tx.Rollback(context.Background())
	incident, role, err := loadAuthorizedIncident(ctx, tx, userID, environmentID, incidentID)
	if err != nil {
		return Incident{}, err
	}
	if !authorization.Allows(role, authorization.PermissionIncidentsManage) {
		return Incident{}, ErrForbidden
	}
	if incident.Status != "open" {
		return Incident{}, ErrAlreadyResolved
	}
	now := service.now().UTC()
	if incident.AcknowledgedAt == nil {
		if err = database.New(tx).AcknowledgeOpenIncident(ctx, database.AcknowledgeOpenIncidentParams{AcknowledgedAt: databaseTimestamp(now), UserID: userID, IncidentID: incidentID}); err != nil {
			return Incident{}, err
		}
		actor := userID
		if _, err = insertEvent(ctx, tx, incident.OrganizationID, incident.EnvironmentID, incident.ID,
			"acknowledged", "acknowledged", &actor, "", reason, now); err != nil {
			return Incident{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Incident{}, err
	}
	return service.Get(ctx, userID, environmentID, incidentID)
}

func (service *Service) Resolve(ctx context.Context, userID, environmentID, incidentID, reason string) (Incident, error) {
	if !validActionInput(userID, environmentID, incidentID, reason) {
		return Incident{}, ErrInvalidInput
	}
	tx, err := service.db.Begin(ctx)
	if err != nil {
		return Incident{}, err
	}
	defer tx.Rollback(context.Background())
	incident, role, err := loadAuthorizedIncident(ctx, tx, userID, environmentID, incidentID)
	if err != nil {
		return Incident{}, err
	}
	if !authorization.Allows(role, authorization.PermissionIncidentsManage) {
		return Incident{}, ErrForbidden
	}
	if incident.Status != "open" {
		return Incident{}, ErrAlreadyResolved
	}
	actor := userID
	if err = resolveIncident(ctx, tx, incidentID, "", "manual_resolution", &actor, reason, service.now().UTC()); err != nil {
		return Incident{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Incident{}, err
	}
	return service.Get(ctx, userID, environmentID, incidentID)
}

func (service *Service) Get(ctx context.Context, userID, environmentID, incidentID string) (Incident, error) {
	if !validActionInput(userID, environmentID, incidentID, "") {
		return Incident{}, ErrInvalidInput
	}
	tx, err := service.db.Begin(ctx)
	if err != nil {
		return Incident{}, err
	}
	defer tx.Rollback(context.Background())
	incident, role, err := loadAuthorizedIncident(ctx, tx, userID, environmentID, incidentID)
	if err != nil {
		return Incident{}, err
	}
	if !authorization.Allows(role, authorization.PermissionIncidentsRead) {
		return Incident{}, ErrForbidden
	}
	return incident, nil
}

func loadAuthorizedIncident(ctx context.Context, tx pgx.Tx, userID, environmentID, incidentID string) (Incident, authorization.Role, error) {
	row, err := database.New(tx).LoadAuthorizedIncident(ctx, database.LoadAuthorizedIncidentParams{UserID: userID, EnvironmentID: environmentID, IncidentID: incidentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, "", ErrIncidentNotFound
	}
	if err != nil {
		return Incident{}, "", fmt.Errorf("load incident: %w", err)
	}
	incident := Incident{ID: row.ID, OrganizationID: row.OrganizationID, EnvironmentID: row.EnvironmentID,
		MonitorID: row.MonitorID, Status: row.Status, StartedAt: row.StartedAt.Time, OpenedAt: row.OpenedAt.Time,
		AcknowledgedAt: optionalTimestamp(row.AcknowledgedAt), AcknowledgedByUserID: optionalUUID(row.AcknowledgedByUserID),
		ResolvedAt: optionalTimestamp(row.ResolvedAt), ResolvedByUserID: optionalUUID(row.ResolvedByUserID),
		ResolutionKind: optionalText(row.ResolutionKind), ResolutionReason: optionalText(row.ResolutionReason)}
	return incident, authorization.Role(row.Role), nil
}

func validActionInput(userID, environmentID, incidentID, reason string) bool {
	return validUUID(userID) && validUUID(environmentID) && validUUID(incidentID) && len(reason) <= 500
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}

func databaseTimestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func optionalTimestamp(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func optionalText(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func optionalUUID(value pgtype.UUID) *string {
	if !value.Valid {
		return nil
	}
	text := uuid.UUID(value.Bytes).String()
	return &text
}
