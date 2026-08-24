// Package realtime provides durable, tenant-scoped refresh hints for SSE.
package realtime

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
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
	var role authorization.Role
	err = tx.QueryRow(ctx, `SELECT m.role FROM environments e JOIN organizations o ON o.id=e.organization_id AND o.deleted_at IS NULL JOIN org_members m ON m.organization_id=e.organization_id AND m.user_id=$1::uuid WHERE e.id=$2::uuid`, userID, environmentID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) || !authorization.Allows(role, authorization.PermissionTenantRead) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id,event_type,resource_type,resource_id::text,occurred_at FROM api_refresh_events WHERE environment_id=$1::uuid AND id>$2 ORDER BY id LIMIT $3`, environmentID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.ID, &e.Type, &e.ResourceType, &e.ResourceID, &e.OccurredAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
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
