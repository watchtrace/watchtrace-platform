package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/operations"
	"github.com/watchtrace/watchtrace-platform/internal/platform/config"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) != 2 || os.Args[1] != "record-backup-success" {
		logger.Error("usage: maintenance record-backup-success")
		os.Exit(2)
	}
	databaseURL, err := config.LoadDatabaseURL()
	if err != nil {
		logger.Error("load database configuration")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		logger.Error("configure database")
		os.Exit(1)
	}
	defer db.Close()
	started := time.Now().UTC()
	if err = operations.New(db).Record(ctx, "backup", started, 1, nil); err != nil {
		logger.Error("record backup status", "category", safeCategory(err))
		os.Exit(1)
	}
	logger.Info("backup success recorded", "completed_at", started)
}

func safeCategory(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "database"
}
