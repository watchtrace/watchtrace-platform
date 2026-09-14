package httpapi

import (
	"context"
	"log/slog"

	"github.com/gin-gonic/gin"
)

// testRouterOptions keeps focused handler tests small. Production never uses
// this partial composition path; NewRouter requires every API dependency.
type testRouterOptions struct {
	Logger                     *slog.Logger
	ReadinessCheck             func(context.Context) error
	AuthService                AuthenticationService
	Authenticator              SessionAuthenticator
	OwnershipService           OwnershipService
	OwnershipManagementService OwnershipManagementService
	MonitorService             MonitorService
	BackendService             BackendViewService
	RealtimeService            RealtimeService
	OperationsService          OperationsService
	SecureCookies              bool
	RateLimiter                *RateLimiter
}

func newTestRouter(options testRouterOptions) *gin.Engine {
	router := newBaseRouter(routerSettings{
		Logger:         options.Logger,
		ReadinessCheck: options.ReadinessCheck,
		RateLimiter:    options.RateLimiter,
	})
	if options.AuthService != nil {
		registerAuthRoutes(router, options.AuthService, options.SecureCookies)
	}
	if options.Authenticator != nil {
		registerCurrentUserRoute(router, options.Authenticator)
	}
	if options.Authenticator != nil && options.OwnershipService != nil {
		registerOwnershipRoutes(router, options.Authenticator, options.OwnershipService)
	}
	if options.Authenticator != nil && options.OwnershipManagementService != nil {
		registerTenantManagementRoutes(router, options.Authenticator, options.OwnershipManagementService)
	}
	if options.Authenticator != nil && options.MonitorService != nil {
		registerMonitorRoutes(router, options.Authenticator, options.MonitorService)
	}
	if options.Authenticator != nil && options.BackendService != nil {
		registerBackendViewRoutes(router, options.Authenticator, options.BackendService)
	}
	if options.Authenticator != nil && options.RealtimeService != nil {
		registerEventRoutes(router, options.Authenticator, options.RealtimeService)
	}
	if options.OperationsService != nil {
		registerOperationsRoute(router, options.OperationsService)
	}
	return router
}
