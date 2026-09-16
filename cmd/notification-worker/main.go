package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/notification"
	platformconfig "github.com/watchtrace/watchtrace-platform/internal/platform/config"
	"github.com/watchtrace/watchtrace-platform/internal/platform/httpserver"
)

type notificationRuntime struct {
	database      *pgxpool.Pool
	worker        *notification.Worker
	healthAddress string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := runCommand(logger); err != nil {
		logger.Error("notification worker stopped", "error", err)
		os.Exit(1)
	}
}

func runCommand(logger *slog.Logger) error {
	configuration, err := platformconfig.LoadNotificationWorker()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runtime, err := build(ctx, configuration)
	if err != nil {
		return fmt.Errorf("build notification worker: %w", err)
	}
	defer runtime.database.Close()
	return runtime.run(ctx, logger)
}

func build(ctx context.Context, configuration platformconfig.NotificationWorkerConfig) (*notificationRuntime, error) {
	database, err := pgxpool.New(ctx, configuration.DatabaseURL)
	if err != nil {
		return nil, err
	}
	provider, err := configuredProvider(configuration)
	if err != nil {
		database.Close()
		return nil, err
	}
	worker, err := notification.NewWorker(database, provider, notification.Config{
		WorkerID: configuration.WorkerID, LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		database.Close()
		return nil, err
	}
	return &notificationRuntime{database: database, worker: worker, healthAddress: configuration.HealthAddress}, nil
}

func configuredProvider(configuration platformconfig.NotificationWorkerConfig) (notification.Provider, error) {
	if configuration.Provider == "oci" {
		return notification.NewOCIEmailDeliveryProvider(
			configuration.SMTPAddress, configuration.SMTPUsername,
			configuration.SMTPPassword, configuration.From,
		)
	}
	return notification.NewLocalSMTPProvider(configuration.SMTPAddress, configuration.From)
}

func (runtime *notificationRuntime) run(ctx context.Context, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", runtime.healthAddress)
	if err != nil {
		return fmt.Errorf("listen for health checks: %w", err)
	}
	healthDone := make(chan error, 1)
	go func() {
		healthDone <- httpserver.New(healthHandler(runtime.database), 5*time.Second).Serve(ctx, listener)
	}()

	for ctx.Err() == nil {
		select {
		case healthErr := <-healthDone:
			if healthErr != nil {
				return fmt.Errorf("serve health checks: %w", healthErr)
			}
			if ctx.Err() == nil {
				return fmt.Errorf("health server stopped unexpectedly")
			}
			return nil
		default:
		}
		worked, deliveryErr := runtime.worker.DeliverNext(ctx)
		if deliveryErr != nil && ctx.Err() == nil {
			logger.Warn("notification delivery cycle failed")
			waitFor(ctx, time.Second)
		} else if !worked {
			waitFor(ctx, 500*time.Millisecond)
		}
	}
	if healthErr := <-healthDone; healthErr != nil {
		return fmt.Errorf("serve health checks: %w", healthErr)
	}
	return nil
}

type databasePinger interface {
	Ping(context.Context) error
}

func healthHandler(database databasePinger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/health/ready", func(writer http.ResponseWriter, request *http.Request) {
		if database == nil || database.Ping(request.Context()) != nil {
			http.Error(writer, "not_ready", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	return mux
}

func waitFor(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
