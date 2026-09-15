package ownership

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

func (s *Service) ListEnvironments(ctx context.Context, userID, projectID string) ([]TenantEnvironment, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	_, role, err := projectRoleTx(ctx, tx, userID, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := database.New(tx).ListTenantEnvironments(ctx, projectID)
	if err != nil {
		return nil, err
	}
	items := make([]TenantEnvironment, 0, len(rows))
	for _, row := range rows {
		v := TenantEnvironment{ID: row.ID, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, Name: row.Name, Type: row.EnvironmentType, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
		v.Role = role
		v.AllowedActions = authorization.AllowedActions(role)
		items = append(items, v)
	}
	return items, nil
}

func (s *Service) GetEnvironment(ctx context.Context, userID, environmentID string) (TenantEnvironment, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return TenantEnvironment{}, err
	}
	defer tx.Rollback(context.Background())
	_, role, err := environmentRoleTx(ctx, tx, userID, environmentID)
	if err != nil {
		return TenantEnvironment{}, err
	}
	row, err := database.New(tx).GetTenantEnvironment(ctx, environmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantEnvironment{}, ErrEnvironmentNotFound
	}
	if err != nil {
		return TenantEnvironment{}, err
	}
	item := TenantEnvironment{ID: row.ID, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, Name: row.Name, Type: row.EnvironmentType, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
	item.Role = role
	item.AllowedActions = authorization.AllowedActions(role)
	return item, nil
}

func (s *Service) CreateEnvironment(ctx context.Context, userID, projectID, name, kind string) (TenantEnvironment, error) {
	name, kind = strings.TrimSpace(name), strings.ToLower(strings.TrimSpace(kind))
	if name == "" || len(name) > maximumNameBytes || (kind != "production" && kind != "staging" && kind != "development") {
		return TenantEnvironment{}, ErrInvalidInput
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return TenantEnvironment{}, err
	}
	defer tx.Rollback(context.Background())
	org, role, err := projectRoleTx(ctx, tx, userID, projectID)
	if err != nil {
		return TenantEnvironment{}, err
	}
	if !authorization.Allows(role, authorization.PermissionTenantManage) {
		return TenantEnvironment{}, ErrForbidden
	}
	row, err := database.New(tx).CreateTenantEnvironment(ctx, database.CreateTenantEnvironmentParams{OrganizationID: org, ProjectID: projectID, Name: name, EnvironmentType: kind})
	if err != nil {
		return TenantEnvironment{}, err
	}
	v := TenantEnvironment{ID: row.ID, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, Name: row.Name, Type: row.EnvironmentType, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
	if err = recordTenantChange(ctx, tx, org, &v.ID, userID, "environment.created", "environment", v.ID); err != nil {
		return v, err
	}
	if err = tx.Commit(ctx); err != nil {
		return v, err
	}
	v.Role = role
	v.AllowedActions = authorization.AllowedActions(role)
	return v, nil
}

func (s *Service) UpdateEnvironment(ctx context.Context, userID, environmentID, name, kind string) (TenantEnvironment, error) {
	name, kind = strings.TrimSpace(name), strings.ToLower(strings.TrimSpace(kind))
	if name == "" || len(name) > maximumNameBytes || (kind != "production" && kind != "staging" && kind != "development") {
		return TenantEnvironment{}, ErrInvalidInput
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return TenantEnvironment{}, err
	}
	defer tx.Rollback(context.Background())
	org, role, err := environmentRoleTx(ctx, tx, userID, environmentID)
	if err != nil {
		return TenantEnvironment{}, err
	}
	if !authorization.Allows(role, authorization.PermissionTenantManage) {
		return TenantEnvironment{}, ErrForbidden
	}
	row, err := database.New(tx).UpdateTenantEnvironment(ctx, database.UpdateTenantEnvironmentParams{Name: name, EnvironmentType: kind, EnvironmentID: environmentID})
	if err != nil {
		return TenantEnvironment{}, err
	}
	v := TenantEnvironment{ID: row.ID, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, Name: row.Name, Type: row.EnvironmentType, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
	if err = recordTenantChange(ctx, tx, org, &environmentID, userID, "environment.updated", "environment", environmentID); err != nil {
		return v, err
	}
	if err = tx.Commit(ctx); err != nil {
		return v, err
	}
	v.Role = role
	v.AllowedActions = authorization.AllowedActions(role)
	return v, nil
}

func (s *Service) DeleteEnvironment(ctx context.Context, userID, environmentID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	org, role, err := environmentRoleTx(ctx, tx, userID, environmentID)
	if err != nil {
		return err
	}
	if !authorization.Allows(role, authorization.PermissionTenantManage) {
		return ErrForbidden
	}
	rowsAffected, err := database.New(tx).DeleteEmptyTenantEnvironment(ctx, environmentID)
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return ErrDeleteConflict
	}
	if err = recordTenantChange(ctx, tx, org, nil, userID, "environment.deleted", "environment", environmentID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
