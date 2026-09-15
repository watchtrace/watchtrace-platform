package ownership

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
)

var (
	ErrProjectNotFound     = errors.New("project not found")
	ErrEnvironmentNotFound = errors.New("environment not found")
	ErrDeleteConflict      = errors.New("tenant resource is not empty")
	ErrMemberNotFound      = errors.New("member not found")
)

type OrganizationView struct {
	ID             string             `json:"id"`
	Name           string             `json:"name"`
	Slug           string             `json:"slug"`
	Role           authorization.Role `json:"role"`
	AllowedActions []string           `json:"allowed_actions"`
	CreatedAt      time.Time          `json:"created_at"`
}

type TenantProject struct {
	ID             string             `json:"id"`
	OrganizationID string             `json:"organization_id"`
	Name           string             `json:"name"`
	Description    string             `json:"description"`
	Role           authorization.Role `json:"role"`
	AllowedActions []string           `json:"allowed_actions"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
}

type TenantEnvironment struct {
	ID             string             `json:"id"`
	OrganizationID string             `json:"organization_id"`
	ProjectID      string             `json:"project_id"`
	Name           string             `json:"name"`
	Type           string             `json:"type"`
	Role           authorization.Role `json:"role"`
	AllowedActions []string           `json:"allowed_actions"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
}

func (s *Service) ListOrganizations(ctx context.Context, userID string) ([]OrganizationView, error) {
	if !uuidPattern.MatchString(userID) {
		return nil, ErrInvalidInput
	}
	rows, err := s.db.Query(ctx, `SELECT o.id::text,o.name,o.slug,m.role,o.created_at
FROM organizations o JOIN org_members m ON m.organization_id=o.id
WHERE m.user_id=$1::uuid AND o.deleted_at IS NULL ORDER BY o.created_at,o.id LIMIT 100`, userID)
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}
	defer rows.Close()
	items := []OrganizationView{}
	for rows.Next() {
		var item OrganizationView
		if err = rows.Scan(&item.ID, &item.Name, &item.Slug, &item.Role, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.AllowedActions = authorization.AllowedActions(item.Role)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) GetOrganization(ctx context.Context, userID, organizationID string) (OrganizationView, error) {
	if !uuidPattern.MatchString(userID) || !uuidPattern.MatchString(organizationID) {
		return OrganizationView{}, ErrOrganizationNotFound
	}
	var item OrganizationView
	err := s.db.QueryRow(ctx, `SELECT o.id::text,o.name,o.slug,m.role,o.created_at FROM organizations o
JOIN org_members m ON m.organization_id=o.id AND m.user_id=$1::uuid
WHERE o.id=$2::uuid AND o.deleted_at IS NULL`, userID, organizationID).Scan(&item.ID, &item.Name, &item.Slug, &item.Role, &item.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrganizationView{}, ErrOrganizationNotFound
	}
	if err != nil {
		return item, err
	}
	item.AllowedActions = authorization.AllowedActions(item.Role)
	return item, nil
}

func (s *Service) UpdateOrganization(ctx context.Context, userID, organizationID, name string) (OrganizationView, error) {
	name = strings.TrimSpace(name)
	if len(name) < 1 || len(name) > maximumNameBytes {
		return OrganizationView{}, ErrInvalidInput
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return OrganizationView{}, err
	}
	defer tx.Rollback(context.Background())
	role, err := roleTx(ctx, tx, userID, organizationID)
	if err != nil {
		return OrganizationView{}, err
	}
	if !authorization.Allows(role, authorization.PermissionTenantManage) {
		return OrganizationView{}, ErrForbidden
	}
	if _, err = tx.Exec(ctx, `UPDATE organizations SET name=$2,updated_at=CURRENT_TIMESTAMP WHERE id=$1::uuid AND deleted_at IS NULL`, organizationID, name); err != nil {
		return OrganizationView{}, err
	}
	if err = recordTenantChange(ctx, tx, organizationID, nil, userID, "organization.updated", "organization", organizationID); err != nil {
		return OrganizationView{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return OrganizationView{}, err
	}
	return s.GetOrganization(ctx, userID, organizationID)
}

func (s *Service) DeleteOrganization(ctx context.Context, userID, organizationID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	role, err := roleTx(ctx, tx, userID, organizationID)
	if err != nil {
		return err
	}
	if role != authorization.RoleOwner {
		return ErrForbidden
	}
	if _, err = tx.Exec(ctx, `UPDATE organizations SET deleted_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=$1::uuid AND deleted_at IS NULL`, organizationID); err != nil {
		return err
	}
	if err = recordAudit(ctx, tx, organizationID, userID, "organization.deleted", "organization", organizationID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
