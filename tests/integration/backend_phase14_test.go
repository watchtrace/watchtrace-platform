package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/backendapi"
	"github.com/watchtrace/watchtrace-platform/internal/httpapi"
	"github.com/watchtrace/watchtrace-platform/internal/monitor"
	"github.com/watchtrace/watchtrace-platform/internal/operations"
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
	"github.com/watchtrace/watchtrace-platform/internal/realtime"
)

func TestPhase14TenantManagementAPIAuthorization(t *testing.T) {
	ctx, pool := openPhase14Pool(t)
	emails := []string{"p14-owner@example.test", "p14-viewer@example.test", "p14-outsider@example.test"}
	slugs := []string{"p14-tenant", "p14-foreign"}
	deleteOwnershipTestData(t, ctx, pool, emails, slugs)
	t.Cleanup(func() { deleteOwnershipTestData(t, context.Background(), pool, emails, slugs) })

	authService := auth.NewService(pool, &recordingVerificationSender{})
	ownershipService := ownership.NewService(pool)
	router := httpapi.NewRouter(httpapi.Options{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), AuthService: authService, Authenticator: authService, OwnershipService: ownershipService})
	owner := performAuthRequest(t, router, "/api/v1/auth/signup", emails[0], "Phase-14-owner-password!")
	viewer := performAuthRequest(t, router, "/api/v1/auth/signup", emails[1], "Phase-14-viewer-password!")
	outsider := performAuthRequest(t, router, "/api/v1/auth/signup", emails[2], "Phase-14-outsider-password!")
	root := performOwnershipRequest(t, router, owner.Body.Session.Token, ownershipRequestBody{OrganizationName: "Phase 14", OrganizationSlug: slugs[0], ProjectName: "Root"})
	foreign := performOwnershipRequest(t, router, outsider.Body.Session.Token, ownershipRequestBody{OrganizationName: "Foreign", OrganizationSlug: slugs[1], ProjectName: "Root"})
	if root.Status != http.StatusCreated || foreign.Status != http.StatusCreated {
		t.Fatalf("tenant setup=%d/%d", root.Status, foreign.Status)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO org_members(organization_id,user_id,role) VALUES($1::uuid,$2::uuid,'viewer')`, root.Body.Organization.ID, viewer.Body.User.ID); err != nil {
		t.Fatal(err)
	}

	organizations := phase14Request(t, router, http.MethodGet, "/api/v1/organizations", owner.Body.Session.Token, nil)
	if organizations.Code != http.StatusOK || !strings.Contains(organizations.Body.String(), `"tenant:manage"`) {
		t.Fatalf("organizations=%d %s", organizations.Code, organizations.Body.String())
	}
	created := phase14Request(t, router, http.MethodPost, "/api/v1/organizations/"+root.Body.Organization.ID+"/projects", owner.Body.Session.Token, map[string]any{"name": "Worker", "description": "Managed through API"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create project=%d %s", created.Code, created.Body.String())
	}
	var project struct {
		ID string `json:"id"`
	}
	decodePhase14(t, created, &project)
	for _, request := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/projects/" + project.ID, nil},
		{http.MethodPut, "/api/v1/projects/" + project.ID, map[string]any{"name": "Worker API", "description": "Updated"}},
	} {
		response := phase14Request(t, router, request.method, request.path, owner.Body.Session.Token, request.body)
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s=%d %s", request.method, request.path, response.Code, response.Body.String())
		}
	}
	environment := phase14Request(t, router, http.MethodPost, "/api/v1/projects/"+project.ID+"/environments", owner.Body.Session.Token, map[string]any{"name": "Staging", "type": "staging"})
	if environment.Code != http.StatusCreated {
		t.Fatalf("create environment=%d %s", environment.Code, environment.Body.String())
	}
	var environmentBody struct {
		ID string `json:"id"`
	}
	decodePhase14(t, environment, &environmentBody)
	if got := phase14Request(t, router, http.MethodGet, "/api/v1/environments/"+environmentBody.ID, viewer.Body.Session.Token, nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"role":"viewer"`) {
		t.Fatalf("viewer environment=%d %s", got.Code, got.Body.String())
	}
	if denied := phase14Request(t, router, http.MethodPost, "/api/v1/organizations/"+root.Body.Organization.ID+"/projects", viewer.Body.Session.Token, map[string]any{"name": "Denied"}); denied.Code != http.StatusForbidden {
		t.Fatalf("viewer create=%d %s", denied.Code, denied.Body.String())
	}
	if hidden := phase14Request(t, router, http.MethodGet, "/api/v1/projects/"+project.ID, outsider.Body.Session.Token, nil); hidden.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant read=%d %s", hidden.Code, hidden.Body.String())
	}
	if preference := phase14Request(t, router, http.MethodPatch, "/api/v1/organizations/"+root.Body.Organization.ID+"/members/"+viewer.Body.User.ID, viewer.Body.Session.Token, map[string]any{"incident_notifications_enabled": false}); preference.Code != http.StatusOK {
		t.Fatalf("self preference=%d %s", preference.Code, preference.Body.String())
	}
	if removed := phase14Request(t, router, http.MethodDelete, "/api/v1/environments/"+environmentBody.ID, owner.Body.Session.Token, nil); removed.Code != http.StatusNoContent {
		t.Fatalf("delete environment=%d %s", removed.Code, removed.Body.String())
	}
	if removed := phase14Request(t, router, http.MethodDelete, "/api/v1/projects/"+project.ID, owner.Body.Session.Token, nil); removed.Code != http.StatusNoContent {
		t.Fatalf("delete project=%d %s", removed.Code, removed.Body.String())
	}
}

func TestPhase14MonitoringReportingIncidentsEventsAndPolling(t *testing.T) {
	ctx, pool := openPhase14Pool(t)
	emails := []string{"p14-system-owner@example.test", "p14-system-cross@example.test"}
	slugs := []string{"p14-system", "p14-system-cross"}
	deleteOwnershipTestData(t, ctx, pool, emails, slugs)
	t.Cleanup(func() { deleteOwnershipTestData(t, context.Background(), pool, emails, slugs) })

	authService := auth.NewService(pool, &recordingVerificationSender{})
	ownershipService := ownership.NewService(pool)
	monitorService := monitor.NewService(pool)
	realtimeService := realtime.New(pool)
	router := httpapi.NewRouter(httpapi.Options{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), AuthService: authService, Authenticator: authService, OwnershipService: ownershipService, MonitorService: monitorService, BackendService: backendapi.New(pool), RealtimeService: realtimeService, OperationsService: operations.New(pool)})
	owner := performAuthRequest(t, router, "/api/v1/auth/signup", emails[0], "Phase-14-system-password!")
	cross := performAuthRequest(t, router, "/api/v1/auth/signup", emails[1], "Phase-14-cross-password!")
	root := performOwnershipRequest(t, router, owner.Body.Session.Token, ownershipRequestBody{OrganizationName: "System", OrganizationSlug: slugs[0], ProjectName: "API"})
	foreign := performOwnershipRequest(t, router, cross.Body.Session.Token, ownershipRequestBody{OrganizationName: "Cross", OrganizationSlug: slugs[1], ProjectName: "API"})
	created := performMonitorCreate(t, router, owner.Body.Session.Token, root.Body.Environment.ID, monitorCreateBody{Name: "API", URL: "https://example.com/health", IntervalSeconds: 60})
	if root.Status != http.StatusCreated || foreign.Status != http.StatusCreated || created.Status != http.StatusCreated {
		t.Fatal("system setup failed")
	}
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	if _, err := pool.Exec(ctx, `DELETE FROM monitor_schedule_periods WHERE monitor_id=$1::uuid`, created.Body.ID); err != nil {
		t.Fatal(err)
	}
	insertSchedulePeriod(t, ctx, pool, root.Body.Organization.ID, root.Body.Environment.ID, created.Body.ID, 1, 60, base, base, base.Add(5*time.Minute))
	for index := 0; index < 3; index++ {
		applyIncidentObservation(t, ctx, pool, created.Body.ID, base.Add(time.Duration(index)*time.Minute), false, true)
	}
	insertMonitorAPIResult(t, ctx, pool, root.Body.Organization.ID, root.Body.Environment.ID, created.Body.ID, "manual_test", base.Add(30*time.Second), true, int16(200), nil)

	from, to := base.Format(time.RFC3339), base.Add(5*time.Minute).Format(time.RFC3339)
	checks := phase14Request(t, router, http.MethodGet, fmt.Sprintf("/api/v1/environments/%s/monitors/%s/checks?from=%s&to=%s&limit=2", root.Body.Environment.ID, created.Body.ID, from, to), owner.Body.Session.Token, nil)
	if checks.Code != http.StatusOK || !strings.Contains(checks.Body.String(), `"next_cursor"`) {
		t.Fatalf("checks=%d %s", checks.Code, checks.Body.String())
	}
	manual := phase14Request(t, router, http.MethodGet, fmt.Sprintf("/api/v1/environments/%s/monitors/%s/checks?from=%s&to=%s&job_type=manual", root.Body.Environment.ID, created.Body.ID, from, to), owner.Body.Session.Token, nil)
	if manual.Code != http.StatusOK || !strings.Contains(manual.Body.String(), `"job_type":"manual"`) {
		t.Fatalf("manual checks=%d %s", manual.Code, manual.Body.String())
	}
	report := phase14Request(t, router, http.MethodGet, fmt.Sprintf("/api/v1/environments/%s/monitors/%s/report?from=%s&to=%s", root.Body.Environment.ID, created.Body.ID, from, to), owner.Body.Session.Token, nil)
	if report.Code != http.StatusOK || !strings.Contains(report.Body.String(), `"expected":5`) || !strings.Contains(report.Body.String(), `"observed":3`) || !strings.Contains(report.Body.String(), `"unknown":2`) {
		t.Fatalf("report=%d %s", report.Code, report.Body.String())
	}
	dashboard := phase14Request(t, router, http.MethodGet, fmt.Sprintf("/api/v1/environments/%s/dashboard?from=%s&to=%s", root.Body.Environment.ID, from, to), owner.Body.Session.Token, nil)
	if dashboard.Code != http.StatusOK || !strings.Contains(dashboard.Body.String(), `"open_incidents":1`) {
		t.Fatalf("dashboard=%d %s", dashboard.Code, dashboard.Body.String())
	}
	incidents := phase14Request(t, router, http.MethodGet, fmt.Sprintf("/api/v1/environments/%s/incidents?from=%s&to=%s", root.Body.Environment.ID, from, time.Now().UTC().Add(time.Hour).Format(time.RFC3339)), owner.Body.Session.Token, nil)
	if incidents.Code != http.StatusOK {
		t.Fatalf("incidents=%d %s", incidents.Code, incidents.Body.String())
	}
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	decodePhase14(t, incidents, &page)
	if len(page.Items) != 1 {
		t.Fatalf("incident page=%s", incidents.Body.String())
	}
	detail := phase14Request(t, router, http.MethodGet, "/api/v1/environments/"+root.Body.Environment.ID+"/incidents/"+page.Items[0].ID, owner.Body.Session.Token, nil)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"deliveries"`) || !strings.Contains(detail.Body.String(), `"events"`) {
		t.Fatalf("detail=%d %s", detail.Code, detail.Body.String())
	}
	acknowledged := phase14Request(t, router, http.MethodPost, "/api/v1/environments/"+root.Body.Environment.ID+"/incidents/"+page.Items[0].ID+"/acknowledge", owner.Body.Session.Token, map[string]any{"reason": "investigating"})
	if acknowledged.Code != http.StatusOK || !strings.Contains(acknowledged.Body.String(), `"acknowledged_at"`) {
		t.Fatalf("acknowledge=%d %s", acknowledged.Code, acknowledged.Body.String())
	}
	events, err := realtimeService.Poll(ctx, owner.Body.User.ID, root.Body.Environment.ID, 0, 100)
	if err != nil || len(events) < 4 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	last := events[len(events)-1].ID
	if replay, err := realtimeService.Poll(ctx, owner.Body.User.ID, root.Body.Environment.ID, last, 100); err != nil || len(replay) != 0 {
		t.Fatalf("poll reconstruction=%v err=%v", replay, err)
	}
	if _, err := realtimeService.Poll(ctx, cross.Body.User.ID, root.Body.Environment.ID, 0, 100); err != realtime.ErrNotFound {
		t.Fatalf("cross-tenant events error=%v", err)
	}
	if hidden := phase14Request(t, router, http.MethodGet, fmt.Sprintf("/api/v1/environments/%s/dashboard?from=%s&to=%s", root.Body.Environment.ID, from, to), cross.Body.Session.Token, nil); hidden.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant dashboard=%d %s", hidden.Code, hidden.Body.String())
	}
	if invalid := phase14Request(t, router, http.MethodGet, fmt.Sprintf("/api/v1/environments/%s/dashboard?from=%s&to=%s", root.Body.Environment.ID, base.Add(-32*24*time.Hour).Format(time.RFC3339), to), owner.Body.Session.Token, nil); invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unbounded range=%d %s", invalid.Code, invalid.Body.String())
	}
	if health := phase14Request(t, router, http.MethodGet, "/health/operations", "", nil); health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"maintenance"`) {
		t.Fatalf("operations health=%d %s", health.Code, health.Body.String())
	}
}

func openPhase14Pool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

func phase14Request(t *testing.T, handler http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodePhase14(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
}
