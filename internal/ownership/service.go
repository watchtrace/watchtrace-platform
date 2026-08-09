// Package ownership implements organization, project, and environment
// ownership operations.
package ownership

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

const (
	maximumNameBytes        = 120
	maximumSlugBytes        = 63
	maximumDescriptionBytes = 1000
	invitationLifetime      = 7 * 24 * time.Hour
)

var (
	// ErrInvalidInput indicates that ownership names or the slug do not satisfy
	// the bounded service rules.
	ErrInvalidInput = errors.New("invalid ownership input")
	// ErrSlugInUse indicates that another organization owns the normalized slug.
	ErrSlugInUse            = errors.New("organization slug already in use")
	ErrOrganizationNotFound = errors.New("organization not found")
	ErrForbidden            = errors.New("permission denied")
	ErrAlreadyMember        = errors.New("organization member already exists")
	ErrInvalidInvitation    = errors.New("invalid organization invitation")
	ErrEmailNotVerified     = errors.New("verified email required")

	slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
)

type databaseConnection interface {
	database.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

// CreateDefaultInput contains the customer-selected values for the initial
// ownership hierarchy. The production environment is server-defined.
type CreateDefaultInput struct {
	OrganizationName   string
	OrganizationSlug   string
	ProjectName        string
	ProjectDescription string
}

// Organization is the tenant root returned by the creation operation.
type Organization struct {
	ID   string
	Name string
	Slug string
}

// Membership identifies the authenticated user as the organization owner.
type Membership struct {
	OrganizationID string
	UserID         string
	Role           string
}

type Member struct {
	UserID                       string
	Email                        string
	Role                         authorization.Role
	IncidentNotificationsEnabled bool
	CreatedAt                    time.Time
}

type Invitation struct {
	OrganizationID string
	Email          string
	Role           authorization.Role
	ExpiresAt      time.Time
}

// Project is the initial project owned by the new organization.
type Project struct {
	ID             string
	OrganizationID string
	Name           string
	Description    string
}

// Environment is the server-created production environment.
type Environment struct {
	ID              string
	OrganizationID  string
	ProjectID       string
	Name            string
	EnvironmentType string
}

// DefaultResult is the complete hierarchy committed by CreateDefault.
type DefaultResult struct {
	Organization Organization
	Membership   Membership
	Project      Project
	Environment  Environment
}

// Service creates and validates tenant ownership data.
type Service struct {
	db     databaseConnection
	sender auth.AccountActionSender
}

// NewService constructs an ownership service backed by PostgreSQL.
func NewService(db databaseConnection, senders ...auth.AccountActionSender) *Service {
	var sender auth.AccountActionSender
	if len(senders) > 0 {
		sender = senders[0]
	}
	return &Service{db: db, sender: sender}
}

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
	if s.sender == nil {
		return Invitation{}, errors.New("invitation sender is unavailable")
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

// CreateDefault creates one organization, its sole owner membership, an
// initial project, and its production environment in one transaction.
func (s *Service) CreateDefault(
	ctx context.Context,
	userID string,
	input CreateDefaultInput,
) (DefaultResult, error) {
	normalized, err := normalizeInput(userID, input)
	if err != nil {
		return DefaultResult{}, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return DefaultResult{}, fmt.Errorf("begin ownership transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	queries := database.New(tx)
	organization, err := queries.CreateOrganization(ctx, database.CreateOrganizationParams{
		Name: normalized.OrganizationName,
		Slug: normalized.OrganizationSlug,
	})
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.ConstraintName == "organizations_slug_unique_idx" {
			return DefaultResult{}, ErrSlugInUse
		}
		return DefaultResult{}, fmt.Errorf("create organization: %w", err)
	}

	if err := queries.CreateOwnerMembership(ctx, database.CreateOwnerMembershipParams{
		OrganizationID: organization.ID,
		UserID:         userID,
	}); err != nil {
		return DefaultResult{}, fmt.Errorf("create owner membership: %w", err)
	}

	project, err := queries.CreateProject(ctx, database.CreateProjectParams{
		OrganizationID: organization.ID,
		Name:           normalized.ProjectName,
		Description:    normalized.ProjectDescription,
	})
	if err != nil {
		return DefaultResult{}, fmt.Errorf("create project: %w", err)
	}

	environment, err := queries.CreateProductionEnvironment(ctx, database.CreateProductionEnvironmentParams{
		OrganizationID: organization.ID,
		ProjectID:      project.ID,
	})
	if err != nil {
		return DefaultResult{}, fmt.Errorf("create production environment: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return DefaultResult{}, fmt.Errorf("commit ownership transaction: %w", err)
	}

	return DefaultResult{
		Organization: Organization{
			ID:   organization.ID,
			Name: organization.Name,
			Slug: organization.Slug,
		},
		Membership: Membership{
			OrganizationID: organization.ID,
			UserID:         userID,
			Role:           "owner",
		},
		Project: Project{
			ID:             project.ID,
			OrganizationID: project.OrganizationID,
			Name:           project.Name,
			Description:    project.Description,
		},
		Environment: Environment{
			ID:              environment.ID,
			OrganizationID:  environment.OrganizationID,
			ProjectID:       environment.ProjectID,
			Name:            environment.Name,
			EnvironmentType: environment.EnvironmentType,
		},
	}, nil
}

func normalizeInput(userID string, input CreateDefaultInput) (CreateDefaultInput, error) {
	normalized := CreateDefaultInput{
		OrganizationName:   strings.TrimSpace(input.OrganizationName),
		OrganizationSlug:   strings.ToLower(strings.TrimSpace(input.OrganizationSlug)),
		ProjectName:        strings.TrimSpace(input.ProjectName),
		ProjectDescription: strings.TrimSpace(input.ProjectDescription),
	}

	if userID == "" ||
		normalized.OrganizationName == "" ||
		len(normalized.OrganizationName) > maximumNameBytes ||
		!slugPattern.MatchString(normalized.OrganizationSlug) ||
		len(normalized.OrganizationSlug) > maximumSlugBytes ||
		normalized.ProjectName == "" ||
		len(normalized.ProjectName) > maximumNameBytes ||
		len(normalized.ProjectDescription) > maximumDescriptionBytes {
		return CreateDefaultInput{}, ErrInvalidInput
	}

	return normalized, nil
}
