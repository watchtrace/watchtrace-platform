package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/watchtrace/watchtrace-platform/internal/backendapi"
)

type BackendViewService interface {
	ListChecks(context.Context, string, string, string, backendapi.PageQuery) (backendapi.CheckPage, error)
	MonitorReport(context.Context, string, string, string, backendapi.PageQuery) (backendapi.Report, error)
	Dashboard(context.Context, string, string, backendapi.PageQuery) (backendapi.Dashboard, error)
	ListIncidents(context.Context, string, string, backendapi.PageQuery) (backendapi.IncidentPage, error)
	GetIncident(context.Context, string, string, string) (backendapi.IncidentSummary, error)
	Acknowledge(context.Context, string, string, string, string) (backendapi.IncidentSummary, error)
	Resolve(context.Context, string, string, string, string) (backendapi.IncidentSummary, error)
}

type incidentActionRequest struct {
	Reason string `json:"reason" binding:"max=500"`
}

func registerBackendViewRoutes(router *gin.Engine, authenticator SessionAuthenticator, service BackendViewService) {
	authn := requireAuthenticatedUser(authenticator)
	router.GET("/api/v1/environments/:environmentId/monitors/:monitorId/checks", authn, listChecks(service))
	router.GET("/api/v1/environments/:environmentId/monitors/:monitorId/report", authn, monitorReport(service))
	router.GET("/api/v1/environments/:environmentId/dashboard", authn, dashboard(service))
	router.GET("/api/v1/environments/:environmentId/incidents", authn, listIncidents(service))
	router.GET("/api/v1/environments/:environmentId/incidents/:incidentId", authn, getIncident(service))
	router.POST("/api/v1/environments/:environmentId/incidents/:incidentId/acknowledge", authn, incidentAction(service, true))
	router.POST("/api/v1/environments/:environmentId/incidents/:incidentId/resolve", authn, incidentAction(service, false))
}

func boundedQuery(c *gin.Context) (backendapi.PageQuery, error) {
	q := backendapi.PageQuery{Cursor: c.Query("cursor"), Status: c.Query("status"), JobType: c.Query("job_type")}
	if len(q.Cursor) > 256 || len(q.Status) > 16 || len(q.JobType) > 16 {
		return q, backendapi.ErrInvalidQuery
	}
	if value := c.Query("limit"); value != "" {
		n, e := strconv.Atoi(value)
		if e != nil {
			return q, backendapi.ErrInvalidQuery
		}
		q.Limit = n
	}
	var err error
	fromValue, toValue := c.Query("from"), c.Query("to")
	if fromValue == "" || toValue == "" {
		return q, backendapi.ErrInvalidQuery
	}
	if value := fromValue; value != "" {
		q.From, err = time.Parse(time.RFC3339, value)
		if err != nil {
			return q, backendapi.ErrInvalidQuery
		}
	}
	if value := toValue; value != "" {
		q.To, err = time.Parse(time.RFC3339, value)
		if err != nil {
			return q, backendapi.ErrInvalidQuery
		}
	}
	return q, nil
}
func listChecks(s BackendViewService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		q, e := boundedQuery(c)
		if e != nil {
			respondBackendError(c, e)
			return
		}
		if q.JobType != "" && q.JobType != "scheduled" && q.JobType != "manual" {
			respondBackendError(c, backendapi.ErrInvalidQuery)
			return
		}
		if q.JobType == "manual" {
			q.JobType = "manual_test"
		}
		v, e := s.ListChecks(c, u, c.Param("environmentId"), c.Param("monitorId"), q)
		if e != nil {
			respondBackendError(c, e)
			return
		}
		c.JSON(200, v)
	}
}
func monitorReport(s BackendViewService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		q, e := boundedQuery(c)
		if e != nil {
			respondBackendError(c, e)
			return
		}
		v, e := s.MonitorReport(c, u, c.Param("environmentId"), c.Param("monitorId"), q)
		if e != nil {
			respondBackendError(c, e)
			return
		}
		c.JSON(200, v)
	}
}
func dashboard(s BackendViewService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		q, e := boundedQuery(c)
		if e != nil {
			respondBackendError(c, e)
			return
		}
		v, e := s.Dashboard(c, u, c.Param("environmentId"), q)
		if e != nil {
			respondBackendError(c, e)
			return
		}
		c.JSON(200, v)
	}
}
func listIncidents(s BackendViewService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		q, e := boundedQuery(c)
		if e != nil {
			respondBackendError(c, e)
			return
		}
		v, e := s.ListIncidents(c, u, c.Param("environmentId"), q)
		if e != nil {
			respondBackendError(c, e)
			return
		}
		c.JSON(200, v)
	}
}
func getIncident(s BackendViewService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		v, e := s.GetIncident(c, u, c.Param("environmentId"), c.Param("incidentId"))
		if e != nil {
			respondBackendError(c, e)
			return
		}
		c.JSON(200, v)
	}
}
func incidentAction(s BackendViewService, ack bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := userFrom(c)
		if !ok {
			return
		}
		var r incidentActionRequest
		if !DecodeJSON(c, &r) {
			return
		}
		var v backendapi.IncidentSummary
		var e error
		if ack {
			v, e = s.Acknowledge(c, u, c.Param("environmentId"), c.Param("incidentId"), r.Reason)
		} else {
			v, e = s.Resolve(c, u, c.Param("environmentId"), c.Param("incidentId"), r.Reason)
		}
		if e != nil {
			respondBackendError(c, e)
			return
		}
		c.JSON(200, v)
	}
}
func respondBackendError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, backendapi.ErrInvalidQuery):
		RespondError(c, http.StatusUnprocessableEntity, "validation_failed", "pagination, filters, or time range are invalid")
	case errors.Is(err, backendapi.ErrForbidden):
		RespondError(c, http.StatusForbidden, "permission_denied", "permission denied")
	case errors.Is(err, backendapi.ErrNotFound):
		RespondError(c, http.StatusNotFound, "resource_not_found", "resource not found")
	default:
		RespondError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
	}
}
