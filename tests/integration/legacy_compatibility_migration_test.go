package integration_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/db/migrations"
)

const legacyCompatibilitySlug = "legacy-compatibility-upgrade"

func TestLegacyCompatibilitySchema(t *testing.T) {
	ctx, pool := openMonitoringTestPool(t)
	assertLegacyCheckJobColumns(t, ctx, pool, false)
}

func TestLegacyCompatibilityActiveSessionAudit(t *testing.T) {
	if os.Getenv("WATCHTRACE_PREPARE_LEGACY_COMPATIBILITY_UPGRADE") != "1" {
		t.Skip("WATCHTRACE_PREPARE_LEGACY_COMPATIBILITY_UPGRADE is not set")
	}
	ctx, pool := openMonitoringTestPool(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin legacy session audit: %v", err)
	}
	defer tx.Rollback(context.Background())

	var userID string
	if err = tx.QueryRow(ctx, `INSERT INTO users(email,password_hash)
VALUES('legacy-session-audit@watchtrace.test','test-only-hash') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("insert legacy session audit user: %v", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO auth_sessions(
user_id,family_id,token_digest,expires_at,created_at)
VALUES($1::uuid,gen_random_uuid(),$2,CURRENT_TIMESTAMP+INTERVAL '1 hour',
       TIMESTAMPTZ '2026-08-08 17:59:59+00')`, userID, bytes.Repeat([]byte{0x5a}, 32)); err != nil {
		t.Fatalf("insert active pre-cutover session: %v", err)
	}

	migrationSQL, err := migrations.Files.ReadFile("000015_retire_legacy_compatibility.up.sql")
	if err != nil {
		t.Fatalf("read compatibility migration: %v", err)
	}
	guardEnd := strings.Index(string(migrationSQL), "\n$$;")
	if guardEnd < 0 {
		t.Fatal("compatibility migration does not contain the expiry guard")
	}
	_, err = tx.Exec(ctx, string(migrationSQL[:guardEnd+4]))
	if err == nil {
		t.Fatal("compatibility migration accepted an active pre-cutover session")
	}
	if !strings.Contains(err.Error(), "legacy access-session expiry audit failed") {
		t.Fatalf("compatibility migration failed for the wrong reason: %v", err)
	}
}

func TestPrepareLegacyCompatibilityUpgrade(t *testing.T) {
	if os.Getenv("WATCHTRACE_PREPARE_LEGACY_COMPATIBILITY_UPGRADE") != "1" {
		t.Skip("WATCHTRACE_PREPARE_LEGACY_COMPATIBILITY_UPGRADE is not set")
	}
	ctx, pool := openMonitoringTestPool(t)
	deleteMonitoringTestData(t, ctx, pool, []string{legacyCompatibilitySlug})
	organizationID, environmentID := insertMonitoringTenant(t, ctx, pool, legacyCompatibilitySlug)
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	monitorID := insertMonitoringMonitor(t, ctx, pool, organizationID, environmentID, "Legacy state", 60, base)
	insertSchedulePeriod(t, ctx, pool, organizationID, environmentID, monitorID, 1, 60, base, base, base.Add(5*time.Minute))
	if _, err := pool.Exec(ctx, `INSERT INTO alert_rules(
organization_id,environment_id,monitor_id,failure_threshold,recovery_threshold)
VALUES($1::uuid,$2::uuid,$3::uuid,5,2)`, organizationID, environmentID, monitorID); err != nil {
		t.Fatalf("insert legacy alert rule: %v", err)
	}
	for offset := 0; offset < 5; offset++ {
		insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base.Add(time.Duration(offset)*time.Minute), false)
	}
	var stateRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monitor_reliability_states WHERE monitor_id=$1::uuid`, monitorID).Scan(&stateRows); err != nil {
		t.Fatalf("inspect pre-upgrade state: %v", err)
	}
	if stateRows != 0 {
		t.Fatalf("pre-upgrade monitor has %d durable state rows, want 0", stateRows)
	}
	assertLegacyCheckJobColumns(t, ctx, pool, true)
}

func TestLegacyCompatibilityUpgradeBackfillsReliability(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_LEGACY_COMPATIBILITY_UPGRADED") != "1" {
		t.Skip("WATCHTRACE_EXPECT_LEGACY_COMPATIBILITY_UPGRADED is not set")
	}
	ctx, pool := openMonitoringTestPool(t)
	t.Cleanup(func() {
		deleteMonitoringTestData(t, context.Background(), pool, []string{legacyCompatibilitySlug})
	})

	var monitorID, display, observed string
	var failures, successes int
	var lastObserved time.Time
	if err := pool.QueryRow(ctx, `SELECT states.monitor_id::text,states.display_state,states.observed_state,
states.consecutive_failures,states.consecutive_successes,states.last_observed_scheduled_at
FROM monitor_reliability_states AS states
JOIN monitors ON monitors.id=states.monitor_id
JOIN organizations ON organizations.id=monitors.organization_id
WHERE organizations.slug=$1`, legacyCompatibilitySlug).Scan(
		&monitorID, &display, &observed, &failures, &successes, &lastObserved,
	); err != nil {
		t.Fatalf("read backfilled reliability state: %v", err)
	}
	if display != "down" || observed != "down" || failures != 5 || successes != 0 {
		t.Fatalf("backfilled state display=%s observed=%s failures=%d successes=%d", display, observed, failures, successes)
	}

	var evaluations int
	var maximumFailures int
	if err := pool.QueryRow(ctx, `SELECT count(*),max(consecutive_failures)
FROM monitor_result_evaluations WHERE monitor_id=$1::uuid`, monitorID).Scan(&evaluations, &maximumFailures); err != nil {
		t.Fatalf("read backfilled evaluations: %v", err)
	}
	if evaluations != 5 || maximumFailures != 5 {
		t.Fatalf("backfilled evaluations=%d maximum failures=%d", evaluations, maximumFailures)
	}
	if lastObserved.IsZero() {
		t.Fatal("backfilled state has no last observed timestamp")
	}
	assertLegacyCheckJobColumns(t, ctx, pool, false)
}

func TestLegacyCompatibilityMigrationRollback(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_LEGACY_COMPATIBILITY_SCHEMA_RESTORED") != "1" {
		t.Skip("WATCHTRACE_EXPECT_LEGACY_COMPATIBILITY_SCHEMA_RESTORED is not set")
	}
	ctx, pool := openMonitoringTestPool(t)
	assertLegacyCheckJobColumns(t, ctx, pool, true)
}

func assertLegacyCheckJobColumns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wantPresent bool) {
	t.Helper()
	var columns int
	if err := pool.QueryRow(ctx, `SELECT count(*)
FROM information_schema.columns
WHERE table_schema='public' AND table_name='check_jobs'
  AND column_name IN ('attempt_count','max_attempts','lease_owner','lease_token','lease_expires_at')`).Scan(&columns); err != nil {
		t.Fatalf("inspect legacy check-job columns: %v", err)
	}
	want := 0
	if wantPresent {
		want = 5
	}
	if columns != want {
		t.Fatalf("legacy check-job columns=%d, want %d", columns, want)
	}
}
