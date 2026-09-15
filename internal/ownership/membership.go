package ownership

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

func (s *Service) ListMembers(ctx context.Context, userID, organizationID string) ([]Member, error) {
	role, err := s.currentRole(ctx, userID, organizationID)
	if err != nil {
		return nil, err
	}
	if !authorization.Allows(role, authorization.PermissionMembersRead) {
		return nil, ErrForbidden
	}
	rows, err := database.New(s.db).ListOrganizationMembers(ctx, organizationID)
	if err != nil {
		return nil, fmt.Errorf("list organization members: %w", err)
	}
	members := make([]Member, 0, len(rows))
	for _, row := range rows {
		members = append(members, Member{UserID: row.UserID, Email: row.Email, Role: authorization.Role(row.Role), IncidentNotificationsEnabled: row.IncidentNotificationsEnabled, CreatedAt: row.CreatedAt.Time})
	}
	return members, nil
}

func (s *Service) Invite(ctx context.Context, userID, organizationID, email string, role authorization.Role) (Invitation, error) {
	normalizedEmail, err := normalizeEmail(email)
	if err != nil || !uuidPattern.MatchString(userID) || !uuidPattern.MatchString(organizationID) || !authorization.ValidAssignableRole(role) {
		return Invitation{}, ErrInvalidInput
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Invitation{}, fmt.Errorf("begin invitation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	queries := database.New(tx)
	current, err := queries.GetOrganizationMembershipRole(ctx, database.GetOrganizationMembershipRoleParams{OrganizationID: organizationID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Invitation{}, ErrOrganizationNotFound
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("authorize invitation: %w", err)
	}
	if !authorization.Allows(authorization.Role(current), authorization.PermissionMembersInvite) {
		return Invitation{}, ErrForbidden
	}
	exists, err := queries.ExistingOrganizationMemberByEmail(ctx, database.ExistingOrganizationMemberByEmailParams{OrganizationID: organizationID, Email: normalizedEmail})
	if err != nil {
		return Invitation{}, fmt.Errorf("check existing member: %w", err)
	}
	if exists {
		return Invitation{}, ErrAlreadyMember
	}
	if _, err := queries.InvalidatePendingInvitation(ctx, database.InvalidatePendingInvitationParams{OrganizationID: organizationID, Email: normalizedEmail}); err != nil {
		return Invitation{}, fmt.Errorf("invalidate pending invitation: %w", err)
	}
	token, digest, err := auth.NewInvitationToken()
	if err != nil {
		return Invitation{}, err
	}
	expiresAt := time.Now().UTC().Truncate(time.Microsecond).Add(invitationLifetime)
	if err := queries.CreateOrganizationInvitation(ctx, database.CreateOrganizationInvitationParams{OrganizationID: organizationID, InvitedByUserID: userID, Email: normalizedEmail, Role: string(role), TokenDigest: digest, ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true}}); err != nil {
		return Invitation{}, fmt.Errorf("create organization invitation: %w", err)
	}
	if err := s.sender.SendInvitation(ctx, normalizedEmail, token); err != nil {
		return Invitation{}, fmt.Errorf("deliver organization invitation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Invitation{}, fmt.Errorf("commit invitation: %w", err)
	}
	return Invitation{OrganizationID: organizationID, Email: normalizedEmail, Role: role, ExpiresAt: expiresAt}, nil
}

func (s *Service) AcceptInvitation(ctx context.Context, user auth.User, token string) (Membership, error) {
	if !user.EmailVerified {
		return Membership{}, ErrEmailNotVerified
	}
	if !auth.ValidInvitationToken(token) {
		return Membership{}, ErrInvalidInvitation
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Membership{}, fmt.Errorf("begin invitation acceptance: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	queries := database.New(tx)
	digest := sha256.Sum256([]byte(token))
	stored, err := queries.LockOrganizationInvitation(ctx, digest[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return Membership{}, ErrInvalidInvitation
	}
	if err != nil {
		return Membership{}, fmt.Errorf("lock organization invitation: %w", err)
	}
	if stored.AcceptedAt.Valid || !stored.ExpiresAt.Valid || !stored.ExpiresAt.Time.After(time.Now().UTC()) || !strings.EqualFold(strings.TrimSpace(stored.Email), strings.TrimSpace(user.Email)) {
		return Membership{}, ErrInvalidInvitation
	}
	created, err := queries.AcceptOrganizationInvitation(ctx, database.AcceptOrganizationInvitationParams{InvitationID: stored.ID, UserID: user.ID})
	if err != nil {
		return Membership{}, fmt.Errorf("accept organization invitation: %w", err)
	}
	if created == 1 {
		if err = recordTenantChange(ctx, tx, stored.OrganizationID, nil, user.ID, "membership.created", "membership", user.ID); err != nil {
			return Membership{}, fmt.Errorf("record invitation acceptance: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Membership{}, fmt.Errorf("commit invitation acceptance: %w", err)
	}
	if created != 1 {
		return Membership{}, ErrAlreadyMember
	}
	return Membership{OrganizationID: stored.OrganizationID, UserID: user.ID, Role: stored.Role}, nil
}

func (s *Service) currentRole(ctx context.Context, userID, organizationID string) (authorization.Role, error) {
	if !uuidPattern.MatchString(userID) || !uuidPattern.MatchString(organizationID) {
		return "", ErrOrganizationNotFound
	}
	role, err := database.New(s.db).GetOrganizationMembershipRole(ctx, database.GetOrganizationMembershipRoleParams{OrganizationID: organizationID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrOrganizationNotFound
	}
	if err != nil {
		return "", fmt.Errorf("load current organization role: %w", err)
	}
	return authorization.Role(role), nil
}

func normalizeEmail(email string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	parsed, err := mail.ParseAddress(normalized)
	if err != nil || parsed.Address != normalized || len(normalized) > 254 {
		return "", ErrInvalidInput
	}
	return normalized, nil
}
