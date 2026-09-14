package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/backendapi"
	"github.com/watchtrace/watchtrace-platform/internal/monitor"
	"github.com/watchtrace/watchtrace-platform/internal/operations"
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
	"github.com/watchtrace/watchtrace-platform/internal/realtime"
)

// Options contains every dependency required by the API process. Service
// pointers are concrete here because this is the application's composition
// boundary; the individual handlers still depend on their small interfaces.
type Options struct {
	Logger            *slog.Logger
	ReadinessCheck    func(context.Context) error
	AuthService       *auth.Service
	OwnershipService  *ownership.Service
	MonitorService    *monitor.Service
	BackendService    *backendapi.Service
	RealtimeService   *realtime.Service
	OperationsService *operations.Service
	SecureCookies     bool
	RateLimiter       *RateLimiter
}

// NewRouter assembles the complete HTTP API. It rejects incomplete startup
// configuration instead of silently omitting routes.
func NewRouter(options Options) (*gin.Engine, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}

	router := newBaseRouter(routerSettings{
		Logger:         options.Logger,
		ReadinessCheck: options.ReadinessCheck,
		RateLimiter:    options.RateLimiter,
	})
	registerAuthRoutes(router, options.AuthService, options.SecureCookies)
	registerCurrentUserRoute(router, options.AuthService)
	registerOwnershipRoutes(router, options.AuthService, options.OwnershipService)
	registerTenantManagementRoutes(router, options.AuthService, options.OwnershipService)
	registerMonitorRoutes(router, options.AuthService, options.MonitorService)
	registerBackendViewRoutes(router, options.AuthService, options.BackendService)
	registerEventRoutes(router, options.AuthService, options.RealtimeService)
	registerOperationsRoute(router, options.OperationsService)
	return router, nil
}

func (options Options) validate() error {
	switch {
	case options.ReadinessCheck == nil:
		return errors.New("httpapi: readiness check is required")
	case options.AuthService == nil:
		return errors.New("httpapi: auth service is required")
	case options.OwnershipService == nil:
		return errors.New("httpapi: ownership service is required")
	case options.MonitorService == nil:
		return errors.New("httpapi: monitor service is required")
	case options.BackendService == nil:
		return errors.New("httpapi: backend service is required")
	case options.RealtimeService == nil:
		return errors.New("httpapi: realtime service is required")
	case options.OperationsService == nil:
		return errors.New("httpapi: operations service is required")
	default:
		return nil
	}
}

type routerSettings struct {
	Logger         *slog.Logger
	ReadinessCheck func(context.Context) error
	RateLimiter    *RateLimiter
}

func newBaseRouter(settings routerSettings) *gin.Engine {
	logger := settings.Logger
	if logger == nil {
		logger = slog.Default()
	}

	router := gin.New()
	router.HandleMethodNotAllowed = true
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.Use(
		requestIDMiddleware(),
		accessLogMiddleware(logger),
		recoveryMiddleware(logger),
	)
	router.Use(func(c *gin.Context) {
		c.Header("X-WatchTrace-Service", "api")
		c.Next()
	})
	limiter := settings.RateLimiter
	if limiter == nil {
		limiter = NewRateLimiter(RateLimits{})
	}
	router.Use(limiter.Middleware())

	liveness := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
	router.GET("/health", liveness)
	router.GET("/health/live", liveness)
	router.GET("/health/ready", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if settings.ReadinessCheck != nil {
			if err := settings.ReadinessCheck(c.Request.Context()); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	router.NoRoute(func(c *gin.Context) {
		RespondError(c, http.StatusNotFound, "not_found", "resource not found")
	})
	router.NoMethod(func(c *gin.Context) {
		RespondError(c, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})

	return router
}
