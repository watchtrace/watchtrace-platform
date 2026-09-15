// Package ownership implements organization, project, and environment
// ownership operations.
package ownership

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
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

// NewService constructs a fully configured ownership service.
func NewService(db databaseConnection, sender auth.AccountActionSender) (*Service, error) {
	if db == nil || sender == nil {
		return nil, errors.New("ownership: database and account action sender are required")
	}
	return &Service{db: db, sender: sender}, nil
}
