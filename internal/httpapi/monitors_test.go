package httpapi

import (
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
	"github.com/watchtrace/watchtrace-platform/internal/monitor"
)

const (
	monitorTestUserID        = "97867dd1-1283-4477-a9a5-289cac23151a"
	monitorTestEnvironmentID = "3249c694-c0fc-4430-95d3-f12419307dd2"
	monitorTestMonitorID     = "845d1e0d-933c-41a6-9af3-f479c757868b"
)

func TestCreateMonitorUsesAuthenticatedTenantAndReturnsConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	createdAt := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	service := &fakeMonitorService{created: monitor.Monitor{
		ID:                "monitor-id",
		OrganizationID:    "organization-id",
		EnvironmentID:     monitorTestEnvironmentID,
		Name:              "API",
		TargetURL:         "https://example.test/health",
		Method:            "GET",
		IntervalSeconds:   300,
		TimeoutSeconds:    5,
		ExpectedStatusMin: 200,
		ExpectedStatusMax: 299,
		CreatedAt:         createdAt,
		UpdatedAt:         createdAt,
	}}
	router := NewRouter(Options{
		Logger:         discardLogger(),
		Authenticator:  &fakeSessionAuthenticator{user: auth.User{ID: monitorTestUserID}},
		MonitorService: service,
	})
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/environments/"+monitorTestEnvironmentID+"/monitors",
		strings.NewReader(`{"name":"API","url":"https://example.test/health"}`),
	)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if service.userID != monitorTestUserID || service.environmentID != monitorTestEnvironmentID {
		t.Fatalf("service tenant input = user %q environment %q", service.userID, service.environmentID)
	}
	if service.input.IntervalSeconds != 0 || service.input.TimeoutSeconds != 0 {
		t.Fatalf("handler replaced service-owned defaults: %+v", service.input)
	}
	var body monitorResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode monitor response: %v", err)
	}
	if body.Method != "GET" || body.IntervalSeconds != 300 || body.TimeoutSeconds != 5 {
		t.Fatalf("unexpected monitor response: %+v", body)
	}
}

func TestListMonitorsReturnsNonNullArray(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &fakeMonitorService{listed: []monitor.Monitor{}}
	router := NewRouter(Options{
		Logger:         discardLogger(),
		Authenticator:  &fakeSessionAuthenticator{user: auth.User{ID: monitorTestUserID}},
		MonitorService: service,
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/environments/"+monitorTestEnvironmentID+"/monitors", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if body := response.Body.String(); body != `{"monitors":[]}` {
		t.Fatalf("body = %s, want non-null empty monitor array", body)
	}
}

func TestGetMonitorReturnsStateAndRecentChecks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checkedAt := time.Date(2026, 8, 8, 12, 1, 0, 0, time.UTC)
	statusCode := int16(503)
	errorCategory := "unexpected_status"
	service := &fakeMonitorService{detail: monitor.Detail{
		Monitor: monitor.Monitor{
			ID:                monitorTestMonitorID,
			OrganizationID:    "organization-id",
			EnvironmentID:     monitorTestEnvironmentID,
			Name:              "API",
			TargetURL:         "https://example.test/health",
			Method:            "GET",
			IntervalSeconds:   300,
			TimeoutSeconds:    5,
			ExpectedStatusMin: 200,
			ExpectedStatusMax: 299,
			CreatedAt:         checkedAt.Add(-time.Hour),
			UpdatedAt:         checkedAt.Add(-time.Hour),
		},
		State: monitor.StateDegraded,
		RecentResults: []monitor.CheckResult{{
			JobID:                     "job-id",
			JobType:                   "manual_test",
			ScheduledAt:               checkedAt.Add(-time.Second),
			StartedAt:                 checkedAt.Add(-500 * time.Millisecond),
			CompletedAt:               checkedAt,
			Succeeded:                 false,
			StatusCode:                &statusCode,
			ErrorCategory:             &errorCategory,
			TotalDurationMicroseconds: 500000,
		}},
	}}
	router := NewRouter(Options{
		Logger:         discardLogger(),
		Authenticator:  &fakeSessionAuthenticator{user: auth.User{ID: monitorTestUserID}},
		MonitorService: service,
	})
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/environments/"+monitorTestEnvironmentID+"/monitors/"+monitorTestMonitorID,
		nil,
	)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if service.userID != monitorTestUserID || service.environmentID != monitorTestEnvironmentID ||
		service.monitorID != monitorTestMonitorID {
		t.Fatalf("service scope = user %q environment %q monitor %q", service.userID, service.environmentID, service.monitorID)
	}
	if !strings.Contains(response.Body.String(), `"job_type":"manual"`) || strings.Contains(response.Body.String(), "manual_test") {
		t.Fatalf("recent check must use the public manual job type: %s", response.Body.String())
	}
	var body monitorDetailResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode monitor detail: %v", err)
	}
	if body.ID != monitorTestMonitorID || body.State != monitor.StateDegraded || len(body.RecentChecks) != 1 {
		t.Fatalf("unexpected monitor detail: %+v", body)
	}
	check := body.RecentChecks[0]
	if check.Succeeded || check.StatusCode == nil || *check.StatusCode != 503 ||
		check.ErrorCategory == nil || *check.ErrorCategory != errorCategory {
		t.Fatalf("unexpected recent check: %+v", check)
	}
}

func TestMonitorRoutesRequireSessionAndMapSafeErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("missing session", func(t *testing.T) {
		service := &fakeMonitorService{}
		router := NewRouter(Options{
			Logger:         discardLogger(),
			Authenticator:  &fakeSessionAuthenticator{},
			MonitorService: service,
		})
		request := httptest.NewRequest(http.MethodGet, "/api/v1/environments/"+monitorTestEnvironmentID+"/monitors", nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || service.calls != 0 {
			t.Fatalf("missing-session response = %d, service calls = %d", response.Code, service.calls)
		}
	})

	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid input", err: monitor.ErrInvalidInput, wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_failed"},
		{name: "foreign environment", err: monitor.ErrEnvironmentNotFound, wantStatus: http.StatusNotFound, wantCode: "environment_not_found"},
		{name: "foreign monitor", err: monitor.ErrMonitorNotFound, wantStatus: http.StatusNotFound, wantCode: "monitor_not_found"},
		{name: "limit", err: monitor.ErrMonitorLimitReached, wantStatus: http.StatusConflict, wantCode: "monitor_limit_reached"},
		{name: "internal", err: errors.New("secret database detail"), wantStatus: http.StatusInternalServerError, wantCode: "internal_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeMonitorService{err: test.err}
			router := NewRouter(Options{
				Logger:         discardLogger(),
				Authenticator:  &fakeSessionAuthenticator{user: auth.User{ID: monitorTestUserID}},
				MonitorService: service,
			})
			request := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/environments/"+monitorTestEnvironmentID+"/monitors",
				strings.NewReader(`{"name":"API","url":"https://example.test/health"}`),
			)
			request.Header.Set("Authorization", "Bearer token")
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
			if strings.Contains(response.Body.String(), "secret database detail") {
				t.Fatal("monitor response exposed an internal error")
			}
		})
	}
}

type fakeMonitorService struct {
	created       monitor.Monitor
	listed        []monitor.Monitor
	detail        monitor.Detail
	err           error
	calls         int
	userID        string
	environmentID string
	monitorID     string
	input         monitor.CreateInput
}

func (service *fakeMonitorService) Create(
	_ context.Context,
	userID string,
	environmentID string,
	input monitor.CreateInput,
) (monitor.Monitor, error) {
	service.calls++
	service.userID = userID
	service.environmentID = environmentID
	service.input = input
	return service.created, service.err
}

func (service *fakeMonitorService) List(
	_ context.Context,
	userID string,
	environmentID string,
) ([]monitor.Monitor, error) {
	service.calls++
	service.userID = userID
	service.environmentID = environmentID
	return service.listed, service.err
}

func (service *fakeMonitorService) Get(
	_ context.Context,
	userID string,
	environmentID string,
	monitorID string,
) (monitor.Detail, error) {
	service.calls++
	service.userID = userID
	service.environmentID = environmentID
	service.monitorID = monitorID
	return service.detail, service.err
}
