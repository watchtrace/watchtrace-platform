package ownership

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

func roleTx(ctx context.Context, tx pgx.Tx, userID, organizationID string) (authorization.Role, error) {
	role, err := database.New(tx).GetOrganizationMembershipRole(ctx, database.GetOrganizationMembershipRoleParams{
		OrganizationID: organizationID,
		UserID:         userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrOrganizationNotFound
	}
	return authorization.Role(role), err
}
func projectRoleTx(ctx context.Context, tx pgx.Tx, userID, projectID string) (string, authorization.Role, error) {
	row, err := database.New(tx).AuthorizeProjectMembership(ctx, database.AuthorizeProjectMembershipParams{
		UserID:    userID,
		ProjectID: projectID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrProjectNotFound
	}
	return row.OrganizationID, authorization.Role(row.Role), err
}
func environmentRoleTx(ctx context.Context, tx pgx.Tx, userID, environmentID string) (string, authorization.Role, error) {
	row, err := database.New(tx).AuthorizeEnvironmentMembership(ctx, database.AuthorizeEnvironmentMembershipParams{
		UserID:        userID,
		EnvironmentID: environmentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrEnvironmentNotFound
	}
	return row.OrganizationID, authorization.Role(row.Role), err
}
func recordTenantChange(ctx context.Context, tx pgx.Tx, org string, env *string, actor, action, resource, id string) error {
	environmentID := ""
	if env != nil {
		environmentID = *env
	}
	if err := database.New(tx).InsertTenantRefreshEvent(ctx, database.InsertTenantRefreshEventParams{
		OrganizationID: org,
		EnvironmentID:  environmentID,
		EventType:      eventType(action),
		ResourceType:   resource,
		ResourceID:     id,
	}); err != nil {
		return err
	}
	return recordAudit(ctx, tx, org, actor, action, resource, id)
}
func recordAudit(ctx context.Context, tx pgx.Tx, org, actor, action, resource, id string) error {
	return database.New(tx).InsertAuditLog(ctx, database.InsertAuditLogParams{
		OrganizationID: org,
		ActorUserID:    actor,
		Action:         action,
		ResourceType:   resource,
		ResourceID:     id,
	})
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
