// Package incident owns threshold incident transitions and their durable timeline.
package incident

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/notification"
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
	ID                   string
	OrganizationID       string
	EnvironmentID        string
	MonitorID            string
	Status               string
	StartedAt            time.Time
	OpenedAt             time.Time
	AcknowledgedAt       *time.Time
	AcknowledgedByUserID *string
	ResolvedAt           *time.Time
	ResolvedByUserID     *string
	ResolutionKind       *string
	ResolutionReason     *string
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
	if _, err := tx.Exec(ctx, `INSERT INTO alert_rules(organization_id,environment_id,monitor_id)
SELECT organization_id,environment_id,id FROM monitors WHERE id=$1::uuid
ON CONFLICT(monitor_id,rule_key) DO NOTHING`, monitorID); err != nil {
		return err
	}
	var ruleID, organizationID, environmentID, observedState string
	var failureThreshold int
	err := tx.QueryRow(ctx, `SELECT a.id::text,a.organization_id::text,a.environment_id::text,
 a.failure_threshold,r.observed_state
FROM alert_rules a JOIN monitor_reliability_states r ON r.monitor_id=a.monitor_id
WHERE a.monitor_id=$1::uuid AND a.rule_key='consecutive_failures' AND a.enabled`, monitorID).Scan(
		&ruleID, &organizationID, &environmentID, &failureThreshold, &observedState)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	openID, hasOpen, err := openIncidentID(ctx, tx, monitorID, ruleID)
	if err != nil {
		return err
	}
	if observedState == "down" {
		if hasOpen {
			return nil
		}
		startedAt := now.UTC()
		if err = tx.QueryRow(ctx, `SELECT COALESCE(min(scheduled_at),$3) FROM (
 SELECT scheduled_at FROM monitor_result_evaluations
 WHERE monitor_id=$1::uuid ORDER BY scheduled_at DESC,job_id DESC LIMIT $2
) recent`, monitorID, failureThreshold, startedAt).Scan(&startedAt); err != nil {
			return err
		}
		return openIncident(ctx, tx, organizationID, environmentID, monitorID, ruleID, sourceJobID, startedAt, now.UTC())
	}
	if corrected && hasOpen {
		return resolveIncident(ctx, tx, openID, sourceJobID, "late_result_correction", nil,
			"late result correction invalidated the open threshold", now.UTC())
	}
	if observedState == "healthy" && hasOpen {
		return resolveIncident(ctx, tx, openID, sourceJobID, "automatic_recovery", nil,
			"recovery threshold reached", now.UTC())
	}
	return nil
}

func openIncidentID(ctx context.Context, tx pgx.Tx, monitorID, ruleID string) (string, bool, error) {
	var incidentID string
	err := tx.QueryRow(ctx, `SELECT id::text FROM incidents
WHERE monitor_id=$1::uuid AND alert_rule_id=$2::uuid AND status='open' FOR UPDATE`, monitorID, ruleID).Scan(&incidentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return incidentID, err == nil, err
}

func openIncident(ctx context.Context, tx pgx.Tx, organizationID, environmentID, monitorID, ruleID, sourceJobID string, startedAt, now time.Time) error {
	var incidentID string
	err := tx.QueryRow(ctx, `INSERT INTO incidents(
 organization_id,environment_id,monitor_id,alert_rule_id,started_at,opened_at)
VALUES($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5,$6)
ON CONFLICT(monitor_id,alert_rule_id) WHERE status='open' DO NOTHING
RETURNING id::text`, organizationID, environmentID, monitorID, ruleID, startedAt, now).Scan(&incidentID)
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
	var organizationID, environmentID string
	tag, err := tx.Exec(ctx, `UPDATE incidents SET status='resolved',resolved_at=$2,
 resolved_by_user_id=$3::uuid,resolution_kind=$4,resolution_reason=$5,updated_at=$2
WHERE id=$1::uuid AND status='open'`, incidentID, now, actorUserID, kind, nullableReason(reason))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if err = tx.QueryRow(ctx, `SELECT organization_id::text,environment_id::text FROM incidents WHERE id=$1::uuid`, incidentID).Scan(&organizationID, &environmentID); err != nil {
		return err
	}
	eventID, err := insertEvent(ctx, tx, organizationID, environmentID, incidentID,
		"resolved", kind, actorUserID, sourceJobID, reason, now)
	if err != nil {
		return err
	}
	_, err = notification.EnqueueIncidentEventTx(ctx, tx, eventID, "resolved")
	return err
}

func insertEvent(ctx context.Context, tx pgx.Tx, organizationID, environmentID, incidentID, eventKey, eventType string, actorUserID *string, sourceJobID, reason string, now time.Time) (string, error) {
	var eventID string
	var source any
	if strings.TrimSpace(sourceJobID) != "" {
		source = sourceJobID
	}
	err := tx.QueryRow(ctx, `INSERT INTO incident_events(
 organization_id,environment_id,incident_id,event_key,event_type,actor_user_id,source_job_id,safe_reason,occurred_at)
VALUES($1::uuid,$2::uuid,$3::uuid,$4,$5,$6::uuid,$7::uuid,$8,$9)
ON CONFLICT(incident_id,event_key) DO UPDATE SET event_key=EXCLUDED.event_key
RETURNING id::text`, organizationID, environmentID, incidentID, eventKey, eventType,
		actorUserID, source, nullableReason(reason), now).Scan(&eventID)
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
		if _, err = tx.Exec(ctx, `UPDATE incidents SET acknowledged_at=$2,acknowledged_by_user_id=$3::uuid,updated_at=$2
WHERE id=$1::uuid AND status='open' AND acknowledged_at IS NULL`, incidentID, now, userID); err != nil {
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
	var incident Incident
	var role string
	err := tx.QueryRow(ctx, `SELECT i.id::text,i.organization_id::text,i.environment_id::text,i.monitor_id::text,
 i.status,i.started_at,i.opened_at,i.acknowledged_at,i.acknowledged_by_user_id::text,
 i.resolved_at,i.resolved_by_user_id::text,i.resolution_kind,i.resolution_reason,m.role
FROM incidents i JOIN org_members m ON m.organization_id=i.organization_id AND m.user_id=$1::uuid
WHERE i.environment_id=$2::uuid AND i.id=$3::uuid FOR UPDATE OF i`, userID, environmentID, incidentID).Scan(
		&incident.ID, &incident.OrganizationID, &incident.EnvironmentID, &incident.MonitorID,
		&incident.Status, &incident.StartedAt, &incident.OpenedAt, &incident.AcknowledgedAt,
		&incident.AcknowledgedByUserID, &incident.ResolvedAt, &incident.ResolvedByUserID,
		&incident.ResolutionKind, &incident.ResolutionReason, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, "", ErrIncidentNotFound
	}
	if err != nil {
		return Incident{}, "", fmt.Errorf("load incident: %w", err)
	}
	return incident, authorization.Role(role), nil
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

func nullableReason(reason string) any {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil
	}
	return reason
}
