package ownership

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
)

func (s *Service) ListProjects(ctx context.Context, userID, organizationID string) ([]TenantProject, error) {
	role, err := s.currentRole(ctx, userID, organizationID)
	if err != nil {
		return nil, err
	}
	if !authorization.Allows(role, authorization.PermissionTenantRead) {
		return nil, ErrForbidden
	}
	rows, err := s.db.Query(ctx, `SELECT id::text,organization_id::text,name,description,created_at,updated_at FROM projects WHERE organization_id=$1::uuid ORDER BY created_at,id LIMIT 100`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []TenantProject{}
	for rows.Next() {
		var v TenantProject
		if err = rows.Scan(&v.ID, &v.OrganizationID, &v.Name, &v.Description, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		v.Role = role
		v.AllowedActions = authorization.AllowedActions(role)
		items = append(items, v)
	}
	return items, rows.Err()
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
	var item TenantProject
	err = tx.QueryRow(ctx, `SELECT id::text,organization_id::text,name,description,created_at,updated_at FROM projects WHERE id=$1::uuid`, projectID).Scan(&item.ID, &item.OrganizationID, &item.Name, &item.Description, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantProject{}, ErrProjectNotFound
	}
	if err != nil {
		return TenantProject{}, err
	}
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
	var p TenantProject
	err = tx.QueryRow(ctx, `INSERT INTO projects(organization_id,name,description) VALUES($1::uuid,$2,$3) RETURNING id::text,organization_id::text,name,description,created_at,updated_at`, organizationID, name, description).Scan(&p.ID, &p.OrganizationID, &p.Name, &p.Description, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
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
	var p TenantProject
	err = tx.QueryRow(ctx, `UPDATE projects SET name=$2,description=$3,updated_at=CURRENT_TIMESTAMP WHERE id=$1::uuid RETURNING id::text,organization_id::text,name,description,created_at,updated_at`, projectID, name, description).Scan(&p.ID, &p.OrganizationID, &p.Name, &p.Description, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
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
	tag, err := tx.Exec(ctx, `DELETE FROM projects p WHERE p.id=$1::uuid AND NOT EXISTS(SELECT 1 FROM environments e WHERE e.project_id=p.id)`, projectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDeleteConflict
	}
	if err = recordTenantChange(ctx, tx, org, nil, userID, "project.deleted", "project", projectID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
