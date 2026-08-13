package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
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
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
)

func TestSecureMonitorLifecycleWithPostgreSQL(t *testing.T) {
	databaseURL := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	email, slug := "monitor-lifecycle@example.test", "monitor-lifecycle"
	deleteOwnershipTestData(t, ctx, pool, []string{email}, []string{slug})
	t.Cleanup(func() { deleteOwnershipTestData(t, context.Background(), pool, []string{email}, []string{slug}) })

	authService := auth.NewService(pool, &recordingVerificationSender{})
	keys, _ := secureheaders.New(1, map[int32][]byte{1: bytes.Repeat([]byte{9}, 32)})
	platformPublic, platformPrivate, _ := ed25519.GenerateKey(rand.Reader)
	_ = platformPublic
	service := monitor.NewServiceWithQueue(pool, keys, platformPrivate, "platform-v1")
	router := httpapi.NewRouter(httpapi.Options{Logger: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), AuthService: authService, Authenticator: authService, OwnershipService: ownership.NewService(pool), MonitorService: service})
	signup := performAuthRequest(t, router, "/api/v1/auth/signup", email, "P1-301-lifecycle-password!")
	owned := performOwnershipRequest(t, router, signup.Body.Session.Token, ownershipRequestBody{OrganizationName: "Lifecycle", OrganizationSlug: slug, ProjectName: "Checks"})
	workerPrivate, _ := ecdh.X25519().GenerateKey(rand.Reader)
	resultPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err = pool.Exec(ctx, `UPDATE worker_pools SET encryption_key_id='enc-v1',encryption_public_key=$1,result_key_id='result-v1',result_public_key=$2,job_queue_url='https://sqs.local/jobs.fifo' WHERE id='hosted'`, workerPrivate.PublicKey().Bytes(), resultPublic); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE worker_pools SET encryption_key_id=NULL,encryption_public_key=NULL,result_key_id=NULL,result_public_key=NULL,job_queue_url=NULL WHERE id='hosted'`)
	})
	const secret = "Bearer lifecycle-secret"
	created := performMonitorCreate(t, router, signup.Body.Session.Token, owned.Body.Environment.ID, monitorCreateBody{Name: "Lifecycle", URL: "https://example.test/head", Method: "HEAD", Headers: map[string]string{"Authorization": secret}})
	if created.Status != http.StatusCreated || created.Body.Method != "HEAD" || created.Body.Version != 1 || len(created.Body.HeaderNames) != 1 || strings.Contains(created.RawBody, secret) {
		t.Fatalf("secure create = %d %+v", created.Status, created.Body)
	}
	var ciphertext []byte
	if err = pool.QueryRow(ctx, `SELECT headers_ciphertext FROM monitors WHERE id=$1::uuid`, created.Body.ID).Scan(&ciphertext); err != nil || strings.Contains(string(ciphertext), secret) {
		t.Fatal("monitor header was not encrypted at rest")
	}

	updated := performMonitorLifecycle(t, router, http.MethodPut, signup.Body.Session.Token, owned.Body.Environment.ID, created.Body.ID, monitorCreateBody{Name: "Updated", URL: "https://example.test/get", Method: "GET", IntervalSeconds: 60, TimeoutSeconds: 3, ExpectedStatusMin: 200, ExpectedStatusMax: 204})
	if updated.Status != http.StatusOK || updated.Body.Version != 2 || updated.Body.Method != "GET" {
		t.Fatalf("update = %d %+v", updated.Status, updated.Body)
	}
	paused := performMonitorLifecycle(t, router, http.MethodPost, signup.Body.Session.Token, owned.Body.Environment.ID, created.Body.ID+"/pause", nil)
	if paused.Status != http.StatusOK || !paused.Body.Paused || paused.Body.Version != 3 {
		t.Fatalf("pause = %d %+v", paused.Status, paused.Body)
	}
	resumed := performMonitorLifecycle(t, router, http.MethodPost, signup.Body.Session.Token, owned.Body.Environment.ID, created.Body.ID+"/resume", nil)
	if resumed.Status != http.StatusOK || resumed.Body.Paused || resumed.Body.Version != 4 {
		t.Fatalf("resume = %d %+v", resumed.Status, resumed.Body)
	}
	tested := performMonitorLifecycle(t, router, http.MethodPost, signup.Body.Session.Token, owned.Body.Environment.ID, created.Body.ID+"/test", nil)
	if tested.Status != http.StatusAccepted {
		t.Fatalf("test-now = %d %s", tested.Status, tested.RawBody)
	}
	var manualJobs, outboxRows int
	if err = pool.QueryRow(ctx, `SELECT count(*),(SELECT count(*) FROM check_dispatch_outbox o JOIN check_jobs j ON j.id=o.job_id WHERE j.monitor_id=$1::uuid AND j.job_type='manual_test') FROM check_jobs WHERE monitor_id=$1::uuid AND job_type='manual_test'`, created.Body.ID).Scan(&manualJobs, &outboxRows); err != nil || manualJobs != 1 || outboxRows != 1 {
		t.Fatalf("manual job/outbox = %d/%d error=%v", manualJobs, outboxRows, err)
	}
	for i := 1; i < 10; i++ {
		attempt := performMonitorLifecycle(t, router, http.MethodPost, signup.Body.Session.Token, owned.Body.Environment.ID, created.Body.ID+"/test", nil)
		if attempt.Status != http.StatusAccepted {
			t.Fatalf("manual attempt %d status=%d", i+1, attempt.Status)
		}
	}
	overflow := performMonitorLifecycle(t, router, http.MethodPost, signup.Body.Session.Token, owned.Body.Environment.ID, created.Body.ID+"/test", nil)
	if overflow.Status != http.StatusTooManyRequests {
		t.Fatalf("eleventh manual attempt status=%d", overflow.Status)
	}
	deleted := performMonitorLifecycle(t, router, http.MethodDelete, signup.Body.Session.Token, owned.Body.Environment.ID, created.Body.ID, nil)
	if deleted.Status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", deleted.Status, deleted.RawBody)
	}
	if got := performMonitorGet(t, router, signup.Body.Session.Token, owned.Body.Environment.ID, created.Body.ID); got.Status != http.StatusNotFound {
		t.Fatalf("soft-deleted monitor read status = %d", got.Status)
	}
}

func performMonitorLifecycle(t *testing.T, handler http.Handler, method, token, environmentID, suffix string, input any) monitorAPIResult {
	t.Helper()
	var body bytes.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = *bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, "/api/v1/environments/"+environmentID+"/monitors/"+suffix, &body)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	result := monitorAPIResult{Status: response.Code, RawBody: response.Body.String()}
	if response.Code == http.StatusNoContent || response.Code == http.StatusAccepted {
		return result
	}
	if response.Code >= 400 {
		var errorBody httpapi.ErrorResponse
		_ = json.Unmarshal(response.Body.Bytes(), &errorBody)
		result.ErrorCode = errorBody.Error.Code
		return result
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result.Body); err != nil {
		t.Fatal(err)
	}
	return result
}
