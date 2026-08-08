package integration_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/scheduler"
)

func TestInitialSchedulerBatchWithPostgreSQL(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"scheduler-batch"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deleteSchedulerTestData(t, cleanupCtx, pool, slugs)
	})

	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, "scheduler-batch")
	baseTime := time.Now().UTC().Truncate(time.Microsecond)
	firstScheduledAt := baseTime.Add(-125 * time.Second)
	secondScheduledAt := baseTime.Add(-65 * time.Second)
	thirdScheduledAt := baseTime.Add(-5 * time.Second)
	firstMonitorID := insertSchedulerMonitor(
		t, ctx, pool, organizationID, environmentID, "First due", 60, firstScheduledAt,
	)
	secondMonitorID := insertSchedulerMonitor(
		t, ctx, pool, organizationID, environmentID, "Second due", 120, secondScheduledAt,
	)
	thirdMonitorID := insertSchedulerMonitor(
		t, ctx, pool, organizationID, environmentID, "Third due", 300, thirdScheduledAt,
	)
	insertSchedulerMonitor(
		t, ctx, pool, organizationID, environmentID, "Future", 60, baseTime.Add(time.Hour),
	)

	service := scheduler.NewService(pool)
	created, err := service.ScheduleDue(ctx, 2)
	if err != nil {
		t.Fatalf("schedule first batch: %v", err)
	}
	if created != 2 {
		t.Fatalf("first batch created %d jobs, want 2", created)
	}

	assertScheduledJob(t, ctx, pool, firstMonitorID, firstScheduledAt)
	assertScheduledJob(t, ctx, pool, secondMonitorID, secondScheduledAt)
	assertNoScheduledJob(t, ctx, pool, thirdMonitorID)
	assertMonitorAdvancedPastDatabaseTime(t, ctx, pool, firstMonitorID, firstScheduledAt, 60)
	assertMonitorAdvancedPastDatabaseTime(t, ctx, pool, secondMonitorID, secondScheduledAt, 120)

	created, err = service.ScheduleDue(ctx, scheduler.DefaultBatchSize)
	if err != nil {
		t.Fatalf("schedule remaining due monitors: %v", err)
	}
	if created != 1 {
		t.Fatalf("second batch created %d jobs, want 1", created)
	}
	assertScheduledJob(t, ctx, pool, thirdMonitorID, thirdScheduledAt)

	if _, err := pool.Exec(ctx, `
		UPDATE monitors
		SET next_check_at = CURRENT_TIMESTAMP - INTERVAL '1 second'
		WHERE id = $1::text::uuid
	`, thirdMonitorID); err != nil {
		t.Fatalf("make monitor due with outstanding job: %v", err)
	}
	created, err = service.ScheduleDue(ctx, scheduler.DefaultBatchSize)
	if err != nil {
		t.Fatalf("schedule with outstanding job: %v", err)
	}
	if created != 0 {
		t.Fatalf("scheduler created %d duplicate outstanding jobs", created)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE check_jobs
		SET state = 'completed',
		    started_at = CURRENT_TIMESTAMP,
		    completed_at = CURRENT_TIMESTAMP
		WHERE monitor_id = $1::text::uuid
	`, thirdMonitorID); err != nil {
		t.Fatalf("complete scheduled job for idempotent retry: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE monitors
		SET next_check_at = $2
		WHERE id = $1::text::uuid
	`, thirdMonitorID, thirdScheduledAt); err != nil {
		t.Fatalf("prepare idempotent scheduler retry: %v", err)
	}
	created, err = service.ScheduleDue(ctx, scheduler.DefaultBatchSize)
	if err != nil {
		t.Fatalf("retry already-scheduled planned time: %v", err)
	}
	if created != 0 {
		t.Fatalf("idempotent retry created %d jobs, want 0", created)
	}
	assertMonitorAdvancedPastDatabaseTime(t, ctx, pool, thirdMonitorID, thirdScheduledAt, 300)

	restartedPool, err := pgxpool.New(ctx, os.Getenv("WATCHTRACE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("reopen PostgreSQL pool: %v", err)
	}
	defer restartedPool.Close()
	var durableJobs int
	if err := restartedPool.QueryRow(ctx, `
		SELECT count(*)
		FROM check_jobs
		WHERE organization_id = $1::text::uuid
	`, organizationID).Scan(&durableJobs); err != nil {
		t.Fatalf("count durable jobs after scheduler recreation: %v", err)
	}
	if durableJobs != 3 {
		t.Fatalf("durable job count = %d, want 3", durableJobs)
	}
}

func TestInitialSchedulerConstraintsWithPostgreSQL(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"scheduler-constraints-first", "scheduler-constraints-second"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deleteSchedulerTestData(t, cleanupCtx, pool, slugs)
	})

	firstOrganizationID, firstEnvironmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	secondOrganizationID, secondEnvironmentID := insertSchedulerTenant(t, ctx, pool, slugs[1])
	scheduledAt := time.Now().UTC().Truncate(time.Microsecond)
	firstMonitorID := insertSchedulerMonitor(
		t, ctx, pool, firstOrganizationID, firstEnvironmentID, "First", 60, scheduledAt,
	)
	secondMonitorID := insertSchedulerMonitor(
		t, ctx, pool, secondOrganizationID, secondEnvironmentID, "Second", 60, scheduledAt,
	)

	_, err := pool.Exec(ctx, `
		INSERT INTO check_jobs (
			organization_id, environment_id, monitor_id, scheduled_at
		) VALUES ($1::text::uuid, $2::text::uuid, $3::text::uuid, $4)
	`, firstOrganizationID, firstEnvironmentID, firstMonitorID, scheduledAt)
	if err != nil {
		t.Fatalf("insert first scheduled job: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO check_jobs (
			organization_id, environment_id, monitor_id, scheduled_at
		) VALUES ($1::text::uuid, $2::text::uuid, $3::text::uuid, $4)
	`, firstOrganizationID, firstEnvironmentID, firstMonitorID, scheduledAt)
	assertPostgreSQLErrorCode(t, err, "23505")

	_, err = pool.Exec(ctx, `
		INSERT INTO check_jobs (
			organization_id, environment_id, monitor_id, scheduled_at
		) VALUES ($1::text::uuid, $2::text::uuid, $3::text::uuid, $4)
	`, firstOrganizationID, firstEnvironmentID, firstMonitorID, scheduledAt.Add(time.Minute))
	assertPostgreSQLErrorCode(t, err, "23505")

	_, err = pool.Exec(ctx, `
		INSERT INTO check_jobs (
			organization_id, environment_id, monitor_id, scheduled_at
		) VALUES ($1::text::uuid, $2::text::uuid, $3::text::uuid, $4)
	`, firstOrganizationID, firstEnvironmentID, secondMonitorID, scheduledAt)
	assertPostgreSQLErrorCode(t, err, "23503")

	for _, columnName := range []string{"scheduled_at", "started_at", "completed_at", "created_at"} {
		var dataType string
		if err := pool.QueryRow(ctx, `
			SELECT data_type
			FROM information_schema.columns
			WHERE table_schema = 'public'
			  AND table_name = 'check_jobs'
			  AND column_name = $1
		`, columnName).Scan(&dataType); err != nil {
			t.Fatalf("inspect check_jobs.%s: %v", columnName, err)
		}
		if dataType != "timestamp with time zone" {
			t.Errorf("check_jobs.%s type = %q", columnName, dataType)
		}
	}
	for _, indexName := range []string{
		"monitors_due_schedule_idx",
		"check_jobs_pending_schedule_idx",
		"check_jobs_monitor_history_idx",
		"check_jobs_one_outstanding_scheduled_idx",
	} {
		var relationName *string
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, "public."+indexName).Scan(&relationName); err != nil {
			t.Fatalf("inspect scheduler index %s: %v", indexName, err)
		}
		if relationName == nil {
			t.Errorf("scheduler index %s is absent", indexName)
		}
	}
	var nextCheckAtType string
	if err := pool.QueryRow(ctx, `
		SELECT data_type
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = 'monitors'
		  AND column_name = 'next_check_at'
	`).Scan(&nextCheckAtType); err != nil {
		t.Fatalf("inspect monitors.next_check_at: %v", err)
	}
	if nextCheckAtType != "timestamp with time zone" {
		t.Errorf("monitors.next_check_at type = %q", nextCheckAtType)
	}
}

func TestInitialSchedulerRollsBackJobWhenAdvanceFails(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"scheduler-atomicity"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		removeSchedulerFailureTrigger(t, cleanupCtx, pool)
		deleteSchedulerTestData(t, cleanupCtx, pool, slugs)
	})

	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	scheduledAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	monitorID := insertSchedulerMonitor(
		t, ctx, pool, organizationID, environmentID, "Reject schedule advance", 60, scheduledAt,
	)
	installSchedulerFailureTrigger(t, ctx, pool)

	if _, err := scheduler.NewService(pool).ScheduleDue(ctx, 1); err == nil {
		t.Fatal("scheduler unexpectedly committed after forced advance failure")
	}

	assertNoScheduledJob(t, ctx, pool, monitorID)
	var storedSchedule time.Time
	if err := pool.QueryRow(ctx, `
		SELECT next_check_at
		FROM monitors
		WHERE id = $1::text::uuid
	`, monitorID).Scan(&storedSchedule); err != nil {
		t.Fatalf("read rolled-back monitor schedule: %v", err)
	}
	if !storedSchedule.Equal(scheduledAt) {
		t.Fatalf("schedule after rollback = %s, want %s", storedSchedule, scheduledAt)
	}
}

func TestInitialSchedulerConcurrentBatchesDoNotDuplicateJobs(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"scheduler-concurrent"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deleteSchedulerTestData(t, cleanupCtx, pool, slugs)
	})

	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	scheduledAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	for index := 0; index < 12; index++ {
		insertSchedulerMonitor(
			t,
			ctx,
			pool,
			organizationID,
			environmentID,
			fmt.Sprintf("Concurrent %02d", index),
			60,
			scheduledAt,
		)
	}

	start := make(chan struct{})
	results := make(chan int, 2)
	errors := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for range 2 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			created, err := scheduler.NewService(pool).ScheduleDue(ctx, 6)
			results <- created
			errors <- err
		}()
	}
	close(start)
	waitGroup.Wait()
	close(results)
	close(errors)

	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent scheduler: %v", err)
		}
	}
	totalCreated := 0
	for created := range results {
		totalCreated += created
	}
	if totalCreated != 12 {
		t.Fatalf("concurrent schedulers created %d jobs, want 12", totalCreated)
	}

	var jobCount int
	var duplicateMonitors int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM check_jobs
		WHERE organization_id = $1::text::uuid
	`, organizationID).Scan(&jobCount); err != nil {
		t.Fatalf("count concurrent jobs: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM (
			SELECT monitor_id
			FROM check_jobs
			WHERE organization_id = $1::text::uuid
			GROUP BY monitor_id
			HAVING count(*) > 1
		) AS duplicates
	`, organizationID).Scan(&duplicateMonitors); err != nil {
		t.Fatalf("count duplicate monitor jobs: %v", err)
	}
	if jobCount != 12 || duplicateMonitors != 0 {
		t.Fatalf("job count = %d, duplicate monitors = %d", jobCount, duplicateMonitors)
	}
}

func TestSchedulerSchemaRollback(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_SCHEDULER_SCHEMA_ABSENT") != "1" {
		t.Skip("WATCHTRACE_EXPECT_SCHEDULER_SCHEMA_ABSENT is not set")
	}

	ctx, tx := beginOwnershipSchemaTest(t)
	var relationName *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.check_jobs')::text`).Scan(&relationName); err != nil {
		t.Fatalf("inspect rolled-back check_jobs table: %v", err)
	}
	if relationName != nil {
		t.Fatal("check_jobs still exists after migration rollback")
	}

	var nextCheckColumnCount int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = 'monitors'
		  AND column_name = 'next_check_at'
	`).Scan(&nextCheckColumnCount); err != nil {
		t.Fatalf("inspect rolled-back monitor schedule column: %v", err)
	}
	if nextCheckColumnCount != 0 {
		t.Fatal("monitors.next_check_at still exists after migration rollback")
	}
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.monitors')::text`).Scan(&relationName); err != nil {
		t.Fatalf("inspect preserved monitors table: %v", err)
	}
	if relationName == nil {
		t.Fatal("preceding monitors table is absent after scheduler rollback")
	}
}

func openSchedulerTestPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create scheduler PostgreSQL pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("connect scheduler PostgreSQL pool: %v", err)
	}
	return ctx, pool
}

func insertSchedulerTenant(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	slug string,
) (string, string) {
	t.Helper()
	var organizationID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO organizations (name, slug)
		VALUES ($1, $2)
		RETURNING id::text
	`, slug, slug).Scan(&organizationID); err != nil {
		t.Fatalf("insert scheduler organization: %v", err)
	}
	var projectID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO projects (organization_id, name)
		VALUES ($1::text::uuid, 'Scheduler project')
		RETURNING id::text
	`, organizationID).Scan(&projectID); err != nil {
		t.Fatalf("insert scheduler project: %v", err)
	}
	var environmentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO environments (organization_id, project_id, name, environment_type)
		VALUES ($1::text::uuid, $2::text::uuid, 'Production', 'production')
		RETURNING id::text
	`, organizationID, projectID).Scan(&environmentID); err != nil {
		t.Fatalf("insert scheduler environment: %v", err)
	}
	return organizationID, environmentID
}

func insertSchedulerMonitor(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	organizationID string,
	environmentID string,
	name string,
	intervalSeconds int,
	nextCheckAt time.Time,
) string {
	t.Helper()
	var monitorID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO monitors (
			organization_id,
			environment_id,
			name,
			target_url,
			interval_seconds,
			next_check_at
		)
		VALUES ($1::text::uuid, $2::text::uuid, $3, 'https://example.test/health', $4, $5)
		RETURNING id::text
	`, organizationID, environmentID, name, intervalSeconds, nextCheckAt).Scan(&monitorID); err != nil {
		t.Fatalf("insert scheduler monitor: %v", err)
	}
	return monitorID
}

func assertScheduledJob(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	monitorID string,
	wantScheduledAt time.Time,
) {
	t.Helper()
	var jobID string
	var jobType string
	var state string
	var scheduledAt time.Time
	var startedAt *time.Time
	var completedAt *time.Time
	var createdAt time.Time
	if err := pool.QueryRow(ctx, `
		SELECT id::text, job_type, state, scheduled_at, started_at, completed_at, created_at
		FROM check_jobs
		WHERE monitor_id = $1::text::uuid
	`, monitorID).Scan(
		&jobID,
		&jobType,
		&state,
		&scheduledAt,
		&startedAt,
		&completedAt,
		&createdAt,
	); err != nil {
		t.Fatalf("read scheduled job: %v", err)
	}
	assertGeneratedUUID(t, jobID)
	if jobType != "scheduled" || state != "pending" {
		t.Fatalf("job type/state = %q/%q", jobType, state)
	}
	if !scheduledAt.Equal(wantScheduledAt) {
		t.Fatalf("scheduled_at = %s, want %s", scheduledAt, wantScheduledAt)
	}
	if startedAt != nil || completedAt != nil {
		t.Fatalf("new job has lifecycle timestamps: started=%v completed=%v", startedAt, completedAt)
	}
	assertRecentTimestamp(t, createdAt)
}

func assertNoScheduledJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, monitorID string) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM check_jobs
		WHERE monitor_id = $1::text::uuid
	`, monitorID).Scan(&count); err != nil {
		t.Fatalf("count scheduled jobs: %v", err)
	}
	if count != 0 {
		t.Fatalf("monitor has %d jobs, want none", count)
	}
}

func assertMonitorAdvancedPastDatabaseTime(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	monitorID string,
	previousSchedule time.Time,
	intervalSeconds int,
) {
	t.Helper()
	var nextCheckAt time.Time
	var databaseTime time.Time
	if err := pool.QueryRow(ctx, `
		SELECT next_check_at, CURRENT_TIMESTAMP
		FROM monitors
		WHERE id = $1::text::uuid
	`, monitorID).Scan(&nextCheckAt, &databaseTime); err != nil {
		t.Fatalf("read advanced monitor schedule: %v", err)
	}
	if !nextCheckAt.After(databaseTime) {
		t.Fatalf("next check %s is not after database time %s", nextCheckAt, databaseTime)
	}
	advance := nextCheckAt.Sub(previousSchedule)
	interval := time.Duration(intervalSeconds) * time.Second
	if advance < interval || advance%interval != 0 {
		t.Fatalf("schedule advanced by %s, want a positive multiple of %s", advance, interval)
	}
}

func installSchedulerFailureTrigger(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	removeSchedulerFailureTrigger(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION watchtrace_test_reject_schedule_advance() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			IF NEW.name = 'Reject schedule advance' THEN
				RAISE EXCEPTION 'forced scheduler transaction failure';
			END IF;
			RETURN NEW;
		END;
		$function$;

		CREATE TRIGGER watchtrace_test_reject_schedule_advance
		BEFORE UPDATE OF next_check_at ON monitors
		FOR EACH ROW EXECUTE FUNCTION watchtrace_test_reject_schedule_advance();
	`); err != nil {
		t.Fatalf("install scheduler failure trigger: %v", err)
	}
}

func removeSchedulerFailureTrigger(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		DROP TRIGGER IF EXISTS watchtrace_test_reject_schedule_advance ON monitors;
		DROP FUNCTION IF EXISTS watchtrace_test_reject_schedule_advance();
	`); err != nil {
		t.Fatalf("remove scheduler failure trigger: %v", err)
	}
}

func deleteSchedulerTestData(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	slugs []string,
) {
	t.Helper()
	statements := []string{
		`DELETE FROM health_checks WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM check_jobs WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM monitors WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM environments WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM projects WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM org_members WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM organizations WHERE slug = ANY($1::text[])`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement, slugs); err != nil {
			t.Fatalf("delete scheduler test data: %v", err)
		}
	}
}
