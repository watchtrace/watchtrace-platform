package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/watchtrace/watchtrace-platform/internal/realtime"
)

type RealtimeService interface {
	Poll(context.Context, string, string, int64, int) ([]realtime.Event, error)
}

func registerEventRoutes(router *gin.Engine, authenticator SessionAuthenticator, service RealtimeService) {
	router.GET("/api/v1/environments/:environmentId/events", requireAuthenticatedUser(authenticator), streamEvents(service))
}
func streamEvents(service RealtimeService) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := userFrom(c)
		if !ok {
			return
		}
		last, err := realtime.ParseLastID(c.GetHeader("Last-Event-ID"))
		if err != nil {
			RespondError(c, 422, "validation_failed", "Last-Event-ID is invalid")
			return
		}
		initial, err := service.Poll(c.Request.Context(), user, c.Param("environmentId"), last, 100)
		if errors.Is(err, realtime.ErrNotFound) {
			RespondError(c, 404, "event_stream_not_found", "event stream not found")
			return
		}
		if err != nil {
			RespondError(c, 500, "internal_error", "an internal error occurred")
			return
		}
		flusher, ok := c.Writer.(http.Flusher)
		if !ok {
			RespondError(c, 500, "stream_unavailable", "event stream is unavailable")
			return
		}
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-store")
		c.Header("X-Accel-Buffering", "no")
		c.Status(200)
		write := func(events []realtime.Event) bool {
			for _, event := range events {
				data, _ := json.Marshal(gin.H{"resource_type": event.ResourceType, "resource_id": event.ResourceID})
				if _, err = fmt.Fprintf(c.Writer, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Type, data); err != nil {
					return false
				}
				last = event.ID
			}
			flusher.Flush()
			return true
		}
		if !write(initial) {
			return
		}
		ticker := time.NewTicker(time.Second)
		heartbeat := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		defer heartbeat.Stop()
		for {
			select {
			case <-c.Request.Context().Done():
				return
			case <-heartbeat.C:
				if _, err = fmt.Fprint(c.Writer, ": refresh-hint stream\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case <-ticker.C:
				// Use the authenticated identifier captured before streaming. Gin's context
				// must not be queried from another goroutine, and polling remains bounded.
				events, pollErr := service.Poll(c.Request.Context(), user, c.Param("environmentId"), last, 100)
				if pollErr != nil {
					return
				}
				if !write(events) {
					return
				}
			}
		}
	}
}
