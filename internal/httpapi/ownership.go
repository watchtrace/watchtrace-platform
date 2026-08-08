package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
)

// OwnershipService is the tenant creation boundary used by the HTTP API.
type OwnershipService interface {
	CreateDefault(context.Context, string, ownership.CreateDefaultInput) (ownership.DefaultResult, error)
}

type defaultOwnershipRequest struct {
	Organization *organizationRequest `json:"organization" binding:"required"`
	Project      *projectRequest      `json:"project" binding:"required"`
}

type organizationRequest struct {
	Name string `json:"name" binding:"required,max=120"`
	Slug string `json:"slug" binding:"required,max=63"`
}

type projectRequest struct {
	Name        string `json:"name" binding:"required,max=120"`
	Description string `json:"description" binding:"max=1000"`
}

type defaultOwnershipResponse struct {
	Organization organizationResponse `json:"organization"`
	Membership   membershipResponse   `json:"membership"`
	Project      projectResponse      `json:"project"`
	Environment  environmentResponse  `json:"environment"`
}

type organizationResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type membershipResponse struct {
	OrganizationID string `json:"organization_id"`
	UserID         string `json:"user_id"`
	Role           string `json:"role"`
}

type projectResponse struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
}

type environmentResponse struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
}

func registerOwnershipRoutes(
	router *gin.Engine,
	authenticator SessionAuthenticator,
	service OwnershipService,
) {
	router.POST(
		"/api/v1/organizations",
		requireAuthenticatedUser(authenticator),
		createDefaultOwnership(service),
	)
}

func createDefaultOwnership(service OwnershipService) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := authenticatedUser(c)
		if !ok {
			RespondError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
			return
		}

		var request defaultOwnershipRequest
		if !DecodeJSON(c, &request) {
			return
		}

		result, err := service.CreateDefault(c.Request.Context(), user.ID, ownership.CreateDefaultInput{
			OrganizationName:   request.Organization.Name,
			OrganizationSlug:   request.Organization.Slug,
			ProjectName:        request.Project.Name,
			ProjectDescription: request.Project.Description,
		})
		if err != nil {
			respondOwnershipError(c, err)
			return
		}

		c.JSON(http.StatusCreated, defaultOwnershipResponse{
			Organization: organizationResponse{
				ID: result.Organization.ID, Name: result.Organization.Name, Slug: result.Organization.Slug,
			},
			Membership: membershipResponse{
				OrganizationID: result.Membership.OrganizationID,
				UserID:         result.Membership.UserID,
				Role:           result.Membership.Role,
			},
			Project: projectResponse{
				ID:             result.Project.ID,
				OrganizationID: result.Project.OrganizationID,
				Name:           result.Project.Name,
				Description:    result.Project.Description,
			},
			Environment: environmentResponse{
				ID:             result.Environment.ID,
				OrganizationID: result.Environment.OrganizationID,
				ProjectID:      result.Environment.ProjectID,
				Name:           result.Environment.Name,
				Type:           result.Environment.EnvironmentType,
			},
		})
	}
}

func respondOwnershipError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ownership.ErrInvalidInput):
		RespondError(c, http.StatusUnprocessableEntity, "validation_failed", "organization or project details are invalid")
	case errors.Is(err, ownership.ErrSlugInUse):
		RespondError(c, http.StatusConflict, "organization_slug_in_use", "organization slug is already in use")
	default:
		RespondError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
	}
}
