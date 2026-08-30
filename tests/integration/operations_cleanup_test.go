package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/operations"
)

type nestedOperationsDB struct{ tx pgx.Tx }

func (d nestedOperationsDB) Begin(ctx context.Context) (pgx.Tx, error) {
	return d.tx.Begin(ctx)
}

func TestOperationsCleanupUsesTimestampParameters(t *testing.T) {
	ctx, pool := openPhase14Pool(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = operations.New(nestedOperationsDB{tx: tx}).CleanupExpired(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}
