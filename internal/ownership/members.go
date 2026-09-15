package ownership

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
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
