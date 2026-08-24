package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
)

type OwnershipManagementService interface {
	OwnershipService
	ListOrganizations(context.Context, string) ([]ownership.OrganizationView, error)
	GetOrganization(context.Context, string, string) (ownership.OrganizationView, error)
	UpdateOrganization(context.Context, string, string, string) (ownership.OrganizationView, error)
	DeleteOrganization(context.Context, string, string) error
	ListProjects(context.Context, string, string) ([]ownership.TenantProject, error)
	GetProject(context.Context, string, string) (ownership.TenantProject, error)
	CreateProject(context.Context, string, string, string, string) (ownership.TenantProject, error)
	UpdateProject(context.Context, string, string, string, string) (ownership.TenantProject, error)
	DeleteProject(context.Context, string, string) error
	ListEnvironments(context.Context, string, string) ([]ownership.TenantEnvironment, error)
	GetEnvironment(context.Context, string, string) (ownership.TenantEnvironment, error)
	CreateEnvironment(context.Context, string, string, string, string) (ownership.TenantEnvironment, error)
	UpdateEnvironment(context.Context, string, string, string, string) (ownership.TenantEnvironment, error)
	DeleteEnvironment(context.Context, string, string) error
	UpdateMember(context.Context, string, string, string, authorization.Role, *bool) (ownership.Member, error)
	RemoveMember(context.Context, string, string, string) error
}

type tenantNameRequest struct {
	Name string `json:"name" binding:"required,max=120"`
}
type projectMutationRequest struct {
	Name        string `json:"name" binding:"required,max=120"`
	Description string `json:"description" binding:"max=1000"`
}
type environmentMutationRequest struct {
	Name string `json:"name" binding:"required,max=120"`
	Type string `json:"type" binding:"required,oneof=production staging development"`
}
type memberMutationRequest struct {
	Role                         string `json:"role" binding:"omitempty,oneof=admin member viewer"`
	IncidentNotificationsEnabled *bool  `json:"incident_notifications_enabled"`
}

func registerTenantManagementRoutes(router *gin.Engine, authenticator SessionAuthenticator, service OwnershipManagementService) {
	authn := requireAuthenticatedUser(authenticator)
	router.GET("/api/v1/organizations", authn, listOrganizations(service))
	router.GET("/api/v1/organizations/:orgId", authn, getOrganization(service))
	router.PUT("/api/v1/organizations/:orgId", authn, updateOrganization(service))
	router.DELETE("/api/v1/organizations/:orgId", authn, deleteOrganization(service))
	router.GET("/api/v1/organizations/:orgId/projects", authn, listProjects(service))
	router.POST("/api/v1/organizations/:orgId/projects", authn, createProject(service))
	router.GET("/api/v1/projects/:projectId", authn, getProject(service))
	router.PUT("/api/v1/projects/:projectId", authn, updateProject(service))
	router.DELETE("/api/v1/projects/:projectId", authn, deleteProject(service))
	router.GET("/api/v1/projects/:projectId/environments", authn, listEnvironments(service))
	router.POST("/api/v1/projects/:projectId/environments", authn, createEnvironment(service))
	router.GET("/api/v1/environments/:environmentId", authn, getEnvironment(service))
	router.PUT("/api/v1/environments/:environmentId", authn, updateEnvironment(service))
	router.DELETE("/api/v1/environments/:environmentId", authn, deleteEnvironment(service))
	router.PATCH("/api/v1/organizations/:orgId/members/:memberId", authn, updateMember(service))
	router.DELETE("/api/v1/organizations/:orgId/members/:memberId", authn, removeMember(service))
}

func userFrom(c *gin.Context) (string, bool) {
	u, ok := authenticatedUser(c)
	if !ok {
		RespondError(c, 500, "internal_error", "an internal error occurred")
		return "", false
	}
	return u.ID, true
}
func listOrganizations(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		v, e := s.ListOrganizations(c, u)
		if e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.JSON(200, gin.H{"organizations": v})
	}
}
func getOrganization(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		v, e := s.GetOrganization(c, u, c.Param("orgId"))
		if e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.JSON(200, v)
	}
}
func updateOrganization(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		var r tenantNameRequest
		if !DecodeJSON(c, &r) {
			return
		}
		v, e := s.UpdateOrganization(c, u, c.Param("orgId"), r.Name)
		if e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.JSON(200, v)
	}
}
func deleteOrganization(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		if e := s.DeleteOrganization(c, u, c.Param("orgId")); e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.Status(204)
	}
}
func listProjects(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		v, e := s.ListProjects(c, u, c.Param("orgId"))
		if e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.JSON(200, gin.H{"projects": v})
	}
}
func createProject(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		var r projectMutationRequest
		if !DecodeJSON(c, &r) {
			return
		}
		v, e := s.CreateProject(c, u, c.Param("orgId"), r.Name, r.Description)
		if e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.JSON(201, v)
	}
}
func getProject(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		v, e := s.GetProject(c, u, c.Param("projectId"))
		if e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.JSON(http.StatusOK, v)
	}
}
func updateProject(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		var r projectMutationRequest
		if !DecodeJSON(c, &r) {
			return
		}
		v, e := s.UpdateProject(c, u, c.Param("projectId"), r.Name, r.Description)
		if e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.JSON(200, v)
	}
}
func deleteProject(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		if e := s.DeleteProject(c, u, c.Param("projectId")); e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.Status(204)
	}
}
func listEnvironments(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		v, e := s.ListEnvironments(c, u, c.Param("projectId"))
		if e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.JSON(200, gin.H{"environments": v})
	}
}
func createEnvironment(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		var r environmentMutationRequest
		if !DecodeJSON(c, &r) {
			return
		}
		v, e := s.CreateEnvironment(c, u, c.Param("projectId"), r.Name, r.Type)
		if e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.JSON(201, v)
	}
}
func getEnvironment(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		v, e := s.GetEnvironment(c, u, c.Param("environmentId"))
		if e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.JSON(http.StatusOK, v)
	}
}
func updateEnvironment(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		var r environmentMutationRequest
		if !DecodeJSON(c, &r) {
			return
		}
		v, e := s.UpdateEnvironment(c, u, c.Param("environmentId"), r.Name, r.Type)
		if e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.JSON(200, v)
	}
}
func deleteEnvironment(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		if e := s.DeleteEnvironment(c, u, c.Param("environmentId")); e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.Status(204)
	}
}
func updateMember(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		var r memberMutationRequest
		if !DecodeJSON(c, &r) {
			return
		}
		if r.Role == "" && r.IncidentNotificationsEnabled == nil {
			RespondError(c, 422, "validation_failed", "at least one member setting is required")
			return
		}
		v, e := s.UpdateMember(c, u, c.Param("orgId"), c.Param("memberId"), authorization.Role(r.Role), r.IncidentNotificationsEnabled)
		if e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.JSON(200, memberResponse{UserID: v.UserID, Email: v.Email, Role: string(v.Role), IncidentNotificationsEnabled: v.IncidentNotificationsEnabled, CreatedAt: v.CreatedAt})
	}
}
func removeMember(s OwnershipManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		if e := s.RemoveMember(c, u, c.Param("orgId"), c.Param("memberId")); e != nil {
			respondOwnershipError(c, e)
			return
		}
		c.Status(http.StatusNoContent)
	}
}
