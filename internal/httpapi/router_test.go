package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHealthEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		path       string
		wantStatus string
	}{
		{path: "/health", wantStatus: "ok"},
		{path: "/health/live", wantStatus: "ok"},
		{path: "/health/ready", wantStatus: "ready"},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()

			NewRouter(Options{Logger: discardLogger()}).ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if contentType := response.Header().Get("Content-Type"); contentType != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", contentType)
			}
			if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", cacheControl)
			}
			if requestID := response.Header().Get(requestIDHeader); requestID == "" {
				t.Fatal("response did not include a request ID")
			}

			var body struct {
				Status string `json:"status"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Status != test.wantStatus {
				t.Fatalf("status body = %q, want %q", body.Status, test.wantStatus)
			}
		})
	}
}

func TestReadinessFailureIsSafe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "database-password-must-not-escape"
	var logs bytes.Buffer
	router := NewRouter(Options{
		Logger: testLogger(&logs),
		ReadinessCheck: func(_ context.Context) error {
			return errors.New(secret)
		},
	})

	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if body := response.Body.String(); body != "{\"status\":\"not_ready\"}" {
		t.Fatalf("body = %q, want safe not-ready response", body)
	}
	if strings.Contains(response.Body.String(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatal("readiness failure exposed its internal error")
	}
}

func TestRoutingErrorsUseStandardEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := NewRouter(Options{Logger: discardLogger()})

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{name: "not found", method: http.MethodGet, path: "/missing", wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "trailing slash is not redirected", method: http.MethodGet, path: "/health/", wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "method not allowed", method: http.MethodPost, path: "/health", wantStatus: http.StatusMethodNotAllowed, wantCode: "method_not_allowed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			errorResponse := decodeErrorResponse(t, response)
			if errorResponse.Error.Code != test.wantCode {
				t.Fatalf("error code = %q, want %q", errorResponse.Error.Code, test.wantCode)
			}
			if errorResponse.Error.RequestID == "" {
				t.Fatal("error response did not include a request ID")
			}
			if headerID := response.Header().Get(requestIDHeader); headerID != errorResponse.Error.RequestID {
				t.Fatalf("header request ID = %q, body request ID = %q", headerID, errorResponse.Error.RequestID)
			}
		})
	}
}

func decodeErrorResponse(t *testing.T, response *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var body ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	return body
}

func testLogger(output *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(output, nil))
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
}
