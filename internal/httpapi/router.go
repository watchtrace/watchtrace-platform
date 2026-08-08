package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Options contains the dependencies used by the HTTP router.
type Options struct {
	Logger           *slog.Logger
	ReadinessCheck   func(context.Context) error
	AuthService      AuthenticationService
	Authenticator    SessionAuthenticator
	OwnershipService OwnershipService
	MonitorService   MonitorService
}

// NewRouter assembles the HTTP routes owned by the API command.
func NewRouter(options Options) *gin.Engine {
	logger := options.Logger
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

	liveness := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
	router.GET("/health", liveness)
	router.GET("/health/live", liveness)
	router.GET("/health/ready", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if options.ReadinessCheck != nil {
			if err := options.ReadinessCheck(c.Request.Context()); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	if options.AuthService != nil {
		registerAuthRoutes(router, options.AuthService)
	}
	if options.Authenticator != nil && options.OwnershipService != nil {
		registerOwnershipRoutes(router, options.Authenticator, options.OwnershipService)
	}
	if options.Authenticator != nil && options.MonitorService != nil {
		registerMonitorRoutes(router, options.Authenticator, options.MonitorService)
	}

	router.NoRoute(func(c *gin.Context) {
		RespondError(c, http.StatusNotFound, "not_found", "resource not found")
	})
	router.NoMethod(func(c *gin.Context) {
		RespondError(c, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})

	return router
}
