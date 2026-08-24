package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
)

// OwnershipService is the tenant creation boundary used by the HTTP API.
type OwnershipService interface {
	CreateDefault(context.Context, string, ownership.CreateDefaultInput) (ownership.DefaultResult, error)
	ListMembers(context.Context, string, string) ([]ownership.Member, error)
	Invite(context.Context, string, string, string, authorization.Role) (ownership.Invitation, error)
	AcceptInvitation(context.Context, auth.User, string) (ownership.Membership, error)
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
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Slug           string   `json:"slug"`
	Role           string   `json:"role"`
	AllowedActions []string `json:"allowed_actions"`
}

type membershipResponse struct {
	OrganizationID string `json:"organization_id"`
	UserID         string `json:"user_id"`
	Role           string `json:"role"`
}

type projectResponse struct {
	ID             string   `json:"id"`
	OrganizationID string   `json:"organization_id"`
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Role           string   `json:"role"`
	AllowedActions []string `json:"allowed_actions"`
}

type environmentResponse struct {
	ID             string   `json:"id"`
	OrganizationID string   `json:"organization_id"`
	ProjectID      string   `json:"project_id"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Role           string   `json:"role"`
	AllowedActions []string `json:"allowed_actions"`
}

type invitationRequest struct {
	Email string `json:"email" binding:"required,email,max=254"`
	Role  string `json:"role" binding:"required,oneof=admin member viewer"`
}
type acceptInvitationRequest struct {
	Token string `json:"token" binding:"required,max=128"`
}
type memberResponse struct {
	UserID                       string    `json:"user_id"`
	Email                        string    `json:"email"`
	Role                         string    `json:"role"`
	IncidentNotificationsEnabled bool      `json:"incident_notifications_enabled"`
	CreatedAt                    time.Time `json:"created_at"`
}
type membersResponse struct {
	Members []memberResponse `json:"members"`
}
type invitationResponse struct {
	OrganizationID string    `json:"organization_id"`
	Email          string    `json:"email"`
	Role           string    `json:"role"`
	ExpiresAt      time.Time `json:"expires_at"`
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
	router.GET("/api/v1/organizations/:orgId/members", requireAuthenticatedUser(authenticator), listOrganizationMembers(service))
	router.POST("/api/v1/organizations/:orgId/invitations", requireAuthenticatedUser(authenticator), inviteOrganizationMember(service))
	router.POST("/api/v1/auth/accept-invitation", requireAuthenticatedUser(authenticator), acceptOrganizationInvitation(service))
}

func listOrganizationMembers(service OwnershipService) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := authenticatedUser(c)
		if !ok {
			RespondError(c, 500, "internal_error", "an internal error occurred")
			return
		}
		members, err := service.ListMembers(c.Request.Context(), user.ID, c.Param("orgId"))
		if err != nil {
			respondOwnershipError(c, err)
			return
		}
		response := make([]memberResponse, 0, len(members))
		for _, member := range members {
			response = append(response, memberResponse{UserID: member.UserID, Email: member.Email, Role: string(member.Role), IncidentNotificationsEnabled: member.IncidentNotificationsEnabled, CreatedAt: member.CreatedAt})
		}
		c.JSON(http.StatusOK, membersResponse{Members: response})
	}
}

func inviteOrganizationMember(service OwnershipService) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := authenticatedUser(c)
		if !ok {
			RespondError(c, 500, "internal_error", "an internal error occurred")
			return
		}
		var request invitationRequest
		if !DecodeJSON(c, &request) {
			return
		}
		invitation, err := service.Invite(c.Request.Context(), user.ID, c.Param("orgId"), request.Email, authorization.Role(request.Role))
		if err != nil {
			respondOwnershipError(c, err)
			return
		}
		c.JSON(http.StatusCreated, invitationResponse{OrganizationID: invitation.OrganizationID, Email: invitation.Email, Role: string(invitation.Role), ExpiresAt: invitation.ExpiresAt})
	}
}

func acceptOrganizationInvitation(service OwnershipService) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := authenticatedUser(c)
		if !ok {
			RespondError(c, 500, "internal_error", "an internal error occurred")
			return
		}
		var request acceptInvitationRequest
		if !DecodeJSON(c, &request) {
			return
		}
		membership, err := service.AcceptInvitation(c.Request.Context(), user, request.Token)
		if err != nil {
			respondOwnershipError(c, err)
			return
		}
		c.JSON(http.StatusCreated, membershipResponse{OrganizationID: membership.OrganizationID, UserID: membership.UserID, Role: membership.Role})
	}
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
				Role: result.Membership.Role, AllowedActions: authorization.AllowedActions(authorization.Role(result.Membership.Role)),
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
				Role:           result.Membership.Role,
				AllowedActions: authorization.AllowedActions(authorization.Role(result.Membership.Role)),
			},
			Environment: environmentResponse{
				ID:             result.Environment.ID,
				OrganizationID: result.Environment.OrganizationID,
				ProjectID:      result.Environment.ProjectID,
				Name:           result.Environment.Name,
				Type:           result.Environment.EnvironmentType,
				Role:           result.Membership.Role,
				AllowedActions: authorization.AllowedActions(authorization.Role(result.Membership.Role)),
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
	case errors.Is(err, ownership.ErrOrganizationNotFound):
		RespondError(c, http.StatusNotFound, "organization_not_found", "organization not found")
	case errors.Is(err, ownership.ErrForbidden):
		RespondError(c, http.StatusForbidden, "permission_denied", "permission denied")
	case errors.Is(err, ownership.ErrAlreadyMember):
		RespondError(c, http.StatusConflict, "already_member", "user is already an organization member")
	case errors.Is(err, ownership.ErrInvalidInvitation):
		RespondError(c, http.StatusBadRequest, "invalid_invitation", "a valid unused invitation is required")
	case errors.Is(err, ownership.ErrEmailNotVerified):
		RespondError(c, http.StatusForbidden, "email_not_verified", "verify your email before accepting invitations")
	case errors.Is(err, ownership.ErrProjectNotFound):
		RespondError(c, http.StatusNotFound, "project_not_found", "project not found")
	case errors.Is(err, ownership.ErrEnvironmentNotFound):
		RespondError(c, http.StatusNotFound, "environment_not_found", "environment not found")
	case errors.Is(err, ownership.ErrMemberNotFound):
		RespondError(c, http.StatusNotFound, "member_not_found", "member not found")
	case errors.Is(err, ownership.ErrDeleteConflict):
		RespondError(c, http.StatusConflict, "resource_not_empty", "resource must be empty before deletion")
	default:
		RespondError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
	}
}
