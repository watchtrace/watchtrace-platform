package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Server owns the HTTP lifecycle, including bounded graceful shutdown.
type Server struct {
	httpServer      *http.Server
	shutdownTimeout time.Duration
}

// Config controls HTTP timeouts. Zero values use the service defaults.
type Config struct {
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

func New(handler http.Handler, shutdownTimeout time.Duration) *Server {
	return NewConfigured(handler, Config{ShutdownTimeout: shutdownTimeout})
}

// NewConfigured constructs a server for commands that need stricter request
// timeouts than the defaults used by internal health endpoints.
func NewConfigured(handler http.Handler, configuration Config) *Server {
	if configuration.ShutdownTimeout <= 0 {
		configuration.ShutdownTimeout = 10 * time.Second
	}
	if configuration.ReadHeaderTimeout <= 0 {
		configuration.ReadHeaderTimeout = 5 * time.Second
	}
	if configuration.IdleTimeout <= 0 {
		configuration.IdleTimeout = 60 * time.Second
	}
	return &Server{
		httpServer: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: configuration.ReadHeaderTimeout,
			ReadTimeout:       configuration.ReadTimeout,
			WriteTimeout:      configuration.WriteTimeout,
			IdleTimeout:       configuration.IdleTimeout,
		},
		shutdownTimeout: configuration.ShutdownTimeout,
	}
}

// Serve runs until the listener fails or the context is cancelled. On
// cancellation, it gives active requests time to finish before returning.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- s.httpServer.Serve(listener)
	}()

	select {
	case err := <-serveErrors:
		return normalizeServeError(err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		_ = s.httpServer.Close()
		<-serveErrors
		return fmt.Errorf("shut down HTTP server: %w", err)
	}

	return normalizeServeError(<-serveErrors)
}

func normalizeServeError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
