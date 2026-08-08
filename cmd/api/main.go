package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/watchtrace/watchtrace-platform/internal/httpapi"
	"github.com/watchtrace/watchtrace-platform/internal/platform/config"
	"github.com/watchtrace/watchtrace-platform/internal/platform/httpserver"
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

	listener, err := net.Listen("tcp", configuration.HTTPAddress)
	if err != nil {
		logger.Error("listen for API requests", "address", configuration.HTTPAddress, "error", err)
		os.Exit(1)
	}

	logger.Info("API server listening", "address", listener.Addr())

	server := httpserver.New(httpapi.NewRouter(httpapi.Options{Logger: logger}), configuration.ShutdownTimeout)
	if err := server.Serve(ctx, listener); err != nil {
		logger.Error("API server stopped", "error", err)
		os.Exit(1)
	}
}
