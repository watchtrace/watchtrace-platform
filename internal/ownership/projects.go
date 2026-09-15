package ownership

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

func (s *Service) ListProjects(ctx context.Context, userID, organizationID string) ([]TenantProject, error) {
	role, err := s.currentRole(ctx, userID, organizationID)
	if err != nil {
		return nil, err
	}
	if !authorization.Allows(role, authorization.PermissionTenantRead) {
		return nil, ErrForbidden
	}
	rows, err := database.New(s.db).ListTenantProjects(ctx, organizationID)
	if err != nil {
		return nil, err
	}
	items := make([]TenantProject, 0, len(rows))
	for _, row := range rows {
		v := TenantProject{ID: row.ID, OrganizationID: row.OrganizationID, Name: row.Name, Description: row.Description, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
		v.Role = role
		v.AllowedActions = authorization.AllowedActions(role)
		items = append(items, v)
	}
	return items, nil
}

func (s *Service) GetProject(ctx context.Context, userID, projectID string) (TenantProject, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return TenantProject{}, err
	}
	defer tx.Rollback(context.Background())
	_, role, err := projectRoleTx(ctx, tx, userID, projectID)
	if err != nil {
		return TenantProject{}, err
	}
	row, err := database.New(tx).GetTenantProject(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantProject{}, ErrProjectNotFound
	}
	if err != nil {
		return TenantProject{}, err
	}
	item := TenantProject{ID: row.ID, OrganizationID: row.OrganizationID, Name: row.Name, Description: row.Description, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
	item.Role = role
	item.AllowedActions = authorization.AllowedActions(role)
	return item, nil
}

func (s *Service) CreateProject(ctx context.Context, userID, organizationID, name, description string) (TenantProject, error) {
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	if name == "" || len(name) > maximumNameBytes || len(description) > maximumDescriptionBytes {
		return TenantProject{}, ErrInvalidInput
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return TenantProject{}, err
	}
	defer tx.Rollback(context.Background())
	role, err := roleTx(ctx, tx, userID, organizationID)
	if err != nil {
		return TenantProject{}, err
	}
	if !authorization.Allows(role, authorization.PermissionTenantManage) {
		return TenantProject{}, ErrForbidden
	}
	row, err := database.New(tx).CreateTenantProject(ctx, database.CreateTenantProjectParams{OrganizationID: organizationID, Name: name, Description: description})
	if err != nil {
		return TenantProject{}, err
	}
	p := TenantProject{ID: row.ID, OrganizationID: row.OrganizationID, Name: row.Name, Description: row.Description, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
	if err = recordTenantChange(ctx, tx, organizationID, nil, userID, "project.created", "project", p.ID); err != nil {
		return p, err
	}
	if err = tx.Commit(ctx); err != nil {
		return p, err
	}
	p.Role = role
	p.AllowedActions = authorization.AllowedActions(role)
	return p, nil
}

func (s *Service) UpdateProject(ctx context.Context, userID, projectID, name, description string) (TenantProject, error) {
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	if name == "" || len(name) > maximumNameBytes || len(description) > maximumDescriptionBytes {
		return TenantProject{}, ErrInvalidInput
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return TenantProject{}, err
	}
	defer tx.Rollback(context.Background())
	org, role, err := projectRoleTx(ctx, tx, userID, projectID)
	if err != nil {
		return TenantProject{}, err
	}
	if !authorization.Allows(role, authorization.PermissionTenantManage) {
		return TenantProject{}, ErrForbidden
	}
	row, err := database.New(tx).UpdateTenantProject(ctx, database.UpdateTenantProjectParams{Name: name, Description: description, ProjectID: projectID})
	if err != nil {
		return TenantProject{}, err
	}
	p := TenantProject{ID: row.ID, OrganizationID: row.OrganizationID, Name: row.Name, Description: row.Description, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
	if err = recordTenantChange(ctx, tx, org, nil, userID, "project.updated", "project", projectID); err != nil {
		return p, err
	}
	if err = tx.Commit(ctx); err != nil {
		return p, err
	}
	p.Role = role
	p.AllowedActions = authorization.AllowedActions(role)
	return p, nil
}

func (s *Service) DeleteProject(ctx context.Context, userID, projectID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	org, role, err := projectRoleTx(ctx, tx, userID, projectID)
	if err != nil {
		return err
	}
	if !authorization.Allows(role, authorization.PermissionTenantManage) {
		return ErrForbidden
	}
	rowsAffected, err := database.New(tx).DeleteEmptyTenantProject(ctx, projectID)
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return ErrDeleteConflict
	}
	if err = recordTenantChange(ctx, tx, org, nil, userID, "project.deleted", "project", projectID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
