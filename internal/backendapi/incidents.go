package backendapi

import (
	"context"
	"errors"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/incident"
)

func (s *Service) ListIncidents(ctx context.Context, userID, environmentID string, q PageQuery) (IncidentPage, error) {
	q, err := normalizeQuery(q, 366*24*time.Hour)
	if err != nil {
		return IncidentPage{}, err
	}
	if q.Status != "" && q.Status != "open" && q.Status != "resolved" {
		return IncidentPage{}, ErrInvalidQuery
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return IncidentPage{}, err
	}
	defer tx.Rollback(context.Background())
	if _, _, err = s.authorizeEnvironment(ctx, tx, userID, environmentID, authorization.PermissionIncidentsRead); err != nil {
		return IncidentPage{}, err
	}
	var cursorTime time.Time
	var cursorID string
	if q.Cursor != "" {
		cursorTime, cursorID, err = decodeCursor(q.Cursor)
		if err != nil {
			return IncidentPage{}, ErrInvalidQuery
		}
	}
	rows, err := tx.Query(ctx, `SELECT id::text,organization_id::text,environment_id::text,monitor_id::text,status,started_at,opened_at,acknowledged_at,acknowledged_by_user_id::text,resolved_at,resolved_by_user_id::text,resolution_kind,resolution_reason FROM incidents WHERE environment_id=$1::uuid AND opened_at>=$2 AND opened_at<$3 AND ($4='' OR status=$4) AND ($5::timestamptz IS NULL OR (opened_at,id)<($5,$6::uuid)) ORDER BY opened_at DESC,id DESC LIMIT $7`, environmentID, q.From, q.To, q.Status, nullableTime(cursorTime), nullableString(cursorID), q.Limit+1)
	if err != nil {
		return IncidentPage{}, err
	}
	defer rows.Close()
	items := []incident.Incident{}
	for rows.Next() {
		var v incident.Incident
		if err = rows.Scan(&v.ID, &v.OrganizationID, &v.EnvironmentID, &v.MonitorID, &v.Status, &v.StartedAt, &v.OpenedAt, &v.AcknowledgedAt, &v.AcknowledgedByUserID, &v.ResolvedAt, &v.ResolvedByUserID, &v.ResolutionKind, &v.ResolutionReason); err != nil {
			return IncidentPage{}, err
		}
		items = append(items, v)
	}
	page := IncidentPage{Items: items}
	if len(items) > q.Limit {
		last := items[q.Limit-1]
		c := encodeCursor(last.OpenedAt, last.ID)
		page.NextCursor = &c
		page.Items = items[:q.Limit]
	}
	return page, rows.Err()
}

func (s *Service) GetIncident(ctx context.Context, userID, environmentID, incidentID string) (IncidentSummary, error) {
	base, err := s.incidents.Get(ctx, userID, environmentID, incidentID)
	if err != nil {
		return IncidentSummary{}, mapIncidentError(err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return IncidentSummary{}, err
	}
	defer tx.Rollback(context.Background())
	result := IncidentSummary{Incident: base, Events: []IncidentEvent{}, Deliveries: []Delivery{}}
	rows, err := tx.Query(ctx, `SELECT id::text,event_type,actor_user_id::text,source_job_id::text,safe_reason,occurred_at FROM incident_events WHERE incident_id=$1::uuid ORDER BY occurred_at,id LIMIT 200`, incidentID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var v IncidentEvent
		if err = rows.Scan(&v.ID, &v.Type, &v.ActorUserID, &v.SourceJobID, &v.Reason, &v.OccurredAt); err != nil {
			rows.Close()
			return result, err
		}
		result.Events = append(result.Events, v)
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT delivery_id::text,transition,state,attempt_count,next_attempt_at,last_provider_status,accepted_at,failed_at FROM notification_outbox WHERE incident_id=$1::uuid ORDER BY created_at,delivery_id LIMIT 200`, incidentID)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var v Delivery
		if err = rows.Scan(&v.ID, &v.Transition, &v.State, &v.Attempts, &v.NextAttemptAt, &v.ProviderStatus, &v.AcceptedAt, &v.FailedAt); err != nil {
			return result, err
		}
		result.Deliveries = append(result.Deliveries, v)
	}
	return result, rows.Err()
}
func (s *Service) Acknowledge(ctx context.Context, userID, environmentID, incidentID, reason string) (IncidentSummary, error) {
	if _, err := s.incidents.Acknowledge(ctx, userID, environmentID, incidentID, reason); err != nil {
		return IncidentSummary{}, mapIncidentError(err)
	}
	return s.GetIncident(ctx, userID, environmentID, incidentID)
}
func (s *Service) Resolve(ctx context.Context, userID, environmentID, incidentID, reason string) (IncidentSummary, error) {
	if _, err := s.incidents.Resolve(ctx, userID, environmentID, incidentID, reason); err != nil {
		return IncidentSummary{}, mapIncidentError(err)
	}
	return s.GetIncident(ctx, userID, environmentID, incidentID)
}
func mapIncidentError(err error) error {
	if errors.Is(err, incident.ErrIncidentNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, incident.ErrForbidden) {
		return ErrForbidden
	}
	if errors.Is(err, incident.ErrInvalidInput) || errors.Is(err, incident.ErrAlreadyResolved) {
		return ErrInvalidQuery
	}
	return err
}
