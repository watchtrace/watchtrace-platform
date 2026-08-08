package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type decodeTestRequest struct {
	Name string `json:"name" binding:"required,min=2"`
}

func TestDecodeJSONAcceptsOneValidatedObject(t *testing.T) {
	router := decodeTestRouter()
	request := httptest.NewRequest(http.MethodPost, "/decode", strings.NewReader(`{"name":"monitor"}`))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusNoContent, response.Body)
	}
}

func TestDecodeJSONRejectsInvalidBodies(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantCode    string
	}{
		{name: "missing content type", body: `{"name":"monitor"}`, wantStatus: http.StatusUnsupportedMediaType, wantCode: "unsupported_media_type"},
		{name: "wrong content type", contentType: "text/plain", body: `{"name":"monitor"}`, wantStatus: http.StatusUnsupportedMediaType, wantCode: "unsupported_media_type"},
		{name: "empty body", contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "malformed JSON", contentType: "application/json", body: `{"name":`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "unknown field", contentType: "application/json", body: `{"name":"monitor","secret_field":"hidden"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "multiple values", contentType: "application/json", body: `{"name":"monitor"}{"name":"second"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "validation failure", contentType: "application/json", body: `{"name":""}`, wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_failed"},
		{name: "oversized body", contentType: "application/json", body: `{"name":"` + strings.Repeat("a", int(maxJSONBodyBytes)) + `"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/decode", strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()

			decodeTestRouter().ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body)
			}
			errorResponse := decodeErrorResponse(t, response)
			if errorResponse.Error.Code != test.wantCode {
				t.Fatalf("error code = %q, want %q", errorResponse.Error.Code, test.wantCode)
			}
			if strings.Contains(response.Body.String(), "secret_field") || strings.Contains(response.Body.String(), "hidden") {
				t.Fatal("validation response exposed request body content")
			}
		})
	}
}

func decodeTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := NewRouter(Options{Logger: discardLogger()})
	router.POST("/decode", func(c *gin.Context) {
		var request decodeTestRequest
		if !DecodeJSON(c, &request) {
			return
		}
		c.Status(http.StatusNoContent)
	})
	return router
}
