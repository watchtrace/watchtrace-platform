package ownership

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
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
	rows, err := tx.Query(ctx, `SELECT id::text,organization_id::text,project_id::text,name,environment_type,created_at,updated_at FROM environments WHERE project_id=$1::uuid ORDER BY created_at,id LIMIT 100`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []TenantEnvironment{}
	for rows.Next() {
		var v TenantEnvironment
		if err = rows.Scan(&v.ID, &v.OrganizationID, &v.ProjectID, &v.Name, &v.Type, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		v.Role = role
		v.AllowedActions = authorization.AllowedActions(role)
		items = append(items, v)
	}
	return items, rows.Err()
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
	var item TenantEnvironment
	err = tx.QueryRow(ctx, `SELECT id::text,organization_id::text,project_id::text,name,environment_type,created_at,updated_at FROM environments WHERE id=$1::uuid`, environmentID).Scan(&item.ID, &item.OrganizationID, &item.ProjectID, &item.Name, &item.Type, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantEnvironment{}, ErrEnvironmentNotFound
	}
	if err != nil {
		return TenantEnvironment{}, err
	}
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
	var v TenantEnvironment
	err = tx.QueryRow(ctx, `INSERT INTO environments(organization_id,project_id,name,environment_type) VALUES($1::uuid,$2::uuid,$3,$4) RETURNING id::text,organization_id::text,project_id::text,name,environment_type,created_at,updated_at`, org, projectID, name, kind).Scan(&v.ID, &v.OrganizationID, &v.ProjectID, &v.Name, &v.Type, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return v, err
	}
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
	var v TenantEnvironment
	err = tx.QueryRow(ctx, `UPDATE environments SET name=$2,environment_type=$3,updated_at=CURRENT_TIMESTAMP WHERE id=$1::uuid RETURNING id::text,organization_id::text,project_id::text,name,environment_type,created_at,updated_at`, environmentID, name, kind).Scan(&v.ID, &v.OrganizationID, &v.ProjectID, &v.Name, &v.Type, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return v, err
	}
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
	tag, err := tx.Exec(ctx, `DELETE FROM environments e WHERE e.id=$1::uuid AND NOT EXISTS(SELECT 1 FROM monitors m WHERE m.environment_id=e.id)`, environmentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDeleteConflict
	}
	if err = recordTenantChange(ctx, tx, org, nil, userID, "environment.deleted", "environment", environmentID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
