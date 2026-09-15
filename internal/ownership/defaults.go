package ownership

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

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
	if err = recordTenantChange(ctx, tx, organization.ID, &environment.ID, userID, "environment.created", "environment", environment.ID); err != nil {
		return DefaultResult{}, fmt.Errorf("record ownership creation: %w", err)
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
