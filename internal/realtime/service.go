// Package realtime provides durable, tenant-scoped refresh hints for SSE.
package realtime

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

var ErrNotFound = errors.New("event stream not found")

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
}
type Service struct{ db DB }

func New(db DB) *Service { return &Service{db: db} }

type Event struct {
	ID           int64     `json:"id"`
	Type         string    `json:"type"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	OccurredAt   time.Time `json:"occurred_at"`
}

func (s *Service) Poll(ctx context.Context, userID, environmentID string, after int64, limit int) ([]Event, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, ErrNotFound
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	queries := database.New(tx)
	authorized, err := queries.GetAccessibleEnvironmentOrganization(ctx, database.GetAccessibleEnvironmentOrganizationParams{UserID: userID, EnvironmentID: environmentID})
	if errors.Is(err, pgx.ErrNoRows) || !authorization.Allows(authorization.Role(authorized.Role), authorization.PermissionTenantRead) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := queries.ListEnvironmentRefreshEvents(ctx, database.ListEnvironmentRefreshEventsParams{EnvironmentID: environmentID, AfterID: after, ResultLimit: int32(limit)})
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		e := Event{ID: row.ID, Type: row.EventType, ResourceType: row.ResourceType, ResourceID: row.ResourceID, OccurredAt: row.OccurredAt.Time}
		events = append(events, e)
	}
	return events, nil
}
func ParseLastID(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 0 {
		return 0, ErrNotFound
	}
	return id, nil
}
