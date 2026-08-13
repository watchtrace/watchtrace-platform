package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/watchtrace/watchtrace-platform/internal/monitor"
)

// MonitorService is the initial monitor boundary used by the HTTP API.
type MonitorService interface {
	Create(context.Context, string, string, monitor.CreateInput) (monitor.Monitor, error)
	List(context.Context, string, string) ([]monitor.Monitor, error)
	Get(context.Context, string, string, string) (monitor.Detail, error)
}
type monitorLifecycleService interface {
	MonitorService
	Update(context.Context, string, string, string, monitor.UpdateInput) (monitor.Monitor, error)
	Delete(context.Context, string, string, string) error
	Pause(context.Context, string, string, string) (monitor.Monitor, error)
	Resume(context.Context, string, string, string) (monitor.Monitor, error)
	TestNow(context.Context, string, string, string) (string, error)
}

type createMonitorRequest struct {
	Name              string            `json:"name" binding:"required,max=120"`
	URL               string            `json:"url" binding:"required,max=2048"`
	IntervalSeconds   *int32            `json:"interval_seconds" binding:"omitempty,oneof=60 120 300 600 1800"`
	TimeoutSeconds    *int32            `json:"timeout_seconds" binding:"omitempty,min=1,max=10"`
	ExpectedStatusMin *int16            `json:"expected_status_min" binding:"omitempty,min=100,max=599"`
	ExpectedStatusMax *int16            `json:"expected_status_max" binding:"omitempty,min=100,max=599"`
	Method            string            `json:"method" binding:"omitempty,oneof=GET HEAD"`
	Headers           map[string]string `json:"headers" binding:"omitempty,max=32"`
	WorkerPoolID      string            `json:"worker_pool_id" binding:"omitempty,max=63"`
}

type monitorResponse struct {
	ID                string    `json:"id"`
	OrganizationID    string    `json:"organization_id"`
	EnvironmentID     string    `json:"environment_id"`
	Name              string    `json:"name"`
	URL               string    `json:"url"`
	Method            string    `json:"method"`
	IntervalSeconds   int32     `json:"interval_seconds"`
	TimeoutSeconds    int32     `json:"timeout_seconds"`
	ExpectedStatusMin int16     `json:"expected_status_min"`
	ExpectedStatusMax int16     `json:"expected_status_max"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	Version           int64     `json:"version"`
	Paused            bool      `json:"paused"`
	WorkerPoolID      string    `json:"worker_pool_id"`
	HeaderNames       []string  `json:"header_names"`
}

type monitorListResponse struct {
	Monitors []monitorResponse `json:"monitors"`
}

type monitorDetailResponse struct {
	monitorResponse
	State        monitor.State          `json:"state"`
	RecentChecks []monitorCheckResponse `json:"recent_checks"`
}

type monitorCheckResponse struct {
	JobID                     string    `json:"job_id"`
	JobType                   string    `json:"job_type"`
	ScheduledAt               time.Time `json:"scheduled_at"`
	StartedAt                 time.Time `json:"started_at"`
	CompletedAt               time.Time `json:"completed_at"`
	Succeeded                 bool      `json:"succeeded"`
	StatusCode                *int16    `json:"status_code"`
	ErrorCategory             *string   `json:"error_category"`
	TotalDurationMicroseconds int64     `json:"total_duration_microseconds"`
}

func registerMonitorRoutes(
	router *gin.Engine,
	authenticator SessionAuthenticator,
	service MonitorService,
) {
	router.POST(
		"/api/v1/environments/:environmentId/monitors",
		requireAuthenticatedUser(authenticator),
		createMonitor(service),
	)
	router.GET(
		"/api/v1/environments/:environmentId/monitors",
		requireAuthenticatedUser(authenticator),
		listMonitors(service),
	)
	if lifecycle, ok := service.(monitorLifecycleService); ok {
		router.PUT("/api/v1/environments/:environmentId/monitors/:monitorId", requireAuthenticatedUser(authenticator), updateMonitor(lifecycle))
		router.DELETE("/api/v1/environments/:environmentId/monitors/:monitorId", requireAuthenticatedUser(authenticator), deleteMonitor(lifecycle))
		router.POST("/api/v1/environments/:environmentId/monitors/:monitorId/pause", requireAuthenticatedUser(authenticator), pauseMonitor(lifecycle, true))
		router.POST("/api/v1/environments/:environmentId/monitors/:monitorId/resume", requireAuthenticatedUser(authenticator), pauseMonitor(lifecycle, false))
		router.POST("/api/v1/environments/:environmentId/monitors/:monitorId/test", requireAuthenticatedUser(authenticator), testMonitor(lifecycle))
	}
	router.GET(
		"/api/v1/environments/:environmentId/monitors/:monitorId",
		requireAuthenticatedUser(authenticator),
		getMonitor(service),
	)
}

func createMonitor(service MonitorService) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := authenticatedUser(c)
		if !ok {
			RespondError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
			return
		}

		var request createMonitorRequest
		if !DecodeJSON(c, &request) {
			return
		}

		created, err := service.Create(c.Request.Context(), user.ID, c.Param("environmentId"), monitor.CreateInput{
			Name:              request.Name,
			TargetURL:         request.URL,
			IntervalSeconds:   optionalInt32(request.IntervalSeconds),
			TimeoutSeconds:    optionalInt32(request.TimeoutSeconds),
			ExpectedStatusMin: optionalInt16(request.ExpectedStatusMin),
			ExpectedStatusMax: optionalInt16(request.ExpectedStatusMax),
			Method:            request.Method, Headers: request.Headers, WorkerPoolID: request.WorkerPoolID,
		})
		if err != nil {
			respondMonitorError(c, err)
			return
		}

		c.JSON(http.StatusCreated, monitorToResponse(created))
	}
}

func updateMonitor(service monitorLifecycleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := authenticatedUser(c)
		if !ok {
			RespondError(c, 500, "internal_error", "an internal error occurred")
			return
		}
		var request createMonitorRequest
		if !DecodeJSON(c, &request) {
			return
		}
		item, err := service.Update(c.Request.Context(), user.ID, c.Param("environmentId"), c.Param("monitorId"), monitor.UpdateInput{Name: request.Name, TargetURL: request.URL, Method: request.Method, Headers: request.Headers, WorkerPoolID: request.WorkerPoolID, IntervalSeconds: optionalInt32(request.IntervalSeconds), TimeoutSeconds: optionalInt32(request.TimeoutSeconds), ExpectedStatusMin: optionalInt16(request.ExpectedStatusMin), ExpectedStatusMax: optionalInt16(request.ExpectedStatusMax)})
		if err != nil {
			respondMonitorError(c, err)
			return
		}
		c.JSON(http.StatusOK, monitorToResponse(item))
	}
}
func deleteMonitor(service monitorLifecycleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := authenticatedUser(c)
		if !ok {
			RespondError(c, 500, "internal_error", "an internal error occurred")
			return
		}
		if err := service.Delete(c.Request.Context(), user.ID, c.Param("environmentId"), c.Param("monitorId")); err != nil {
			respondMonitorError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}
func pauseMonitor(service monitorLifecycleService, paused bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := authenticatedUser(c)
		if !ok {
			RespondError(c, 500, "internal_error", "an internal error occurred")
			return
		}
		var item monitor.Monitor
		var err error
		if paused {
			item, err = service.Pause(c.Request.Context(), user.ID, c.Param("environmentId"), c.Param("monitorId"))
		} else {
			item, err = service.Resume(c.Request.Context(), user.ID, c.Param("environmentId"), c.Param("monitorId"))
		}
		if err != nil {
			respondMonitorError(c, err)
			return
		}
		c.JSON(http.StatusOK, monitorToResponse(item))
	}
}
func testMonitor(service monitorLifecycleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := authenticatedUser(c)
		if !ok {
			RespondError(c, 500, "internal_error", "an internal error occurred")
			return
		}
		id, err := service.TestNow(c.Request.Context(), user.ID, c.Param("environmentId"), c.Param("monitorId"))
		if err != nil {
			respondMonitorError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"job_id": id})
	}
}

func optionalInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

func optionalInt16(value *int16) int16 {
	if value == nil {
		return 0
	}
	return *value
}

func listMonitors(service MonitorService) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := authenticatedUser(c)
		if !ok {
			RespondError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
			return
		}

		monitors, err := service.List(c.Request.Context(), user.ID, c.Param("environmentId"))
		if err != nil {
			respondMonitorError(c, err)
			return
		}

		response := make([]monitorResponse, 0, len(monitors))
		for _, item := range monitors {
			response = append(response, monitorToResponse(item))
		}
		c.JSON(http.StatusOK, monitorListResponse{Monitors: response})
	}
}

func getMonitor(service MonitorService) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := authenticatedUser(c)
		if !ok {
			RespondError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
			return
		}

		detail, err := service.Get(
			c.Request.Context(),
			user.ID,
			c.Param("environmentId"),
			c.Param("monitorId"),
		)
		if err != nil {
			respondMonitorError(c, err)
			return
		}

		checks := make([]monitorCheckResponse, 0, len(detail.RecentResults))
		for _, result := range detail.RecentResults {
			checks = append(checks, monitorCheckToResponse(result))
		}
		c.JSON(http.StatusOK, monitorDetailResponse{
			monitorResponse: monitorToResponse(detail.Monitor),
			State:           detail.State,
			RecentChecks:    checks,
		})
	}
}

func monitorToResponse(item monitor.Monitor) monitorResponse {
	return monitorResponse{
		ID:                item.ID,
		OrganizationID:    item.OrganizationID,
		EnvironmentID:     item.EnvironmentID,
		Name:              item.Name,
		URL:               item.TargetURL,
		Method:            item.Method,
		IntervalSeconds:   item.IntervalSeconds,
		TimeoutSeconds:    item.TimeoutSeconds,
		ExpectedStatusMin: item.ExpectedStatusMin,
		ExpectedStatusMax: item.ExpectedStatusMax,
		CreatedAt:         item.CreatedAt,
		UpdatedAt:         item.UpdatedAt,
		Version:           item.Version, Paused: item.Paused, WorkerPoolID: item.WorkerPoolID, HeaderNames: item.HeaderNames,
	}
}

func monitorCheckToResponse(result monitor.CheckResult) monitorCheckResponse {
	return monitorCheckResponse{
		JobID:                     result.JobID,
		JobType:                   result.JobType,
		ScheduledAt:               result.ScheduledAt,
		StartedAt:                 result.StartedAt,
		CompletedAt:               result.CompletedAt,
		Succeeded:                 result.Succeeded,
		StatusCode:                result.StatusCode,
		ErrorCategory:             result.ErrorCategory,
		TotalDurationMicroseconds: result.TotalDurationMicroseconds,
	}
}

func respondMonitorError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, monitor.ErrInvalidInput):
		RespondError(c, http.StatusUnprocessableEntity, "validation_failed", "monitor configuration is invalid")
	case errors.Is(err, monitor.ErrEnvironmentNotFound):
		RespondError(c, http.StatusNotFound, "environment_not_found", "environment not found")
	case errors.Is(err, monitor.ErrMonitorNotFound):
		RespondError(c, http.StatusNotFound, "monitor_not_found", "monitor not found")
	case errors.Is(err, monitor.ErrMonitorLimitReached):
		RespondError(c, http.StatusConflict, "monitor_limit_reached", "organization monitor limit reached")
	case errors.Is(err, monitor.ErrManualQueueFull):
		RespondError(c, http.StatusTooManyRequests, "manual_queue_full", "manual check queue is full")
	case errors.Is(err, monitor.ErrQueueUnavailable):
		RespondError(c, http.StatusServiceUnavailable, "monitor_queue_unavailable", "monitor queue is unavailable")
	case errors.Is(err, monitor.ErrForbidden):
		RespondError(c, http.StatusForbidden, "permission_denied", "permission denied")
	default:
		RespondError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
	}
}
