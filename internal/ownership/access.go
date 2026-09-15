package ownership

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
)

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
