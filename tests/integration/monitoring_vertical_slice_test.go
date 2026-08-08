package integration_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/httpapi"
	"github.com/watchtrace/watchtrace-platform/internal/monitor"
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
	"github.com/watchtrace/watchtrace-platform/internal/scheduler"
)

func TestBackendMonitoringVerticalSliceWithPostgreSQL(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	emails := []string{"vertical-slice@example.test"}
	slugs := []string{"vertical-slice"}
	deleteOwnershipTestData(t, ctx, pool, emails, slugs)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deleteOwnershipTestData(t, cleanupCtx, pool, emails, slugs)
	})

	const (
		targetURL          = "http://safe.test/vertical-slice"
		responseBodyMarker = "vertical-slice-sensitive-response-body"
	)
	var targetRequests atomic.Int32
	checkService, closeTarget := newControlledChecker(t, pool, http.HandlerFunc(
		func(response http.ResponseWriter, request *http.Request) {
			targetRequests.Add(1)
			if request.Method != http.MethodGet || request.Host != "safe.test" ||
				request.URL.Path != "/vertical-slice" ||
				request.Header.Get("User-Agent") != "WatchTrace-Phase1/1.0" {
				t.Errorf("unexpected controlled target request: method=%q host=%q path=%q user-agent=%q",
					request.Method, request.Host, request.URL.Path, request.Header.Get("User-Agent"))
			}
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write([]byte(strings.Repeat(responseBodyMarker, 4096)))
		},
	))
	defer closeTarget()

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

	signup := performAuthRequest(
		t,
		router,
		"/api/v1/auth/signup",
		emails[0],
		"P1-109-vertical-slice-password!",
	)
	if signup.Status != http.StatusCreated {
		t.Fatalf("vertical-slice signup = %d: %s", signup.Status, signup.RawBody)
	}
	owned := performOwnershipRequest(t, router, signup.Body.Session.Token, ownershipRequestBody{
		OrganizationName: "Vertical Slice",
		OrganizationSlug: slugs[0],
		ProjectName:      "Monitored API",
	})
	if owned.Status != http.StatusCreated {
		t.Fatalf("vertical-slice ownership = %d: %s", owned.Status, owned.RawBody)
	}
	created := performMonitorCreate(
		t,
		router,
		signup.Body.Session.Token,
		owned.Body.Environment.ID,
		monitorCreateBody{
			Name:            "Controlled target",
			URL:             targetURL,
			IntervalSeconds: 60,
			TimeoutSeconds:  5,
		},
	)
	if created.Status != http.StatusCreated {
		t.Fatalf("vertical-slice monitor = %d: %s", created.Status, created.RawBody)
	}

	initial := performMonitorGet(
		t,
		router,
		signup.Body.Session.Token,
		owned.Body.Environment.ID,
		created.Body.ID,
	)
	if initial.Status != http.StatusOK || initial.Body.State != monitor.StateUnknown ||
		len(initial.Body.RecentChecks) != 0 {
		t.Fatalf("initial monitor read = status %d state %q checks %d: %s",
			initial.Status, initial.Body.State, len(initial.Body.RecentChecks), initial.RawBody)
	}

	createdJobs, err := scheduler.NewService(pool).ScheduleDue(ctx, scheduler.DefaultBatchSize)
	if err != nil {
		t.Fatalf("schedule vertical-slice monitor: %v", err)
	}
	if createdJobs != 1 {
		t.Fatalf("scheduled jobs = %d, want 1", createdJobs)
	}
	claimed, err := checkService.RunNext(ctx, "vertical-slice-worker")
	if err != nil || !claimed {
		t.Fatalf("execute vertical-slice check: claimed=%t error=%v", claimed, err)
	}

	detail := waitForScheduledResult(
		t,
		router,
		signup.Body.Session.Token,
		owned.Body.Environment.ID,
		created.Body.ID,
	)
	if detail.Body.ID != created.Body.ID ||
		detail.Body.OrganizationID != owned.Body.Organization.ID ||
		detail.Body.EnvironmentID != owned.Body.Environment.ID ||
		detail.Body.State != monitor.StateHealthy || len(detail.Body.RecentChecks) != 1 {
		t.Fatalf("completed monitor detail is incorrect: %+v", detail.Body)
	}
	result := detail.Body.RecentChecks[0]
	if result.JobID == "" || result.JobType != "scheduled" || !result.Succeeded ||
		result.StatusCode == nil || *result.StatusCode != http.StatusOK ||
		result.ErrorCategory != nil || result.TotalDurationMicroseconds < 0 ||
		result.ScheduledAt.IsZero() || result.StartedAt.Before(result.ScheduledAt) ||
		result.CompletedAt.Before(result.StartedAt) {
		t.Fatalf("completed scheduled result is incorrect: %+v", result)
	}
	if targetRequests.Load() != 1 {
		t.Fatalf("controlled target requests = %d, want 1", targetRequests.Load())
	}

	for name, content := range map[string]string{
		"authorized API response": detail.RawBody,
		"request logs":            logs.String(),
	} {
		for _, secret := range []string{responseBodyMarker, signup.Body.Session.Token} {
			if strings.Contains(content, secret) {
				t.Fatalf("%s contains a response body or session token", name)
			}
		}
	}
	if strings.Contains(logs.String(), targetURL) {
		t.Fatal("request logs contain the monitor target URL")
	}
}

func waitForScheduledResult(
	t *testing.T,
	handler http.Handler,
	token string,
	environmentID string,
	monitorID string,
) monitorGetAPIResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		detail := performMonitorGet(t, handler, token, environmentID, monitorID)
		if detail.Status != http.StatusOK {
			t.Fatalf("poll monitor result = %d %q: %s", detail.Status, detail.ErrorCode, detail.RawBody)
		}
		if len(detail.Body.RecentChecks) > 0 {
			return detail
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for scheduled result through the authorized API")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
