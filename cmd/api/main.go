package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/backendapi"
	"github.com/watchtrace/watchtrace-platform/internal/httpapi"
	"github.com/watchtrace/watchtrace-platform/internal/monitor"
	"github.com/watchtrace/watchtrace-platform/internal/operations"
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
	"github.com/watchtrace/watchtrace-platform/internal/platform/config"
	"github.com/watchtrace/watchtrace-platform/internal/platform/httpserver"
	"github.com/watchtrace/watchtrace-platform/internal/realtime"
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	configuration, err := config.Load()
	if err != nil {
		logger.Error("load configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	databasePool, err := pgxpool.New(ctx, configuration.DatabaseURL)
	if err != nil {
		logger.Error("configure database connection")
		os.Exit(1)
	}
	defer databasePool.Close()
	actionSender, err := configuredActionSender(configuration)
	if err != nil {
		logger.Error("configure account action delivery")
		os.Exit(1)
	}
	authService := auth.NewService(databasePool, actionSender)
	operationsService := operations.New(databasePool)
	go runSessionCleanup(ctx, authService, operationsService, logger)
	ownershipService := ownership.NewService(databasePool, actionSender)
	headerKeys, err := secureheaders.New(configuration.MonitorHeaderKeyVersion, map[int32][]byte{configuration.MonitorHeaderKeyVersion: configuration.MonitorHeaderKey})
	if err != nil {
		logger.Error("configure monitor header encryption")
		os.Exit(1)
	}
	monitorService := monitor.NewServiceWithQueue(databasePool, headerKeys, configuration.PlatformSigningKey, configuration.PlatformSigningKeyID)

	listener, err := net.Listen("tcp", configuration.HTTPAddress)
	if err != nil {
		logger.Error("listen for API requests", "address", configuration.HTTPAddress, "error", err)
		os.Exit(1)
	}

	logger.Info("API server listening", "address", listener.Addr())

	server := httpserver.New(httpapi.NewRouter(httpapi.Options{
		Logger:            logger,
		ReadinessCheck:    databasePool.Ping,
		AuthService:       authService,
		Authenticator:     authService,
		OwnershipService:  ownershipService,
		MonitorService:    monitorService,
		BackendService:    backendapi.New(databasePool),
		RealtimeService:   realtime.New(databasePool),
		OperationsService: operationsService,
		SecureCookies:     configuration.Production,
	}), configuration.ShutdownTimeout)
	if err := server.Serve(ctx, listener); err != nil {
		logger.Error("API server stopped", "error", err)
		os.Exit(1)
	}
}

func configuredActionSender(configuration config.Config) (auth.AccountActionSender, error) {
	if configuration.VerificationProvider == "oci" {
		return auth.NewOCIEmailDeliverySender(
			configuration.VerificationSMTPAddress,
			configuration.VerificationSMTPUsername,
			configuration.VerificationSMTPPassword,
			configuration.VerificationFrom,
			configuration.VerificationURL,
			configuration.PasswordResetURL,
			configuration.InvitationURL,
		)
	}
	return auth.NewLocalSMTPSender(
		configuration.VerificationSMTPAddress,
		configuration.VerificationFrom,
		configuration.VerificationURL,
		configuration.PasswordResetURL,
		configuration.InvitationURL,
	)
}

func runSessionCleanup(ctx context.Context, service *auth.Service, operationsService *operations.Service, logger *slog.Logger) {
	cleanup := func() {
		started := time.Now().UTC()
		count, err := service.CleanupSessions(ctx)
		_ = operationsService.Record(context.Background(), "session_cleanup", started, count, err)
		cleanupCount, cleanupErr := operationsService.CleanupExpired(ctx, started)
		_ = operationsService.Record(context.Background(), "notification_cleanup", started, cleanupCount, cleanupErr)
		if err != nil && ctx.Err() == nil {
			logger.Error("session cleanup failed")
		}
	}
	cleanup()

	ticker := time.NewTicker(auth.DefaultCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}
