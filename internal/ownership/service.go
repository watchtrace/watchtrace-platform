// Package ownership implements organization, project, and environment
// ownership operations.
package ownership

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

const (
	maximumNameBytes        = 120
	maximumSlugBytes        = 63
	maximumDescriptionBytes = 1000
)

var (
	// ErrInvalidInput indicates that ownership names or the slug do not satisfy
	// the bounded service rules.
	ErrInvalidInput = errors.New("invalid ownership input")
	// ErrSlugInUse indicates that another organization owns the normalized slug.
	ErrSlugInUse = errors.New("organization slug already in use")

	slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
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
	db databaseConnection
}

// NewService constructs an ownership service backed by PostgreSQL.
func NewService(db databaseConnection) *Service {
	return &Service{db: db}
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
