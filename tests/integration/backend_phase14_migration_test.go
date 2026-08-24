package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBackendPhase14SchemaRollback(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_BACKEND_PHASE14_SCHEMA_ABSENT") != "1" {
		t.Skip("WATCHTRACE_EXPECT_BACKEND_PHASE14_SCHEMA_ABSENT is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("WATCHTRACE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, relation := range []string{"api_refresh_events", "audit_logs", "maintenance_status"} {
		var found *string
		if err = pool.QueryRow(ctx, `SELECT to_regclass('public.'||$1)::text`, relation).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if found != nil {
			t.Errorf("%s remains after migration 14 rollback", relation)
		}
	}
}
