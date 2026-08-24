package httpapi

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/watchtrace/watchtrace-platform/internal/operations"
	"net/http"
)

type OperationsService interface {
	Read(context.Context) (operations.Health, error)
}

func registerOperationsRoute(router *gin.Engine, service OperationsService) {
	router.GET("/health/operations", func(c *gin.Context) {
		h, err := service.Read(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, h)
	})
}
