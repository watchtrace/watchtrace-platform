package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

func TestGeneratedDatabaseQuery(t *testing.T) {
	databaseURL := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal("create PostgreSQL connection pool")
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatal("connect to PostgreSQL")
	}

	databaseTime, err := database.New(pool).GetDatabaseTime(ctx)
	if err != nil {
		t.Fatalf("execute generated query: %v", err)
	}
	if !databaseTime.Valid {
		t.Fatal("generated query returned a null database time")
	}

	now := time.Now()
	if databaseTime.Time.Before(now.Add(-time.Minute)) || databaseTime.Time.After(now.Add(time.Minute)) {
		t.Fatalf("database time %s is not close to application time %s", databaseTime.Time, now)
	}
}
