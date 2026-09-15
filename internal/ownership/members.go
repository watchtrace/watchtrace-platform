package ownership

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

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
	existingRoleValue, err := database.New(tx).LockOrganizationMemberRole(ctx, database.LockOrganizationMemberRoleParams{OrganizationID: organizationID, UserID: memberID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Member{}, ErrMemberNotFound
	}
	if err != nil {
		return Member{}, err
	}
	existingRole := authorization.Role(existingRoleValue)
	if existingRole == authorization.RoleOwner {
		return Member{}, ErrForbidden
	}
	if role == "" {
		role = existingRole
	}
	if notifications == nil {
		current, preferenceErr := database.New(tx).GetOrganizationMemberNotificationPreference(ctx, database.GetOrganizationMemberNotificationPreferenceParams{OrganizationID: organizationID, UserID: memberID})
		if preferenceErr != nil {
			return Member{}, preferenceErr
		}
		notifications = &current
	}
	row, err := database.New(tx).UpdateOrganizationMember(ctx, database.UpdateOrganizationMemberParams{Role: string(role), IncidentNotificationsEnabled: *notifications, OrganizationID: organizationID, UserID: memberID})
	if err != nil {
		return Member{}, err
	}
	m := Member{UserID: row.UserID, Email: row.Email, Role: authorization.Role(row.Role), IncidentNotificationsEnabled: row.IncidentNotificationsEnabled, CreatedAt: row.CreatedAt.Time}
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
	targetValue, err := database.New(tx).GetOrganizationMemberRole(ctx, database.GetOrganizationMemberRoleParams{OrganizationID: organizationID, UserID: memberID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrMemberNotFound
	}
	if err != nil {
		return err
	}
	target := authorization.Role(targetValue)
	if target == authorization.RoleOwner || (role != authorization.RoleOwner && target == authorization.RoleAdmin) {
		return ErrForbidden
	}
	if err = database.New(tx).DeleteOrganizationMember(ctx, database.DeleteOrganizationMemberParams{OrganizationID: organizationID, UserID: memberID}); err != nil {
		return err
	}
	if err = recordTenantChange(ctx, tx, organizationID, nil, actorID, "membership.removed", "membership", memberID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
