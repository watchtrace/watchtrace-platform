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

func New(handler http.Handler, shutdownTimeout time.Duration) *Server {
	return &Server{
		httpServer: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		shutdownTimeout: shutdownTimeout,
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
