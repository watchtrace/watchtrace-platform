package integration_test

import (
	"os"
	"testing"
)

// TestLegacyHTTPCheckWorkerSchemaRollback protects the historical migration
// chain while the legacy PostgreSQL-backed worker implementation remains
// removed from the runtime codebase.
func TestLegacyHTTPCheckWorkerSchemaRollback(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_LEGACY_WORKER_SCHEMA_ABSENT") != "1" {
		t.Skip("WATCHTRACE_EXPECT_LEGACY_WORKER_SCHEMA_ABSENT is not set")
	}

	ctx, pool := openMonitoringTestPool(t)
	var relationName *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.health_checks')::text`).Scan(&relationName); err != nil {
		t.Fatalf("inspect rolled-back health_checks table: %v", err)
	}
	if relationName != nil {
		t.Fatal("health_checks still exists after legacy worker migration rollback")
	}
	var leaseColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = 'check_jobs'
		  AND column_name IN ('attempt_count', 'max_attempts', 'lease_owner', 'lease_token', 'lease_expires_at')
	`).Scan(&leaseColumns); err != nil {
		t.Fatalf("inspect rolled-back legacy worker columns: %v", err)
	}
	if leaseColumns != 0 {
		t.Fatalf("check_jobs retains %d legacy worker columns after rollback", leaseColumns)
	}
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.check_jobs')::text`).Scan(&relationName); err != nil {
		t.Fatalf("inspect preserved check_jobs table: %v", err)
	}
	if relationName == nil {
		t.Fatal("preceding check_jobs table is absent after legacy worker migration rollback")
	}
}
