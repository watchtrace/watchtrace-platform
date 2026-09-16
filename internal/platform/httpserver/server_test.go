package httpserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestServeGracefullyWaitsForActiveRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	finishRequest := make(chan struct{})
	releaseRequest := sync.OnceFunc(func() { close(finishRequest) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-finishRequest
		w.WriteHeader(http.StatusNoContent)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(releaseRequest)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- New(handler, 2*time.Second).Serve(ctx, listener)
	}()

	responseDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		response, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				responseDone <- fmt.Errorf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
				return
			}
		}
		responseDone <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach the server")
	}

	cancel()
	select {
	case err := <-serveDone:
		t.Fatalf("Serve returned before the active request finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	releaseRequest()
	select {
	case err := <-responseDone:
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish")
	}

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned an error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the request finished")
	}
}

func TestNewConfiguredAppliesGatewayTimeouts(t *testing.T) {
	server := NewConfigured(http.NotFoundHandler(), Config{
		ShutdownTimeout:   3 * time.Second,
		ReadHeaderTimeout: 4 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      6 * time.Second,
		IdleTimeout:       7 * time.Second,
	})

	if server.shutdownTimeout != 3*time.Second ||
		server.httpServer.ReadHeaderTimeout != 4*time.Second ||
		server.httpServer.ReadTimeout != 5*time.Second ||
		server.httpServer.WriteTimeout != 6*time.Second ||
		server.httpServer.IdleTimeout != 7*time.Second {
		t.Fatalf("configured timeouts were not applied: %+v", server.httpServer)
	}
}
