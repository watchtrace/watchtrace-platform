package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/notification"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	databaseURL := strings.TrimSpace(os.Getenv("WATCHTRACE_DATABASE_URL"))
	if databaseURL == "" {
		logger.Error("notification database configuration missing")
		os.Exit(1)
	}
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		logger.Error("configure notification database")
		os.Exit(1)
	}
	defer db.Close()
	provider, err := configuredProvider()
	if err != nil {
		logger.Error("configure notification provider")
		os.Exit(1)
	}
	workerID := setting("WATCHTRACE_NOTIFICATION_WORKER_ID", "notification-worker-1")
	worker, err := notification.NewWorker(db, provider, notification.Config{WorkerID: workerID, LeaseDuration: 30 * time.Second})
	if err != nil {
		logger.Error("configure notification worker")
		os.Exit(1)
	}
	for ctx.Err() == nil {
		worked, deliveryErr := worker.DeliverNext(ctx)
		if deliveryErr != nil {
			logger.Warn("notification delivery cycle failed")
			waitFor(ctx, time.Second)
		} else if !worked {
			waitFor(ctx, 500*time.Millisecond)
		}
	}
}

func waitFor(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func configuredProvider() (notification.Provider, error) {
	provider := strings.ToLower(setting("WATCHTRACE_NOTIFICATION_PROVIDER", "local"))
	address := setting("WATCHTRACE_NOTIFICATION_SMTP_ADDRESS", "127.0.0.1:1025")
	from := setting("WATCHTRACE_NOTIFICATION_FROM", "watchtrace@localhost")
	if provider == "oci" {
		return notification.NewOCIEmailDeliveryProvider(address,
			strings.TrimSpace(os.Getenv("WATCHTRACE_NOTIFICATION_SMTP_USERNAME")),
			strings.TrimSpace(os.Getenv("WATCHTRACE_NOTIFICATION_SMTP_PASSWORD")), from)
	}
	if provider == "local" {
		return notification.NewLocalSMTPProvider(address, from)
	}
	return nil, notification.ErrInvalidConfiguration
}

func setting(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
