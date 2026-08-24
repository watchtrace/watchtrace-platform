package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestIDIsValidatedAndPropagated(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		suppliedID string
		wantSame   bool
	}{
		{name: "valid incoming ID", suppliedID: "client-request_123", wantSame: true},
		{name: "invalid incoming ID", suppliedID: "invalid/request/id", wantSame: false},
		{name: "missing incoming ID"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := NewRouter(Options{Logger: discardLogger()})
			router.GET("/request-id", func(c *gin.Context) {
				if RequestID(c) != RequestIDFromContext(c.Request.Context()) {
					t.Error("Gin and request contexts received different request IDs")
				}
				c.Status(http.StatusNoContent)
			})

			request := httptest.NewRequest(http.MethodGet, "/request-id", nil)
			if test.suppliedID != "" {
				request.Header.Set(requestIDHeader, test.suppliedID)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			responseID := response.Header().Get(requestIDHeader)
			if responseID == "" {
				t.Fatal("response request ID is empty")
			}
			if !requestIDPattern.MatchString(responseID) {
				t.Fatalf("response request ID %q is invalid", responseID)
			}
			if test.wantSame && responseID != test.suppliedID {
				t.Fatalf("response request ID = %q, want supplied ID %q", responseID, test.suppliedID)
			}
			if !test.wantSame && test.suppliedID != "" && responseID == test.suppliedID {
				t.Fatalf("invalid supplied request ID %q was echoed", test.suppliedID)
			}
		})
	}
}

func TestAccessLogDoesNotExposeRequestSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var logs bytes.Buffer
	router := NewRouter(Options{Logger: testLogger(&logs)})

	request := httptest.NewRequest(http.MethodGet, "/health?token=query-secret", nil)
	request.Header.Set("Authorization", "Bearer authorization-secret")
	request.Header.Set("Cookie", "session=cookie-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	logOutput := logs.String()
	for _, secret := range []string{"query-secret", "authorization-secret", "cookie-secret"} {
		if strings.Contains(logOutput, secret) {
			t.Fatalf("access log exposed %q", secret)
		}
	}
	if !strings.Contains(logOutput, `"route":"/health"`) {
		t.Fatalf("access log did not use the safe route template: %s", logOutput)
	}
}

func TestAccessLogIncludesOnlyValidatedRecordIdentifiers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var logs bytes.Buffer
	router := NewRouter(Options{Logger: testLogger(&logs)})
	router.GET("/records/:environmentId/:monitorId", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodGet, "/records/550e8400-e29b-41d4-a716-446655440000/not-a-record-secret", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if output := logs.String(); !strings.Contains(output, `"environment_id":"550e8400-e29b-41d4-a716-446655440000"`) || strings.Contains(output, "not-a-record-secret") {
		t.Fatalf("unsafe record logging: %s", output)
	}
}

func TestPanicRecoveryDoesNotExposePanicValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const panicSecret = "panic-secret-must-not-escape"
	var logs bytes.Buffer
	router := NewRouter(Options{Logger: testLogger(&logs)})
	router.GET("/panic", func(_ *gin.Context) {
		panic(panicSecret)
	})

	request := httptest.NewRequest(http.MethodGet, "/panic", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	errorResponse := decodeErrorResponse(t, response)
	if errorResponse.Error.Code != "internal_error" {
		t.Fatalf("error code = %q, want internal_error", errorResponse.Error.Code)
	}
	if strings.Contains(response.Body.String(), panicSecret) || strings.Contains(logs.String(), panicSecret) {
		t.Fatal("panic recovery exposed the panic value")
	}
	if !strings.Contains(logs.String(), "panic recovered") {
		t.Fatal("panic recovery did not emit a safe diagnostic log")
	}
}
