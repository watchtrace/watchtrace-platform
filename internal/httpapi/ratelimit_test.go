package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRateLimiterReturnsStableEnvelopeAndSeconds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	limiter := NewRateLimiter(RateLimits{ManualPerMinute: 1})
	limiter.now = func() time.Time { return now }
	router := gin.New()
	router.Use(requestIDMiddleware(), limiter.Middleware())
	router.POST("/api/v1/environments/:environmentId/monitors/:monitorId/test", func(c *gin.Context) { c.Status(http.StatusAccepted) })

	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/environments/e/monitors/m/test", nil)
		request.RemoteAddr = "192.0.2.10:1234"
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if attempt == 0 && response.Code != http.StatusAccepted {
			t.Fatalf("first status=%d", response.Code)
		}
		if attempt == 1 {
			if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "60" {
				t.Fatalf("status=%d retry=%q", response.Code, response.Header().Get("Retry-After"))
			}
			var envelope ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "rate_limited" || envelope.Error.RequestID == "" {
				t.Fatalf("body=%s err=%v", response.Body.String(), err)
			}
		}
	}
}

func TestRateLimitClassesAreExplicit(t *testing.T) {
	limiter := NewRateLimiter(RateLimits{})
	tests := []struct {
		method, path, class string
		live                bool
	}{
		{http.MethodPost, "/api/v1/auth/login", "auth", false},
		{http.MethodPost, "/api/v1/environments/e/monitors/m/test", "manual", false},
		{http.MethodPost, "/api/v1/organizations/o/invitations", "invite", false},
		{http.MethodGet, "/api/v1/environments/e/dashboard", "report", false},
		{http.MethodGet, "/api/v1/environments/e/events", "live", true},
		{http.MethodPut, "/api/v1/environments/e", "mutation", false},
	}
	for _, test := range tests {
		class, _, _, live := limiter.classify(test.method, test.path)
		if class != test.class || live != test.live {
			t.Errorf("%s %s class=%q live=%v", test.method, test.path, class, live)
		}
	}
}
