package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/httpapi"
	"github.com/watchtrace/watchtrace-platform/internal/monitor"
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
)

func TestMonitorAPIWithPostgreSQL(t *testing.T) {
	databaseURL := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create PostgreSQL connection pool: %v", err)
	}
	t.Cleanup(pool.Close)

	emails := []string{"monitor-first@example.test", "monitor-second@example.test"}
	slugs := []string{"monitor-first", "monitor-second"}
	deleteOwnershipTestData(t, ctx, pool, emails, slugs)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		deleteOwnershipTestData(t, cleanupCtx, pool, emails, slugs)
	})

	authService := auth.NewService(pool)
	ownershipService := ownership.NewService(pool)
	monitorService := monitor.NewService(pool)
	var logs bytes.Buffer
	router := httpapi.NewRouter(httpapi.Options{
		Logger:           slog.New(slog.NewJSONHandler(&logs, nil)),
		AuthService:      authService,
		Authenticator:    authService,
		OwnershipService: ownershipService,
		MonitorService:   monitorService,
	})

	firstSignup := performAuthRequest(t, router, "/api/v1/auth/signup", emails[0], "P1-104-first-password!")
	secondSignup := performAuthRequest(t, router, "/api/v1/auth/signup", emails[1], "P1-104-second-password!")
	if firstSignup.Status != http.StatusCreated || secondSignup.Status != http.StatusCreated {
		t.Fatalf("monitor test signup failed: first=%d second=%d", firstSignup.Status, secondSignup.Status)
	}
	firstOwnership := performOwnershipRequest(t, router, firstSignup.Body.Session.Token, ownershipRequestBody{
		OrganizationName: "Monitor First",
		OrganizationSlug: slugs[0],
		ProjectName:      "First API",
	})
	secondOwnership := performOwnershipRequest(t, router, secondSignup.Body.Session.Token, ownershipRequestBody{
		OrganizationName: "Monitor Second",
		OrganizationSlug: slugs[1],
		ProjectName:      "Second API",
	})
	if firstOwnership.Status != http.StatusCreated || secondOwnership.Status != http.StatusCreated {
		t.Fatalf("monitor test ownership failed: first=%d second=%d", firstOwnership.Status, secondOwnership.Status)
	}

	const firstTarget = "https://example.test/health"
	createdDefault := performMonitorCreate(t, router, firstSignup.Body.Session.Token, firstOwnership.Body.Environment.ID, monitorCreateBody{
		Name: "Default monitor",
		URL:  firstTarget,
	})
	if createdDefault.Status != http.StatusCreated {
		t.Fatalf("default monitor status = %d: %s", createdDefault.Status, createdDefault.RawBody)
	}
	assertMonitorDefaults(t, createdDefault.Body, firstOwnership)

	createdCustom := performMonitorCreate(t, router, firstSignup.Body.Session.Token, firstOwnership.Body.Environment.ID, monitorCreateBody{
		Name:              "Custom monitor",
		URL:               "https://example.test/ready",
		IntervalSeconds:   120,
		TimeoutSeconds:    7,
		ExpectedStatusMin: 201,
		ExpectedStatusMax: 399,
	})
	if createdCustom.Status != http.StatusCreated {
		t.Fatalf("custom monitor status = %d: %s", createdCustom.Status, createdCustom.RawBody)
	}
	if createdCustom.Body.IntervalSeconds != 120 || createdCustom.Body.TimeoutSeconds != 7 ||
		createdCustom.Body.ExpectedStatusMin != 201 || createdCustom.Body.ExpectedStatusMax != 399 {
		t.Fatalf("custom settings were not stored: %+v", createdCustom.Body)
	}

	listed := performMonitorList(t, router, firstSignup.Body.Session.Token, firstOwnership.Body.Environment.ID)
	if listed.Status != http.StatusOK || len(listed.Monitors) != 2 {
		t.Fatalf("monitor list = status %d count %d: %s", listed.Status, len(listed.Monitors), listed.RawBody)
	}
	if listed.Monitors[0].ID != createdDefault.Body.ID || listed.Monitors[1].ID != createdCustom.Body.ID {
		t.Fatal("monitor list does not use stable creation order")
	}

	assertMonitorDatabaseDefaults(t, ctx, pool, createdDefault.Body.ID)

	crossTenantList := performMonitorList(t, router, secondSignup.Body.Session.Token, firstOwnership.Body.Environment.ID)
	if crossTenantList.Status != http.StatusNotFound || crossTenantList.ErrorCode != "environment_not_found" {
		t.Fatalf("cross-tenant list = %d %q", crossTenantList.Status, crossTenantList.ErrorCode)
	}
	crossTenantCreate := performMonitorCreate(t, router, firstSignup.Body.Session.Token, secondOwnership.Body.Environment.ID, monitorCreateBody{
		Name: "Foreign monitor", URL: "https://example.test/foreign",
	})
	if crossTenantCreate.Status != http.StatusNotFound || crossTenantCreate.ErrorCode != "environment_not_found" {
		t.Fatalf("cross-tenant create = %d %q", crossTenantCreate.Status, crossTenantCreate.ErrorCode)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO org_members (organization_id, user_id, role)
		VALUES ($1::text::uuid, $2::text::uuid, 'viewer')
	`, firstOwnership.Body.Organization.ID, secondSignup.Body.User.ID)
	if err != nil {
		t.Fatalf("add viewer membership: %v", err)
	}
	viewerList := performMonitorList(t, router, secondSignup.Body.Session.Token, firstOwnership.Body.Environment.ID)
	if viewerList.Status != http.StatusOK || len(viewerList.Monitors) != 2 {
		t.Fatalf("viewer list = status %d count %d", viewerList.Status, len(viewerList.Monitors))
	}
	viewerCreate := performMonitorCreate(t, router, secondSignup.Body.Session.Token, firstOwnership.Body.Environment.ID, monitorCreateBody{
		Name: "Viewer monitor", URL: "https://example.test/viewer",
	})
	if viewerCreate.Status != http.StatusNotFound || viewerCreate.ErrorCode != "environment_not_found" {
		t.Fatalf("viewer create = %d %q", viewerCreate.Status, viewerCreate.ErrorCode)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO monitors (organization_id, environment_id, name, target_url)
		VALUES ($1::text::uuid, $2::text::uuid, 'Cross tenant', 'https://example.test/cross')
	`, firstOwnership.Body.Organization.ID, secondOwnership.Body.Environment.ID)
	assertPostgreSQLErrorCode(t, err, "23503")
	_, err = pool.Exec(ctx, `
		INSERT INTO monitors (organization_id, environment_id, name, target_url, method)
		VALUES ($1::text::uuid, $2::text::uuid, 'Unsupported method', 'https://example.test/head', 'HEAD')
	`, firstOwnership.Body.Organization.ID, firstOwnership.Body.Environment.ID)
	assertPostgreSQLErrorCode(t, err, "23514")

	invalid := performMonitorCreate(t, router, firstSignup.Body.Session.Token, firstOwnership.Body.Environment.ID, monitorCreateBody{
		Name: "Invalid", URL: "https://example.test/invalid", IntervalSeconds: 61,
	})
	if invalid.Status != http.StatusUnprocessableEntity || invalid.ErrorCode != "validation_failed" {
		t.Fatalf("invalid monitor = %d %q", invalid.Status, invalid.ErrorCode)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO monitors (organization_id, environment_id, name, target_url)
		SELECT
			$1::text::uuid,
			$2::text::uuid,
			'Limit monitor ' || value,
			'https://example.test/limit/' || value
		FROM generate_series(1, 98) AS value
	`, firstOwnership.Body.Organization.ID, firstOwnership.Body.Environment.ID)
	if err != nil {
		t.Fatalf("fill organization monitor limit: %v", err)
	}
	limited := performMonitorCreate(t, router, firstSignup.Body.Session.Token, firstOwnership.Body.Environment.ID, monitorCreateBody{
		Name: "Over limit", URL: "https://example.test/over-limit",
	})
	if limited.Status != http.StatusConflict || limited.ErrorCode != "monitor_limit_reached" {
		t.Fatalf("monitor limit response = %d %q", limited.Status, limited.ErrorCode)
	}

	for _, secret := range []string{firstSignup.Body.Session.Token, secondSignup.Body.Session.Token, firstTarget} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("monitor logs contain a bearer token or target URL")
		}
	}
}

func TestMonitorSchemaRollback(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_MONITOR_SCHEMA_ABSENT") != "1" {
		t.Skip("WATCHTRACE_EXPECT_MONITOR_SCHEMA_ABSENT is not set")
	}

	ctx, tx := beginOwnershipSchemaTest(t)
	var relationName *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.monitors')::text`).Scan(&relationName); err != nil {
		t.Fatalf("inspect rolled-back monitors table: %v", err)
	}
	if relationName != nil {
		t.Fatal("monitors still exists after migration rollback")
	}
	for _, tableName := range []string{"users", "organizations", "org_members", "projects", "environments", "auth_sessions"} {
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1)::text`, "public."+tableName).Scan(&relationName); err != nil {
			t.Fatalf("inspect preserved table %s: %v", tableName, err)
		}
		if relationName == nil {
			t.Errorf("preceding migration table %s is absent", tableName)
		}
	}
}

type monitorCreateBody struct {
	Name              string `json:"name"`
	URL               string `json:"url"`
	IntervalSeconds   int32  `json:"interval_seconds,omitempty"`
	TimeoutSeconds    int32  `json:"timeout_seconds,omitempty"`
	ExpectedStatusMin int16  `json:"expected_status_min,omitempty"`
	ExpectedStatusMax int16  `json:"expected_status_max,omitempty"`
}

type monitorBody struct {
	ID                string    `json:"id"`
	OrganizationID    string    `json:"organization_id"`
	EnvironmentID     string    `json:"environment_id"`
	Name              string    `json:"name"`
	URL               string    `json:"url"`
	Method            string    `json:"method"`
	IntervalSeconds   int32     `json:"interval_seconds"`
	TimeoutSeconds    int32     `json:"timeout_seconds"`
	ExpectedStatusMin int16     `json:"expected_status_min"`
	ExpectedStatusMax int16     `json:"expected_status_max"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type monitorAPIResult struct {
	Status    int
	RawBody   string
	ErrorCode string
	Body      monitorBody
	Monitors  []monitorBody
}

func performMonitorCreate(
	t *testing.T,
	handler http.Handler,
	token string,
	environmentID string,
	input monitorCreateBody,
) monitorAPIResult {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("encode monitor request: %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		fmt.Sprintf("/api/v1/environments/%s/monitors", environmentID),
		bytes.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	return executeMonitorRequest(t, handler, request)
}

func performMonitorList(t *testing.T, handler http.Handler, token, environmentID string) monitorAPIResult {
	t.Helper()
	request := httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/v1/environments/%s/monitors", environmentID),
		nil,
	)
	request.Header.Set("Authorization", "Bearer "+token)
	return executeMonitorRequest(t, handler, request)
}

func executeMonitorRequest(t *testing.T, handler http.Handler, request *http.Request) monitorAPIResult {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	result := monitorAPIResult{Status: response.Code, RawBody: response.Body.String()}
	if response.Code >= http.StatusBadRequest {
		var errorBody httpapi.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &errorBody); err != nil {
			t.Fatalf("decode monitor error: %v", err)
		}
		result.ErrorCode = errorBody.Error.Code
		return result
	}
	if request.Method == http.MethodGet {
		var listBody struct {
			Monitors []monitorBody `json:"monitors"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &listBody); err != nil {
			t.Fatalf("decode monitor list: %v", err)
		}
		result.Monitors = listBody.Monitors
		return result
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result.Body); err != nil {
		t.Fatalf("decode monitor response: %v", err)
	}
	return result
}

func assertMonitorDefaults(t *testing.T, body monitorBody, ownershipResult ownershipAPIResult) {
	t.Helper()
	if body.ID == "" || body.OrganizationID != ownershipResult.Body.Organization.ID ||
		body.EnvironmentID != ownershipResult.Body.Environment.ID {
		t.Fatalf("monitor tenant identifiers are incorrect: %+v", body)
	}
	if body.Method != "GET" || body.IntervalSeconds != 300 || body.TimeoutSeconds != 5 ||
		body.ExpectedStatusMin != 200 || body.ExpectedStatusMax != 299 {
		t.Fatalf("monitor defaults are incorrect: %+v", body)
	}
	if body.CreatedAt.IsZero() || body.UpdatedAt.IsZero() {
		t.Fatalf("monitor timestamps are missing: %+v", body)
	}
}

func assertMonitorDatabaseDefaults(t *testing.T, ctx context.Context, pool *pgxpool.Pool, monitorID string) {
	t.Helper()
	var method string
	var interval int
	var timeout int
	var statusMin int
	var statusMax int
	if err := pool.QueryRow(ctx, `
		SELECT method, interval_seconds, timeout_seconds, expected_status_min, expected_status_max
		FROM monitors
		WHERE id = $1::text::uuid
	`, monitorID).Scan(&method, &interval, &timeout, &statusMin, &statusMax); err != nil {
		t.Fatalf("inspect monitor database defaults: %v", err)
	}
	if method != "GET" || interval != 300 || timeout != 5 || statusMin != 200 || statusMax != 299 {
		t.Fatalf("database defaults = %s %d %d %d-%d", method, interval, timeout, statusMin, statusMax)
	}
}
