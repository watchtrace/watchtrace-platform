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

func (s *Service) UpdateMember(ctx context.Context, actorID, organizationID, memberID string, role authorization.Role, notifications *bool) (Member, error) {
	if !uuidPattern.MatchString(memberID) || (role != "" && !authorization.ValidAssignableRole(role)) {
		return Member{}, ErrInvalidInput
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback(context.Background())
	actorRole, err := roleTx(ctx, tx, actorID, organizationID)
	if err != nil {
		return Member{}, err
	}
	if actorID != memberID && !authorization.Allows(actorRole, authorization.PermissionMembersManage) {
		return Member{}, ErrForbidden
	}
	if role != "" && actorID == memberID {
		return Member{}, ErrForbidden
	}
	if role != "" && actorRole != authorization.RoleOwner && role == authorization.RoleAdmin {
		return Member{}, ErrForbidden
	}
	var existingRole authorization.Role
	err = tx.QueryRow(ctx, `SELECT role FROM org_members WHERE organization_id=$1::uuid AND user_id=$2::uuid FOR UPDATE`, organizationID, memberID).Scan(&existingRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return Member{}, ErrMemberNotFound
	}
	if err != nil {
		return Member{}, err
	}
	if existingRole == authorization.RoleOwner {
		return Member{}, ErrForbidden
	}
	if role == "" {
		role = existingRole
	}
	if notifications == nil {
		var current bool
		if err = tx.QueryRow(ctx, `SELECT incident_notifications_enabled FROM org_members WHERE organization_id=$1::uuid AND user_id=$2::uuid`, organizationID, memberID).Scan(&current); err != nil {
			return Member{}, err
		}
		notifications = &current
	}
	var m Member
	err = tx.QueryRow(ctx, `UPDATE org_members m SET role=$3,incident_notifications_enabled=$4,updated_at=CURRENT_TIMESTAMP FROM users u WHERE m.organization_id=$1::uuid AND m.user_id=$2::uuid AND u.id=m.user_id RETURNING m.user_id::text,u.email,m.role,m.incident_notifications_enabled,m.created_at`, organizationID, memberID, role, *notifications).Scan(&m.UserID, &m.Email, &m.Role, &m.IncidentNotificationsEnabled, &m.CreatedAt)
	if err != nil {
		return m, err
	}
	if err = recordTenantChange(ctx, tx, organizationID, nil, actorID, "membership.updated", "membership", memberID); err != nil {
		return m, err
	}
	if err = tx.Commit(ctx); err != nil {
		return m, err
	}
	return m, nil
}

func (s *Service) RemoveMember(ctx context.Context, actorID, organizationID, memberID string) error {
	if actorID == memberID {
		return ErrForbidden
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	role, err := roleTx(ctx, tx, actorID, organizationID)
	if err != nil {
		return err
	}
	if !authorization.Allows(role, authorization.PermissionMembersManage) {
		return ErrForbidden
	}
	var target authorization.Role
	if err = tx.QueryRow(ctx, `SELECT role FROM org_members WHERE organization_id=$1::uuid AND user_id=$2::uuid`, organizationID, memberID).Scan(&target); errors.Is(err, pgx.ErrNoRows) {
		return ErrMemberNotFound
	}
	if err != nil {
		return err
	}
	if target == authorization.RoleOwner || (role != authorization.RoleOwner && target == authorization.RoleAdmin) {
		return ErrForbidden
	}
	if _, err = tx.Exec(ctx, `DELETE FROM org_members WHERE organization_id=$1::uuid AND user_id=$2::uuid`, organizationID, memberID); err != nil {
		return err
	}
	if err = recordTenantChange(ctx, tx, organizationID, nil, actorID, "membership.removed", "membership", memberID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func roleTx(ctx context.Context, tx pgx.Tx, userID, organizationID string) (authorization.Role, error) {
	var role authorization.Role
	err := tx.QueryRow(ctx, `SELECT m.role FROM org_members m JOIN organizations o ON o.id=m.organization_id WHERE m.user_id=$1::uuid AND m.organization_id=$2::uuid AND o.deleted_at IS NULL`, userID, organizationID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrOrganizationNotFound
	}
	return role, err
}
func projectRoleTx(ctx context.Context, tx pgx.Tx, userID, projectID string) (string, authorization.Role, error) {
	var org string
	var role authorization.Role
	err := tx.QueryRow(ctx, `SELECT p.organization_id::text,m.role FROM projects p JOIN organizations o ON o.id=p.organization_id AND o.deleted_at IS NULL JOIN org_members m ON m.organization_id=p.organization_id AND m.user_id=$1::uuid WHERE p.id=$2::uuid`, userID, projectID).Scan(&org, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrProjectNotFound
	}
	return org, role, err
}
func environmentRoleTx(ctx context.Context, tx pgx.Tx, userID, environmentID string) (string, authorization.Role, error) {
	var org string
	var role authorization.Role
	err := tx.QueryRow(ctx, `SELECT e.organization_id::text,m.role FROM environments e JOIN organizations o ON o.id=e.organization_id AND o.deleted_at IS NULL JOIN org_members m ON m.organization_id=e.organization_id AND m.user_id=$1::uuid WHERE e.id=$2::uuid`, userID, environmentID).Scan(&org, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrEnvironmentNotFound
	}
	return org, role, err
}
func recordTenantChange(ctx context.Context, tx pgx.Tx, org string, env *string, actor, action, resource, id string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO api_refresh_events(organization_id,environment_id,event_type,resource_type,resource_id) VALUES($1::uuid,$2::uuid,$3,$4,$5::uuid)`, org, env, eventType(action), resource, id); err != nil {
		return err
	}
	return recordAudit(ctx, tx, org, actor, action, resource, id)
}
func recordAudit(ctx context.Context, tx pgx.Tx, org, actor, action, resource, id string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_logs(organization_id,actor_user_id,action,resource_type,resource_id) VALUES($1::uuid,$2::uuid,$3,$4,$5::uuid)`, org, actor, action, resource, id)
	return err
}
func eventType(action string) string {
	switch {
	case strings.HasPrefix(action, "project."):
		return "project.changed"
	case strings.HasPrefix(action, "environment."):
		return "environment.changed"
	case strings.HasPrefix(action, "membership."):
		return "membership.changed"
	default:
		return "organization.changed"
	}
}
