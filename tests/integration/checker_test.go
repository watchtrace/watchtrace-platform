package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/checker"
)

func TestHTTPCheckWorkerStoresOneSuccessfulResultWithPostgreSQL(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"checker-success"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deleteSchedulerTestData(t, cleanupCtx, pool, slugs)
	})

	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	monitorID, jobID, scheduledAt := insertCheckerJob(
		t, ctx, pool, organizationID, environmentID, "Successful check", "http://safe.test/health",
	)

	var requests atomic.Int32
	service, closeTarget := newControlledChecker(t, pool, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestNumber := requests.Add(1)
		if request.Host != "safe.test" || request.Header.Get("User-Agent") != "WatchTrace-Phase1/1.0" {
			t.Errorf("request host/User-Agent = %q/%q", request.Host, request.Header.Get("User-Agent"))
		}
		var state string
		var leaseOwner string
		var leaseToken string
		var leaseExpiresAt time.Time
		var databaseTime time.Time
		if err := pool.QueryRow(context.Background(), `
			SELECT state, lease_owner, lease_token::text, lease_expires_at, CURRENT_TIMESTAMP
			FROM check_jobs
			WHERE id = $1::text::uuid
		`, jobID).Scan(&state, &leaseOwner, &leaseToken, &leaseExpiresAt, &databaseTime); err != nil {
			t.Errorf("inspect committed running lease: %v", err)
		}
		wantWorker := fmt.Sprintf("worker-success-%d", requestNumber)
		if state != "running" || leaseOwner != wantWorker ||
			leaseExpiresAt.Before(databaseTime.Add(50*time.Second)) {
			t.Errorf(
				"visible lease state/owner/expiry = %q/%q/%s",
				state,
				leaseOwner,
				leaseExpiresAt.Sub(databaseTime),
			)
		}
		assertGeneratedUUID(t, leaseToken)
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(strings.Repeat("sensitive-body-not-stored", 4096)))
	}))
	defer closeTarget()

	claimed, err := service.RunNext(ctx, "worker-success-1")
	if err != nil || !claimed {
		t.Fatalf("run successful check: claimed=%t error=%v", claimed, err)
	}
	assertCompletedCheckResult(
		t, ctx, pool, jobID, organizationID, environmentID, monitorID, scheduledAt,
		true, http.StatusOK, nil, 1,
	)

	// Simulate an at-least-once redelivery of the stable job ID. The target may
	// see another request, but the result key must remain unique.
	if _, err := pool.Exec(ctx, `
		UPDATE check_jobs
		SET state = 'pending', completed_at = NULL
		WHERE id = $1::text::uuid
	`, jobID); err != nil {
		t.Fatalf("prepare stable job redelivery: %v", err)
	}
	claimed, err = service.RunNext(ctx, "worker-success-2")
	if err != nil || !claimed {
		t.Fatalf("run stable job redelivery: claimed=%t error=%v", claimed, err)
	}
	assertCompletedCheckResult(
		t, ctx, pool, jobID, organizationID, environmentID, monitorID, scheduledAt,
		true, http.StatusOK, nil, 2,
	)
	if requests.Load() != 2 {
		t.Fatalf("target requests = %d, want documented at-least-once duplicate", requests.Load())
	}

	claimed, err = service.RunNext(ctx, "worker-idle")
	if err != nil || claimed {
		t.Fatalf("empty queue: claimed=%t error=%v", claimed, err)
	}
}

func TestHTTPCheckWorkerSkipsLockedPendingJobWithPostgreSQL(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"checker-skip-locked"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deleteSchedulerTestData(t, cleanupCtx, pool, slugs)
	})

	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	_, firstJobID, _ := insertCheckerJob(
		t, ctx, pool, organizationID, environmentID, "Locked first", "http://safe.test/first",
	)
	_, secondJobID, _ := insertCheckerJob(
		t, ctx, pool, organizationID, environmentID, "Available second", "http://safe.test/second",
	)
	if _, err := pool.Exec(ctx, `
		UPDATE check_jobs
		SET scheduled_at = CURRENT_TIMESTAMP - INTERVAL '2 minutes'
		WHERE id = $1::text::uuid
	`, firstJobID); err != nil {
		t.Fatalf("order locked checker job: %v", err)
	}

	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin pending job lock: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	var lockedJobID string
	if err := lockTx.QueryRow(ctx, `
		SELECT id::text
		FROM check_jobs
		WHERE id = $1::text::uuid
		FOR UPDATE
	`, firstJobID).Scan(&lockedJobID); err != nil {
		t.Fatalf("lock oldest pending job: %v", err)
	}

	service, closeTarget := newControlledChecker(t, pool, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/second" {
			t.Errorf("claimed request path = %q, want unlocked second job", request.URL.Path)
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer closeTarget()
	claimed, err := service.RunNext(ctx, "worker-skip-locked")
	if err != nil || !claimed {
		t.Fatalf("claim around locked pending job: claimed=%t error=%v", claimed, err)
	}

	var firstState string
	var secondState string
	if err := pool.QueryRow(ctx, `SELECT state FROM check_jobs WHERE id = $1::text::uuid`, firstJobID).Scan(&firstState); err != nil {
		t.Fatalf("read locked first state: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM check_jobs WHERE id = $1::text::uuid`, secondJobID).Scan(&secondState); err != nil {
		t.Fatalf("read unlocked second state: %v", err)
	}
	if firstState != "pending" || secondState != "completed" {
		t.Fatalf("skip-locked states = %q/%q, want pending/completed", firstState, secondState)
	}
}

func TestHTTPCheckWorkerStoresUnexpectedStatusWithoutRetryWithPostgreSQL(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"checker-status-failure"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deleteSchedulerTestData(t, cleanupCtx, pool, slugs)
	})

	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	monitorID, jobID, scheduledAt := insertCheckerJob(
		t, ctx, pool, organizationID, environmentID, "Failed check", "http://safe.test/unavailable",
	)
	service, closeTarget := newControlledChecker(t, pool, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer closeTarget()

	claimed, err := service.RunNext(ctx, "worker-status-failure")
	if err != nil || !claimed {
		t.Fatalf("run failed check: claimed=%t error=%v", claimed, err)
	}
	category := "unexpected_status"
	assertCompletedCheckResult(
		t, ctx, pool, jobID, organizationID, environmentID, monitorID, scheduledAt,
		false, http.StatusServiceUnavailable, &category, 1,
	)
}

func TestHTTPCheckWorkerRollsBackResultWhenCompletionFails(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"checker-atomic-result"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		removeCheckerCompletionFailureTrigger(t, cleanupCtx, pool)
		deleteSchedulerTestData(t, cleanupCtx, pool, slugs)
	})

	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	_, jobID, _ := insertCheckerJob(
		t, ctx, pool, organizationID, environmentID, "Atomic result", "http://safe.test/health",
	)
	installCheckerCompletionFailureTrigger(t, ctx, pool, jobID)
	service, closeTarget := newControlledChecker(t, pool, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	}))
	defer closeTarget()

	claimed, err := service.RunNext(ctx, "worker-atomic")
	if !claimed || err == nil {
		t.Fatalf("forced completion failure: claimed=%t error=%v", claimed, err)
	}
	var state string
	var resultCount int
	if err := pool.QueryRow(ctx, `SELECT state FROM check_jobs WHERE id = $1::text::uuid`, jobID).Scan(&state); err != nil {
		t.Fatalf("read job after result rollback: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM health_checks WHERE job_id = $1::text::uuid`, jobID).Scan(&resultCount); err != nil {
		t.Fatalf("count rolled-back result: %v", err)
	}
	if state != "running" || resultCount != 0 {
		t.Fatalf("atomic rollback left state=%q result count=%d", state, resultCount)
	}
}

func TestHTTPCheckWorkerRejectsStaleLeaseCompletionWithPostgreSQL(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"checker-stale-lease"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deleteSchedulerTestData(t, cleanupCtx, pool, slugs)
	})

	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	monitorID, jobID, _ := insertCheckerJob(
		t, ctx, pool, organizationID, environmentID, "Stale lease", "http://safe.test/health",
	)
	service, closeTarget := newControlledChecker(t, pool, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		if _, err := pool.Exec(context.Background(), `
			UPDATE check_jobs
			SET lease_token = gen_random_uuid()
			WHERE monitor_id = $1::text::uuid AND state = 'running'
		`, monitorID); err != nil {
			t.Errorf("replace lease token: %v", err)
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer closeTarget()

	claimed, err := service.RunNext(ctx, "worker-stale")
	if !claimed || !errors.Is(err, checker.ErrLeaseLost) {
		t.Fatalf("stale completion: claimed=%t error=%v", claimed, err)
	}
	var resultCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM health_checks WHERE job_id = $1::text::uuid`, jobID).Scan(&resultCount); err != nil {
		t.Fatalf("count stale result: %v", err)
	}
	if resultCount != 0 {
		t.Fatalf("stale lease stored %d results", resultCount)
	}
}

func TestHTTPCheckWorkerSchemaConstraintsWithPostgreSQL(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"checker-schema-first", "checker-schema-second"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deleteSchedulerTestData(t, cleanupCtx, pool, slugs)
	})

	firstOrganizationID, firstEnvironmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	secondOrganizationID, secondEnvironmentID := insertSchedulerTenant(t, ctx, pool, slugs[1])
	firstMonitorID, firstJobID, scheduledAt := insertCheckerJob(
		t, ctx, pool, firstOrganizationID, firstEnvironmentID, "First schema", "http://safe.test/health",
	)
	secondMonitorID := insertSchedulerMonitor(
		t, ctx, pool, secondOrganizationID, secondEnvironmentID, "Second schema", 60, scheduledAt.Add(time.Hour),
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := pool.Exec(ctx, `
		INSERT INTO health_checks (
			job_id, organization_id, environment_id, monitor_id, job_type,
			scheduled_at, started_at, completed_at, succeeded, status_code,
			total_duration_microseconds
		) VALUES (
			$1::text::uuid, $2::text::uuid, $3::text::uuid, $4::text::uuid,
			'scheduled', $5, $6, $6, true, 200, 0
		)
	`, firstJobID, secondOrganizationID, secondEnvironmentID, firstMonitorID, scheduledAt, now)
	assertPostgreSQLErrorCode(t, err, "23503")

	_, err = pool.Exec(ctx, `
		INSERT INTO health_checks (
			job_id, organization_id, environment_id, monitor_id, job_type,
			scheduled_at, started_at, completed_at, succeeded,
			total_duration_microseconds
		) VALUES (
			gen_random_uuid(), $1::text::uuid, $2::text::uuid, $3::text::uuid,
			'scheduled', $4, $5, $5, true, 0
		)
	`, secondOrganizationID, secondEnvironmentID, secondMonitorID, scheduledAt, now)
	assertPostgreSQLErrorCode(t, err, "23514")

	var bodyColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = 'health_checks'
		  AND column_name ILIKE '%body%'
	`).Scan(&bodyColumns); err != nil {
		t.Fatalf("inspect response body columns: %v", err)
	}
	if bodyColumns != 0 {
		t.Fatalf("health_checks contains %d response body columns", bodyColumns)
	}
	for _, indexName := range []string{"check_jobs_claim_idx", "health_checks_monitor_history_idx"} {
		var relationName *string
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, "public."+indexName).Scan(&relationName); err != nil {
			t.Fatalf("inspect checker index %s: %v", indexName, err)
		}
		if relationName == nil {
			t.Errorf("checker index %s is absent", indexName)
		}
	}
}

func TestHTTPCheckWorkerSchemaRollback(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_CHECKER_SCHEMA_ABSENT") != "1" {
		t.Skip("WATCHTRACE_EXPECT_CHECKER_SCHEMA_ABSENT is not set")
	}

	ctx, tx := beginOwnershipSchemaTest(t)
	var relationName *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.health_checks')::text`).Scan(&relationName); err != nil {
		t.Fatalf("inspect rolled-back health_checks table: %v", err)
	}
	if relationName != nil {
		t.Fatal("health_checks still exists after checker migration rollback")
	}
	var leaseColumns int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = 'check_jobs'
		  AND column_name IN ('attempt_count', 'max_attempts', 'lease_owner', 'lease_token', 'lease_expires_at')
	`).Scan(&leaseColumns); err != nil {
		t.Fatalf("inspect rolled-back checker columns: %v", err)
	}
	if leaseColumns != 0 {
		t.Fatalf("check_jobs retains %d checker columns after rollback", leaseColumns)
	}
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.check_jobs')::text`).Scan(&relationName); err != nil {
		t.Fatalf("inspect preserved check_jobs table: %v", err)
	}
	if relationName == nil {
		t.Fatal("preceding check_jobs table is absent after checker rollback")
	}
}

func insertCheckerJob(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	organizationID string,
	environmentID string,
	name string,
	targetURL string,
) (string, string, time.Time) {
	t.Helper()
	scheduledAt := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	monitorID := insertSchedulerMonitor(
		t, ctx, pool, organizationID, environmentID, name, 60, scheduledAt.Add(time.Hour),
	)
	if _, err := pool.Exec(ctx, `UPDATE monitors SET target_url = $2 WHERE id = $1::text::uuid`, monitorID, targetURL); err != nil {
		t.Fatalf("set checker monitor target: %v", err)
	}
	var jobID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO check_jobs (
			organization_id, environment_id, monitor_id, scheduled_at
		) VALUES ($1::text::uuid, $2::text::uuid, $3::text::uuid, $4)
		RETURNING id::text
	`, organizationID, environmentID, monitorID, scheduledAt).Scan(&jobID); err != nil {
		t.Fatalf("insert checker job: %v", err)
	}
	return monitorID, jobID, scheduledAt
}

func newControlledChecker(
	t *testing.T,
	pool *pgxpool.Pool,
	handler http.Handler,
) (*checker.Service, func()) {
	t.Helper()
	target := httptest.NewServer(handler)
	resolver := checkerResolver{address: netip.MustParseAddr("8.8.8.8")}
	dialer := checkerTargetDialer{target: strings.TrimPrefix(target.URL, "http://")}
	return checker.NewServiceWithNetwork(pool, resolver, dialer), target.Close
}

type checkerResolver struct {
	address netip.Addr
}

func (resolver checkerResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	if network != "ip" || host != "safe.test" {
		return nil, fmt.Errorf("unexpected controlled resolution")
	}
	return []netip.Addr{resolver.address}, nil
}

type checkerTargetDialer struct {
	target string
}

func (dialer checkerTargetDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, dialer.target)
}

func assertCompletedCheckResult(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	jobID string,
	organizationID string,
	environmentID string,
	monitorID string,
	scheduledAt time.Time,
	wantSucceeded bool,
	wantStatus int,
	wantCategory *string,
	wantAttempts int,
) {
	t.Helper()
	var state string
	var attempts int
	var leaseOwner *string
	var leaseToken *string
	var leaseExpiresAt *time.Time
	var startedAt time.Time
	var completedAt time.Time
	if err := pool.QueryRow(ctx, `
		SELECT state, attempt_count, lease_owner, lease_token::text, lease_expires_at,
		       started_at, completed_at
		FROM check_jobs
		WHERE id = $1::text::uuid
	`, jobID).Scan(
		&state, &attempts, &leaseOwner, &leaseToken, &leaseExpiresAt, &startedAt, &completedAt,
	); err != nil {
		t.Fatalf("read completed check job: %v", err)
	}
	if state != "completed" || attempts != wantAttempts ||
		leaseOwner != nil || leaseToken != nil || leaseExpiresAt != nil {
		t.Fatalf("completed job state=%q attempts=%d lease=%v/%v/%v", state, attempts, leaseOwner, leaseToken, leaseExpiresAt)
	}
	if completedAt.Before(startedAt) {
		t.Fatalf("job completed at %s before start %s", completedAt, startedAt)
	}

	var storedOrganizationID string
	var storedEnvironmentID string
	var storedMonitorID string
	var storedScheduledAt time.Time
	var succeeded bool
	var statusCode *int
	var category *string
	var durationMicros int64
	var resultCount int
	if err := pool.QueryRow(ctx, `
		SELECT organization_id::text, environment_id::text, monitor_id::text,
		       scheduled_at, succeeded, status_code, error_category,
		       total_duration_microseconds,
		       count(*) OVER ()
		FROM health_checks
		WHERE job_id = $1::text::uuid
	`, jobID).Scan(
		&storedOrganizationID, &storedEnvironmentID, &storedMonitorID,
		&storedScheduledAt, &succeeded, &statusCode, &category, &durationMicros, &resultCount,
	); err != nil {
		t.Fatalf("read stored health check: %v", err)
	}
	if resultCount != 1 || storedOrganizationID != organizationID ||
		storedEnvironmentID != environmentID || storedMonitorID != monitorID ||
		!storedScheduledAt.Equal(scheduledAt) || succeeded != wantSucceeded ||
		statusCode == nil || *statusCode != wantStatus || durationMicros < 0 {
		t.Fatalf("stored health check has unexpected values")
	}
	if (category == nil) != (wantCategory == nil) ||
		(category != nil && *category != *wantCategory) {
		t.Fatalf("error category = %v, want %v", category, wantCategory)
	}
}

func installCheckerCompletionFailureTrigger(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	jobID string,
) {
	t.Helper()
	removeCheckerCompletionFailureTrigger(t, ctx, pool)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION watchtrace_test_reject_check_completion() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			IF NEW.id = '%s'::uuid AND NEW.state = 'completed' THEN
				RAISE EXCEPTION 'forced check completion failure';
			END IF;
			RETURN NEW;
		END;
		$function$;

		CREATE TRIGGER watchtrace_test_reject_check_completion
		BEFORE UPDATE OF state ON check_jobs
		FOR EACH ROW EXECUTE FUNCTION watchtrace_test_reject_check_completion();
	`, jobID)); err != nil {
		t.Fatalf("install checker completion failure trigger: %v", err)
	}
}

func removeCheckerCompletionFailureTrigger(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		DROP TRIGGER IF EXISTS watchtrace_test_reject_check_completion ON check_jobs;
		DROP FUNCTION IF EXISTS watchtrace_test_reject_check_completion();
	`); err != nil {
		t.Fatalf("remove checker completion failure trigger: %v", err)
	}
}
