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

	authService := auth.NewService(pool, &recordingVerificationSender{})
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

	unknownDetail := performMonitorGet(
		t,
		router,
		firstSignup.Body.Session.Token,
		firstOwnership.Body.Environment.ID,
		createdDefault.Body.ID,
	)
	if unknownDetail.Status != http.StatusOK || unknownDetail.Body.State != monitor.StateUnknown ||
		len(unknownDetail.Body.RecentChecks) != 0 {
		t.Fatalf("new monitor detail = status %d state %q checks %d: %s", unknownDetail.Status,
			unknownDetail.Body.State, len(unknownDetail.Body.RecentChecks), unknownDetail.RawBody)
	}
	if !strings.Contains(unknownDetail.RawBody, `"recent_checks":[]`) {
		t.Fatalf("new monitor recent checks must be a non-null array: %s", unknownDetail.RawBody)
	}

	baseCheckTime := time.Date(2026, 8, 8, 13, 0, 0, 0, time.UTC)
	failedJobID := insertMonitorAPIResult(
		t, ctx, pool, firstOwnership.Body.Organization.ID, firstOwnership.Body.Environment.ID,
		createdDefault.Body.ID, "scheduled", baseCheckTime, false, int16(503), "unexpected_status",
	)
	failedDetail := performMonitorGet(
		t, router, firstSignup.Body.Session.Token, firstOwnership.Body.Environment.ID, createdDefault.Body.ID,
	)
	if failedDetail.Status != http.StatusOK || failedDetail.Body.State != monitor.StateDegraded ||
		len(failedDetail.Body.RecentChecks) != 1 || failedDetail.Body.RecentChecks[0].JobID != failedJobID ||
		failedDetail.Body.RecentChecks[0].Succeeded {
		t.Fatalf("failed monitor detail is incorrect: %+v", failedDetail.Body)
	}

	manualJobIDs := make([]string, 0, 21)
	for index := 1; index <= 21; index++ {
		manualJobIDs = append(manualJobIDs, insertMonitorAPIResult(
			t, ctx, pool, firstOwnership.Body.Organization.ID, firstOwnership.Body.Environment.ID,
			createdDefault.Body.ID, "manual_test", baseCheckTime.Add(time.Duration(index)*time.Minute),
			true, int16(204), nil,
		))
	}
	boundedDetail := performMonitorGet(
		t, router, firstSignup.Body.Session.Token, firstOwnership.Body.Environment.ID, createdDefault.Body.ID,
	)
	if boundedDetail.Status != http.StatusOK || boundedDetail.Body.State != monitor.StateDegraded ||
		len(boundedDetail.Body.RecentChecks) != 20 {
		t.Fatalf("bounded monitor detail = status %d state %q checks %d", boundedDetail.Status,
			boundedDetail.Body.State, len(boundedDetail.Body.RecentChecks))
	}
	if boundedDetail.Body.RecentChecks[0].JobID != manualJobIDs[20] ||
		boundedDetail.Body.RecentChecks[19].JobID != manualJobIDs[1] {
		t.Fatal("recent monitor results are not limited in stable newest-first order")
	}

	successJobID := insertMonitorAPIResult(
		t, ctx, pool, firstOwnership.Body.Organization.ID, firstOwnership.Body.Environment.ID,
		createdDefault.Body.ID, "scheduled", baseCheckTime.Add(22*time.Minute), true, int16(200), nil,
	)
	healthyDetail := performMonitorGet(
		t, router, firstSignup.Body.Session.Token, firstOwnership.Body.Environment.ID, createdDefault.Body.ID,
	)
	if healthyDetail.Status != http.StatusOK || healthyDetail.Body.State != monitor.StateHealthy ||
		len(healthyDetail.Body.RecentChecks) != 20 || healthyDetail.Body.RecentChecks[0].JobID != successJobID {
		t.Fatalf("healthy monitor detail is incorrect: %+v", healthyDetail.Body)
	}
	for _, result := range healthyDetail.Body.RecentChecks {
		if result.JobID == failedJobID {
			t.Fatal("bounded recent results included a row older than the newest 20")
		}
	}

	foreignMonitor := performMonitorCreate(
		t, router, secondSignup.Body.Session.Token, secondOwnership.Body.Environment.ID,
		monitorCreateBody{Name: "Second tenant monitor", URL: "https://example.test/second"},
	)
	if foreignMonitor.Status != http.StatusCreated {
		t.Fatalf("create second tenant monitor = %d: %s", foreignMonitor.Status, foreignMonitor.RawBody)
	}

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
	crossTenantRead := performMonitorGet(
		t, router, secondSignup.Body.Session.Token, firstOwnership.Body.Environment.ID, createdDefault.Body.ID,
	)
	if crossTenantRead.Status != http.StatusNotFound || crossTenantRead.ErrorCode != "environment_not_found" {
		t.Fatalf("cross-tenant read = %d %q", crossTenantRead.Status, crossTenantRead.ErrorCode)
	}
	foreignMonitorRead := performMonitorGet(
		t, router, firstSignup.Body.Session.Token, firstOwnership.Body.Environment.ID, foreignMonitor.Body.ID,
	)
	if foreignMonitorRead.Status != http.StatusNotFound || foreignMonitorRead.ErrorCode != "monitor_not_found" {
		t.Fatalf("foreign monitor read = %d %q", foreignMonitorRead.Status, foreignMonitorRead.ErrorCode)
	}
	malformedMonitorRead := performMonitorGet(
		t, router, firstSignup.Body.Session.Token, firstOwnership.Body.Environment.ID, "not-a-monitor-id",
	)
	if malformedMonitorRead.Status != http.StatusNotFound || malformedMonitorRead.ErrorCode != "monitor_not_found" {
		t.Fatalf("malformed monitor read = %d %q", malformedMonitorRead.Status, malformedMonitorRead.ErrorCode)
	}

	var stagingEnvironmentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO environments (organization_id, project_id, name, environment_type)
		VALUES ($1::text::uuid, $2::text::uuid, 'Staging', 'staging')
		RETURNING id::text
	`, firstOwnership.Body.Organization.ID, firstOwnership.Body.Project.ID).Scan(&stagingEnvironmentID); err != nil {
		t.Fatalf("create same-organization environment: %v", err)
	}
	crossEnvironmentRead := performMonitorGet(
		t, router, firstSignup.Body.Session.Token, stagingEnvironmentID, createdDefault.Body.ID,
	)
	if crossEnvironmentRead.Status != http.StatusNotFound || crossEnvironmentRead.ErrorCode != "monitor_not_found" {
		t.Fatalf("cross-environment read = %d %q", crossEnvironmentRead.Status, crossEnvironmentRead.ErrorCode)
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
	viewerDetail := performMonitorGet(
		t, router, secondSignup.Body.Session.Token, firstOwnership.Body.Environment.ID, createdDefault.Body.ID,
	)
	if viewerDetail.Status != http.StatusOK || viewerDetail.Body.State != monitor.StateHealthy {
		t.Fatalf("viewer detail = status %d state %q", viewerDetail.Status, viewerDetail.Body.State)
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

	invalidMonitors := []monitorCreateBody{
		{Name: "Invalid interval", URL: "https://example.test/invalid", IntervalSeconds: 61},
		{Name: "Invalid scheme", URL: "ftp://example.test/health"},
		{Name: "Invalid port", URL: "https://example.test:8443/health"},
		{Name: "IPv4 loopback", URL: "http://127.0.0.1/health"},
		{Name: "IPv4 metadata", URL: "http://169.254.169.254/latest/meta-data"},
		{Name: "IPv6 private", URL: "https://[fd00::1]/health"},
	}
	for _, input := range invalidMonitors {
		invalid := performMonitorCreate(
			t,
			router,
			firstSignup.Body.Session.Token,
			firstOwnership.Body.Environment.ID,
			input,
		)
		if invalid.Status != http.StatusUnprocessableEntity || invalid.ErrorCode != "validation_failed" {
			t.Fatalf("invalid monitor %q = %d %q", input.Name, invalid.Status, invalid.ErrorCode)
		}
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

type monitorCheckBody struct {
	JobID                     string    `json:"job_id"`
	JobType                   string    `json:"job_type"`
	ScheduledAt               time.Time `json:"scheduled_at"`
	StartedAt                 time.Time `json:"started_at"`
	CompletedAt               time.Time `json:"completed_at"`
	Succeeded                 bool      `json:"succeeded"`
	StatusCode                *int16    `json:"status_code"`
	ErrorCategory             *string   `json:"error_category"`
	TotalDurationMicroseconds int64     `json:"total_duration_microseconds"`
}

type monitorDetailBody struct {
	monitorBody
	State        monitor.State      `json:"state"`
	RecentChecks []monitorCheckBody `json:"recent_checks"`
}

type monitorAPIResult struct {
	Status    int
	RawBody   string
	ErrorCode string
	Body      monitorBody
	Monitors  []monitorBody
}

type monitorGetAPIResult struct {
	Status    int
	RawBody   string
	ErrorCode string
	Body      monitorDetailBody
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

func performMonitorGet(
	t *testing.T,
	handler http.Handler,
	token string,
	environmentID string,
	monitorID string,
) monitorGetAPIResult {
	t.Helper()
	request := httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/v1/environments/%s/monitors/%s", environmentID, monitorID),
		nil,
	)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	result := monitorGetAPIResult{Status: response.Code, RawBody: response.Body.String()}
	if response.Code >= http.StatusBadRequest {
		var errorBody httpapi.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &errorBody); err != nil {
			t.Fatalf("decode monitor detail error: %v", err)
		}
		result.ErrorCode = errorBody.Error.Code
		return result
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result.Body); err != nil {
		t.Fatalf("decode monitor detail: %v", err)
	}
	return result
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

func insertMonitorAPIResult(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	organizationID string,
	environmentID string,
	monitorID string,
	jobType string,
	scheduledAt time.Time,
	succeeded bool,
	statusCode any,
	errorCategory any,
) string {
	t.Helper()
	var jobID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO health_checks (
			job_id,
			organization_id,
			environment_id,
			monitor_id,
			job_type,
			scheduled_at,
			started_at,
			completed_at,
			succeeded,
			status_code,
			error_category,
			total_duration_microseconds
		)
		VALUES (
			gen_random_uuid(),
			$1::text::uuid,
			$2::text::uuid,
			$3::text::uuid,
			$4,
			$5,
			$5::timestamptz + INTERVAL '100 milliseconds',
			$5::timestamptz + INTERVAL '600 milliseconds',
			$6,
			$7::smallint,
			$8::text,
			500000
		)
		RETURNING job_id::text
	`, organizationID, environmentID, monitorID, jobType, scheduledAt, succeeded, statusCode, errorCategory).Scan(&jobID); err != nil {
		t.Fatalf("insert monitor API result: %v", err)
	}
	return jobID
}
