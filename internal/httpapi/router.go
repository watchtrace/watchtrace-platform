package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// NewRouter assembles the HTTP routes owned by the API command.
func NewRouter() *gin.Engine {
	router := gin.New()
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	return router
}
