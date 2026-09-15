package ownership

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
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
	rows, err := database.New(s.db).ListAccessibleOrganizations(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}
	items := make([]OrganizationView, 0, len(rows))
	for _, row := range rows {
		item := OrganizationView{ID: row.ID, Name: row.Name, Slug: row.Slug, Role: authorization.Role(row.Role), CreatedAt: row.CreatedAt.Time}
		item.AllowedActions = authorization.AllowedActions(item.Role)
		items = append(items, item)
	}
	return items, nil
}

func (s *Service) GetOrganization(ctx context.Context, userID, organizationID string) (OrganizationView, error) {
	if !uuidPattern.MatchString(userID) || !uuidPattern.MatchString(organizationID) {
		return OrganizationView{}, ErrOrganizationNotFound
	}
	row, err := database.New(s.db).GetAccessibleOrganization(ctx, database.GetAccessibleOrganizationParams{
		UserID: userID, OrganizationID: organizationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return OrganizationView{}, ErrOrganizationNotFound
	}
	if err != nil {
		return OrganizationView{}, err
	}
	item := OrganizationView{ID: row.ID, Name: row.Name, Slug: row.Slug, Role: authorization.Role(row.Role), CreatedAt: row.CreatedAt.Time}
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
	if err = database.New(tx).UpdateOrganizationName(ctx, database.UpdateOrganizationNameParams{Name: name, OrganizationID: organizationID}); err != nil {
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
	if err = database.New(tx).SoftDeleteOrganization(ctx, organizationID); err != nil {
		return err
	}
	if err = recordAudit(ctx, tx, organizationID, userID, "organization.deleted", "organization", organizationID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
