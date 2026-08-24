package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/realtime"
)

type eventServiceFake struct {
	mu     sync.Mutex
	after  []int64
	first  chan struct{}
	err    error
	events []realtime.Event
}

func (f *eventServiceFake) Poll(ctx context.Context, _, _ string, after int64, _ int) ([]realtime.Event, error) {
	f.mu.Lock()
	f.after = append(f.after, after)
	call := len(f.after)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if call == 1 {
		if f.first != nil {
			close(f.first)
		}
		return f.events, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestEventStreamReplaysLastEventIDAndEmitsIdentifiersOnly(t *testing.T) {
	service := &eventServiceFake{first: make(chan struct{}), events: []realtime.Event{{ID: 42, Type: "monitor.changed", ResourceType: "monitor", ResourceID: "00000000-0000-0000-0000-000000000042", OccurredAt: time.Now()}}}
	authenticator := &fakeSessionAuthenticator{user: auth.User{ID: "00000000-0000-0000-0000-000000000001", Email: "person@example.test"}}
	router := NewRouter(Options{Authenticator: authenticator, RealtimeService: service, RateLimiter: NewRateLimiter(RateLimits{LiveConnections: 2})})
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/v1/environments/00000000-0000-0000-0000-000000000002/events", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer session")
	request.Header.Set("Last-Event-ID", "41")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { router.ServeHTTP(response, request); close(done) }()
	select {
	case <-service.first:
	case <-time.After(time.Second):
		t.Fatal("stream did not poll")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not stop after disconnect")
	}
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "id: 42\nevent: monitor.changed") || !strings.Contains(body, `"resource_id":"00000000-0000-0000-0000-000000000042"`) || strings.Contains(body, "person@example.test") {
		t.Fatalf("status=%d body=%q", response.Code, body)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.after) == 0 || service.after[0] != 41 {
		t.Fatalf("replay cursors=%v", service.after)
	}
}

func TestEventStreamHidesUnauthorizedEnvironment(t *testing.T) {
	service := &eventServiceFake{err: realtime.ErrNotFound}
	router := NewRouter(Options{Authenticator: &fakeSessionAuthenticator{user: auth.User{ID: "00000000-0000-0000-0000-000000000001"}}, RealtimeService: service})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/environments/00000000-0000-0000-0000-000000000002/events", nil)
	request.Header.Set("Authorization", "Bearer session")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"event_stream_not_found"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEventStreamRejectsInvalidReconnectCursor(t *testing.T) {
	router := NewRouter(Options{Authenticator: &fakeSessionAuthenticator{user: auth.User{ID: "00000000-0000-0000-0000-000000000001"}}, RealtimeService: &eventServiceFake{err: errors.New("must not poll")}})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/environments/00000000-0000-0000-0000-000000000002/events", nil)
	request.Header.Set("Authorization", "Bearer session")
	request.Header.Set("Last-Event-ID", "not-an-id")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
