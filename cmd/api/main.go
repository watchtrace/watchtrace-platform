package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/watchtrace/watchtrace-platform/internal/httpapi"
	"github.com/watchtrace/watchtrace-platform/internal/platform/config"
	"github.com/watchtrace/watchtrace-platform/internal/platform/httpserver"
)

func main() {
	configuration, err := config.Load()
	if err != nil {
		log.Printf("load configuration: %v", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listener, err := net.Listen("tcp", configuration.HTTPAddress)
	if err != nil {
		log.Printf("listen on %s: %v", configuration.HTTPAddress, err)
		os.Exit(1)
	}

	log.Printf("API server listening on %s", listener.Addr())

	server := httpserver.New(httpapi.NewRouter(), configuration.ShutdownTimeout)
	if err := server.Serve(ctx, listener); err != nil {
		log.Printf("API server stopped with an error: %v", err)
		os.Exit(1)
	}
}
