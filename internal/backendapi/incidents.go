package backendapi

import (
	"context"
	"errors"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/incident"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
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
	rows, err := database.New(tx).ListBackendIncidents(ctx, database.ListBackendIncidentsParams{
		EnvironmentID: environmentID, FromAt: databaseTimestamp(q.From), ToAt: databaseTimestamp(q.To),
		Status: q.Status, HasCursor: q.Cursor != "", CursorAt: databaseTimestamp(cursorTime),
		CursorID: cursorID, ResultLimit: int32(q.Limit + 1),
	})
	if err != nil {
		return IncidentPage{}, err
	}
	items := make([]incident.Incident, 0, len(rows))
	for _, row := range rows {
		v := incident.Incident{ID: row.ID, OrganizationID: row.OrganizationID, EnvironmentID: row.EnvironmentID,
			MonitorID: row.MonitorID, Status: row.Status, StartedAt: row.StartedAt.Time, OpenedAt: row.OpenedAt.Time,
			AcknowledgedAt: optionalTimestamp(row.AcknowledgedAt), AcknowledgedByUserID: optionalUUID(row.AcknowledgedByUserID),
			ResolvedAt: optionalTimestamp(row.ResolvedAt), ResolvedByUserID: optionalUUID(row.ResolvedByUserID),
			ResolutionKind: optionalText(row.ResolutionKind), ResolutionReason: optionalText(row.ResolutionReason)}
		items = append(items, v)
	}
	page := IncidentPage{Items: items}
	if len(items) > q.Limit {
		last := items[q.Limit-1]
		c := encodeCursor(last.OpenedAt, last.ID)
		page.NextCursor = &c
		page.Items = items[:q.Limit]
	}
	return page, nil
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
	queries := database.New(tx)
	rows, err := queries.ListBackendIncidentEvents(ctx, incidentID)
	if err != nil {
		return result, err
	}
	for _, row := range rows {
		v := IncidentEvent{ID: row.ID, Type: row.EventType, ActorUserID: optionalUUID(row.ActorUserID),
			SourceJobID: optionalUUID(row.SourceJobID), Reason: optionalText(row.SafeReason), OccurredAt: row.OccurredAt.Time}
		result.Events = append(result.Events, v)
	}
	deliveries, err := queries.ListBackendNotificationDeliveries(ctx, incidentID)
	if err != nil {
		return result, err
	}
	for _, row := range deliveries {
		v := Delivery{ID: row.DeliveryID, Transition: row.Transition, State: row.State, Attempts: row.AttemptCount,
			NextAttemptAt: row.NextAttemptAt.Time, ProviderStatus: optionalText(row.LastProviderStatus),
			AcceptedAt: optionalTimestamp(row.AcceptedAt), FailedAt: optionalTimestamp(row.FailedAt)}
		result.Deliveries = append(result.Deliveries, v)
	}
	return result, nil
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
