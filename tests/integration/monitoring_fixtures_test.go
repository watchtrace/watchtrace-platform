package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func openMonitoringTestPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create monitoring PostgreSQL pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("connect monitoring PostgreSQL pool: %v", err)
	}
	return ctx, pool
}

func insertMonitoringTenant(
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
		t.Fatalf("insert monitoring organization: %v", err)
	}
	var projectID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO projects (organization_id, name)
		VALUES ($1::text::uuid, 'Monitoring project')
		RETURNING id::text
	`, organizationID).Scan(&projectID); err != nil {
		t.Fatalf("insert monitoring project: %v", err)
	}
	var environmentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO environments (organization_id, project_id, name, environment_type)
		VALUES ($1::text::uuid, $2::text::uuid, 'Production', 'production')
		RETURNING id::text
	`, organizationID, projectID).Scan(&environmentID); err != nil {
		t.Fatalf("insert monitoring environment: %v", err)
	}
	return organizationID, environmentID
}

func insertMonitoringMonitor(
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
		t.Fatalf("insert monitoring monitor: %v", err)
	}
	return monitorID
}

func deleteMonitoringTestData(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	slugs []string,
) {
	t.Helper()
	statements := []string{
		`DELETE FROM monitoring_operational_events WHERE job_id IN (SELECT id FROM check_jobs WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[])))`,
		`DELETE FROM check_result_conflicts WHERE job_id IN (SELECT id FROM check_jobs WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[])))`,
		`DELETE FROM monitor_rollups_daily WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM monitor_rollups_hourly WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM monitoring_coverage_gaps WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM health_checks WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM check_jobs WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM monitors WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM environments WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM projects WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM org_invitations WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM org_members WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM organizations WHERE slug = ANY($1::text[])`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement, slugs); err != nil {
			t.Fatalf("delete monitoring test data: %v", err)
		}
	}
}
