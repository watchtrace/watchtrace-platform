package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
)

func TestCreateDefaultOwnershipUsesAuthenticatedUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const userID = "97867dd1-1283-4477-a9a5-289cac23151a"
	authenticator := &fakeSessionAuthenticator{user: auth.User{ID: userID}}
	service := &fakeOwnershipService{result: ownership.DefaultResult{
		Organization: ownership.Organization{ID: "org-id", Name: "Example", Slug: "example"},
		Membership:   ownership.Membership{OrganizationID: "org-id", UserID: userID, Role: "owner"},
		Project:      ownership.Project{ID: "project-id", OrganizationID: "org-id", Name: "API", Description: "Primary API"},
		Environment:  ownership.Environment{ID: "environment-id", OrganizationID: "org-id", ProjectID: "project-id", Name: "Production", EnvironmentType: "production"},
	}}
	router := NewRouter(Options{
		Logger:           discardLogger(),
		Authenticator:    authenticator,
		OwnershipService: service,
	})

	request := httptest.NewRequest(http.MethodPost, "/api/v1/organizations", strings.NewReader(`{
		"organization":{"name":" Example ","slug":"Example"},
		"project":{"name":" API ","description":" Primary API "}
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer safe-test-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("protected ownership response is cacheable")
	}
	if authenticator.token != "safe-test-token" {
		t.Fatalf("authenticated token = %q", authenticator.token)
	}
	if service.userID != userID {
		t.Fatalf("ownership user ID = %q, want %q", service.userID, userID)
	}
	if service.input.OrganizationSlug != "Example" || service.input.ProjectName != " API " {
		t.Fatalf("unexpected service input: %+v", service.input)
	}

	var body defaultOwnershipResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Membership.Role != "owner" || body.Environment.Type != "production" {
		t.Fatalf("unexpected ownership response: %+v", body)
	}
}

func TestCreateDefaultOwnershipRequiresValidSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name          string
		header        string
		authenticator *fakeSessionAuthenticator
		wantStatus    int
		wantCode      string
	}{
		{name: "missing header", authenticator: &fakeSessionAuthenticator{}, wantStatus: http.StatusUnauthorized, wantCode: "invalid_session"},
		{name: "wrong scheme", header: "Basic credentials", authenticator: &fakeSessionAuthenticator{}, wantStatus: http.StatusUnauthorized, wantCode: "invalid_session"},
		{name: "invalid token", header: "Bearer invalid", authenticator: &fakeSessionAuthenticator{err: auth.ErrInvalidSession}, wantStatus: http.StatusUnauthorized, wantCode: "invalid_session"},
		{name: "authentication failure", header: "Bearer token", authenticator: &fakeSessionAuthenticator{err: errors.New("database failure")}, wantStatus: http.StatusInternalServerError, wantCode: "internal_error"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeOwnershipService{}
			router := NewRouter(Options{
				Logger:           discardLogger(),
				Authenticator:    test.authenticator,
				OwnershipService: service,
			})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/organizations", strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "application/json")
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			body := decodeErrorResponse(t, response)
			if body.Error.Code != test.wantCode {
				t.Fatalf("error code = %q, want %q", body.Error.Code, test.wantCode)
			}
			if service.calls != 0 {
				t.Fatal("ownership service was called without a valid session")
			}
		})
	}
}

func TestCreateDefaultOwnershipRejectsCallerSelectedOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &fakeOwnershipService{}
	router := NewRouter(Options{
		Logger:           discardLogger(),
		Authenticator:    &fakeSessionAuthenticator{user: auth.User{ID: "authenticated-user"}},
		OwnershipService: service,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/organizations", strings.NewReader(`{
		"organization":{"name":"Example","slug":"example"},
		"project":{"name":"API","description":""},
		"user_id":"another-user"
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if service.calls != 0 {
		t.Fatal("ownership service accepted a caller-selected owner")
	}
}

func TestCreateDefaultOwnershipMapsServiceErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid input", err: ownership.ErrInvalidInput, wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_failed"},
		{name: "duplicate slug", err: ownership.ErrSlugInUse, wantStatus: http.StatusConflict, wantCode: "organization_slug_in_use"},
		{name: "internal", err: errors.New("secret database detail"), wantStatus: http.StatusInternalServerError, wantCode: "internal_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeOwnershipService{err: test.err}
			router := NewRouter(Options{
				Logger:           discardLogger(),
				Authenticator:    &fakeSessionAuthenticator{user: auth.User{ID: "user-id"}},
				OwnershipService: service,
			})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/organizations", strings.NewReader(`{
				"organization":{"name":"Example","slug":"example"},
				"project":{"name":"API","description":""}
			}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer token")
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			body := decodeErrorResponse(t, response)
			if body.Error.Code != test.wantCode {
				t.Fatalf("error code = %q, want %q", body.Error.Code, test.wantCode)
			}
			if strings.Contains(response.Body.String(), "secret database detail") {
				t.Fatal("response exposed internal ownership error")
			}
		})
	}
}

type fakeSessionAuthenticator struct {
	user  auth.User
	err   error
	token string
}

func (authenticator *fakeSessionAuthenticator) Authenticate(_ context.Context, token string) (auth.User, error) {
	authenticator.token = token
	return authenticator.user, authenticator.err
}

type fakeOwnershipService struct {
	result ownership.DefaultResult
	err    error
	calls  int
	userID string
	input  ownership.CreateDefaultInput
}

func (service *fakeOwnershipService) ListMembers(context.Context, string, string) ([]ownership.Member, error) {
	service.calls++
	return nil, service.err
}

func (service *fakeOwnershipService) Invite(context.Context, string, string, string, authorization.Role) (ownership.Invitation, error) {
	service.calls++
	return ownership.Invitation{}, service.err
}

func (service *fakeOwnershipService) AcceptInvitation(context.Context, auth.User, string) (ownership.Membership, error) {
	service.calls++
	return ownership.Membership{}, service.err
}

func (service *fakeOwnershipService) CreateDefault(
	_ context.Context,
	userID string,
	input ownership.CreateDefaultInput,
) (ownership.DefaultResult, error) {
	service.calls++
	service.userID = userID
	service.input = input
	return service.result, service.err
}
