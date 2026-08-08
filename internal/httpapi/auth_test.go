package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
)

func TestAuthEndpointsReturnSafeSessionResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		password     = "a-local-test-password"
		accessToken  = "wt_access_safe-test-token"
		refreshToken = "wt_refresh_safe-test-token"
	)
	var logs bytes.Buffer
	service := &fakeAuthenticationService{
		result: auth.Result{
			User: auth.User{
				ID:    "8c4a5f3c-84a3-4ba9-ab0a-58683fb6b058",
				Email: "user@example.test",
			},
			Session: auth.Session{
				Token:                 accessToken,
				ExpiresAt:             time.Date(2026, 8, 8, 12, 15, 0, 0, time.UTC),
				RefreshToken:          refreshToken,
				RefreshTokenExpiresAt: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
			},
		},
	}
	router := NewRouter(Options{Logger: testLogger(&logs), AuthService: service, SecureCookies: true})

	for _, test := range []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "signup", path: "/api/v1/auth/signup", wantStatus: http.StatusCreated},
		{name: "login", path: "/api/v1/auth/login", wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPost,
				test.path,
				strings.NewReader(`{"email":"user@example.test","password":"`+password+`"}`),
			)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("authentication response is cacheable")
			}

			var body authResponse
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.User.Email != "user@example.test" || body.Session.Token != accessToken {
				t.Fatalf("unexpected authentication response: %+v", body)
			}
			if body.Session.TokenType != "Bearer" {
				t.Fatalf("token type = %q, want Bearer", body.Session.TokenType)
			}
			cookies := response.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("refresh cookies = %d, want 1", len(cookies))
			}
			cookie := cookies[0]
			if cookie.Name != refreshTokenCookieName || cookie.Value != refreshToken ||
				!cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode ||
				cookie.Path != "/api/v1/auth" {
				t.Fatalf("unsafe production refresh cookie: %+v", cookie)
			}
			if strings.Contains(response.Body.String(), refreshToken) {
				t.Fatal("authentication JSON exposed the refresh token")
			}
		})
	}

	if strings.Contains(logs.String(), password) || strings.Contains(logs.String(), accessToken) ||
		strings.Contains(logs.String(), refreshToken) {
		t.Fatal("request logs contain a password or session token")
	}
}

func TestRefreshEndpointUsesCookieAndRotatesIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		oldRefreshToken = "wt_refresh_old-test-token"
		newRefreshToken = "wt_refresh_new-test-token"
	)
	service := &fakeAuthenticationService{result: auth.Result{
		User: auth.User{ID: "user-id", Email: "user@example.test"},
		Session: auth.Session{
			Token:                 "wt_access_new-test-token",
			ExpiresAt:             time.Now().UTC().Add(15 * time.Minute),
			RefreshToken:          newRefreshToken,
			RefreshTokenExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
		},
	}}
	router := NewRouter(Options{Logger: discardLogger(), AuthService: service})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	request.AddCookie(&http.Cookie{Name: refreshTokenCookieName, Value: oldRefreshToken})
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK || service.refreshToken != oldRefreshToken {
		t.Fatalf("refresh response = %d, service token = %q", response.Code, service.refreshToken)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != newRefreshToken || !cookies[0].HttpOnly || cookies[0].Secure {
		t.Fatalf("unexpected development refresh cookie: %+v", cookies)
	}
	if strings.Contains(response.Body.String(), newRefreshToken) {
		t.Fatal("refresh JSON exposed the rotated refresh token")
	}
}

func TestRefreshEndpointRejectsMissingCookieAndClearsIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &fakeAuthenticationService{}
	router := NewRouter(Options{Logger: discardLogger(), AuthService: service, SecureCookies: true})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized || service.calls != 0 {
		t.Fatalf("missing refresh cookie response = %d, service calls = %d", response.Code, service.calls)
	}
	body := decodeErrorResponse(t, response)
	if body.Error.Code != "invalid_refresh_token" {
		t.Fatalf("refresh error code = %q", body.Error.Code)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge != -1 || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatalf("missing refresh cookie was not cleared safely: %+v", cookies)
	}
}

func TestAuthEndpointsMapErrorsWithoutLeakingDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		path       string
		serviceErr error
		wantStatus int
		wantCode   string
	}{
		{name: "duplicate signup", path: "/api/v1/auth/signup", serviceErr: auth.ErrEmailInUse, wantStatus: http.StatusConflict, wantCode: "email_in_use"},
		{name: "invalid login", path: "/api/v1/auth/login", serviceErr: auth.ErrInvalidCredentials, wantStatus: http.StatusUnauthorized, wantCode: "invalid_credentials"},
		{name: "internal login error", path: "/api/v1/auth/login", serviceErr: errors.New("database-password-must-not-escape"), wantStatus: http.StatusInternalServerError, wantCode: "internal_error"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeAuthenticationService{err: test.serviceErr}
			router := NewRouter(Options{Logger: discardLogger(), AuthService: service})
			request := httptest.NewRequest(
				http.MethodPost,
				test.path,
				strings.NewReader(`{"email":"user@example.test","password":"valid-test-password"}`),
			)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			body := decodeErrorResponse(t, response)
			if body.Error.Code != test.wantCode {
				t.Fatalf("error code = %q, want %q", body.Error.Code, test.wantCode)
			}
			if strings.Contains(response.Body.String(), "database-password-must-not-escape") {
				t.Fatal("authentication response leaked an internal error")
			}
		})
	}
}

func TestAuthEndpointValidatesCredentialsBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &fakeAuthenticationService{}
	router := NewRouter(Options{Logger: discardLogger(), AuthService: service})
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/auth/signup",
		strings.NewReader(`{"email":"not-an-email","password":"short"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnprocessableEntity)
	}
	if service.calls != 0 {
		t.Fatalf("service called %d times for invalid input", service.calls)
	}
}

type fakeAuthenticationService struct {
	result       auth.Result
	err          error
	calls        int
	refreshToken string
}

func (service *fakeAuthenticationService) Signup(context.Context, string, string) (auth.Result, error) {
	service.calls++
	return service.result, service.err
}

func (service *fakeAuthenticationService) Login(context.Context, string, string) (auth.Result, error) {
	service.calls++
	return service.result, service.err
}

func (service *fakeAuthenticationService) Refresh(_ context.Context, refreshToken string) (auth.Result, error) {
	service.calls++
	service.refreshToken = refreshToken
	return service.result, service.err
}
