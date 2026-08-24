package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	requestIDHeader = "X-Request-ID"
	requestIDKey    = "request_id"
)

type requestIDContextKey struct{}

var (
	requestIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	safeRecordIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
	fallbackIDCounter   atomic.Uint64
)

func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader(requestIDHeader)
		if !requestIDPattern.MatchString(requestID) {
			requestID = newRequestID()
		}

		c.Set(requestIDKey, requestID)
		c.Header(requestIDHeader, requestID)
		requestContext := context.WithValue(c.Request.Context(), requestIDContextKey{}, requestID)
		c.Request = c.Request.WithContext(requestContext)
		c.Next()
	}
}

// RequestID returns the validated request ID assigned by the router.
func RequestID(c *gin.Context) string {
	return c.GetString(requestIDKey)
}

// RequestIDFromContext returns a request ID propagated to application code.
func RequestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return requestID
}

func newRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		value[6] = (value[6] & 0x0f) | 0x40
		value[8] = (value[8] & 0x3f) | 0x80
		encoded := make([]byte, 36)
		hex.Encode(encoded[0:8], value[0:4])
		encoded[8] = '-'
		hex.Encode(encoded[9:13], value[4:6])
		encoded[13] = '-'
		hex.Encode(encoded[14:18], value[6:8])
		encoded[18] = '-'
		hex.Encode(encoded[19:23], value[8:10])
		encoded[23] = '-'
		hex.Encode(encoded[24:36], value[10:16])
		return string(encoded)
	}

	return fmt.Sprintf("fallback-%x-%x", time.Now().UTC().UnixNano(), fallbackIDCounter.Add(1))
}

func accessLogMiddleware(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		startedAt := time.Now()
		c.Next()

		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}

		level := slog.LevelInfo
		if c.Writer.Status() >= http.StatusInternalServerError {
			level = slog.LevelError
		} else if c.Writer.Status() >= http.StatusBadRequest {
			level = slog.LevelWarn
		}

		attributes := []any{
			"component", "http",
			"request_id", RequestID(c),
			"method", c.Request.Method,
			"route", route,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(startedAt).Milliseconds(),
		}
		keys := map[string]string{"orgId": "organization_id", "projectId": "project_id", "environmentId": "environment_id", "monitorId": "monitor_id", "incidentId": "incident_id", "memberId": "member_id"}
		for _, parameter := range c.Params {
			if key := keys[parameter.Key]; key != "" && safeRecordIDPattern.MatchString(parameter.Value) {
				attributes = append(attributes, key, strings.ToLower(parameter.Value))
			}
		}
		logger.Log(c.Request.Context(), level, "request completed", attributes...)
	}
}

func recoveryMiddleware(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.ErrorContext(c.Request.Context(), "panic recovered",
					"component", "http",
					"request_id", RequestID(c),
					"stack", string(debug.Stack()),
				)

				if !c.Writer.Written() {
					RespondError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
					return
				}
				c.Abort()
			}
		}()

		c.Next()
	}
}
