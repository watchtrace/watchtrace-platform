package checker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/destination"
)

func TestExecuteAppliesExpectedStatusAndDiscardsBoundedBody(t *testing.T) {
	var receivedMethod string
	var receivedUserAgent string
	body := &countingBody{remaining: responseDiscardLimit * 2}
	service := newServiceWithHTTPClient(nil, httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		receivedMethod = request.Method
		receivedUserAgent = request.Header.Get("User-Agent")
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       body,
		}, nil
	}))

	result, err := service.execute(context.Background(), claimedJob{
		TargetURL:         "https://public.example/health",
		Method:            http.MethodHead,
		TimeoutSeconds:    5,
		ExpectedStatusMin: 200,
		ExpectedStatusMax: 299,
	})
	if err != nil {
		t.Fatalf("execute check: %v", err)
	}
	if !result.Succeeded || result.StatusCode != http.StatusNoContent || result.ErrorCategory != "" {
		t.Fatalf("result = %+v, want successful 204", result)
	}
	if receivedMethod != http.MethodHead || receivedUserAgent != userAgent {
		t.Fatalf("request method/User-Agent = %q/%q", receivedMethod, receivedUserAgent)
	}
	if body.bytesRead.Load() != responseDiscardLimit {
		t.Fatalf("response bytes read = %d, want bounded %d", body.bytesRead.Load(), responseDiscardLimit)
	}
	if !body.closed.Load() {
		t.Fatal("response body was not closed")
	}
}

func TestExecuteStoresUnexpectedStatusAsFinalFailure(t *testing.T) {
	service := newServiceWithHTTPClient(nil, httpDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("not stored")),
		}, nil
	}))

	result, err := service.execute(context.Background(), claimedJob{
		TargetURL:         "https://public.example/health",
		Method:            http.MethodGet,
		TimeoutSeconds:    5,
		ExpectedStatusMin: 200,
		ExpectedStatusMax: 299,
	})
	if err != nil {
		t.Fatalf("execute check: %v", err)
	}
	if result.Succeeded || result.StatusCode != http.StatusServiceUnavailable ||
		result.ErrorCategory != "unexpected_status" {
		t.Fatalf("result = %+v, want unexpected-status failure", result)
	}
}

func TestExecuteAppliesMonitorTimeout(t *testing.T) {
	service := newServiceWithHTTPClient(nil, httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))

	startedAt := time.Now()
	result, err := service.execute(context.Background(), claimedJob{
		TargetURL:         "https://public.example/slow",
		Method:            http.MethodGet,
		TimeoutSeconds:    1,
		ExpectedStatusMin: 200,
		ExpectedStatusMax: 299,
	})
	if err != nil {
		t.Fatalf("execute timeout: %v", err)
	}
	if result.Succeeded || result.ErrorCategory != "timeout" {
		t.Fatalf("timeout result = %+v", result)
	}
	if elapsed := time.Since(startedAt); elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("monitor timeout elapsed = %s", elapsed)
	}
}

func TestExecuteLeavesCallerCancellationForLeaseRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	service := newServiceWithHTTPClient(nil, httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		cancel()
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))

	_, err := service.execute(ctx, claimedJob{
		TargetURL:         "https://public.example/health",
		Method:            http.MethodGet,
		TimeoutSeconds:    5,
		ExpectedStatusMin: 200,
		ExpectedStatusMax: 299,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation error = %v", err)
	}
}

func TestCategorizeRequestErrorUsesSafeStableCategories(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{err: context.DeadlineExceeded, want: "timeout"},
		{err: destination.ErrUnsafeTarget, want: "unsafe_target"},
		{err: destination.ErrResolutionFailed, want: "dns"},
		{err: destination.ErrConnectionFailed, want: "connection"},
		{err: errors.New("raw target detail must not be stored"), want: "http_protocol"},
	}
	for _, test := range tests {
		if got := categorizeRequestError(test.err); got != test.want {
			t.Errorf("category for %T = %q, want %q", test.err, got, test.want)
		}
	}
}

func TestRunNextRejectsInvalidWorkerIDBeforeDatabaseAccess(t *testing.T) {
	service := NewService(nil)
	for _, workerID := range []string{"", "two words", strings.Repeat("x", 129)} {
		claimed, err := service.RunNext(context.Background(), workerID)
		if claimed || !errors.Is(err, ErrInvalidWorkerID) {
			t.Fatalf("worker %q claimed=%t error=%v", workerID, claimed, err)
		}
	}
}

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (function httpDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

type countingBody struct {
	remaining int
	bytesRead atomic.Int64
	closed    atomic.Bool
}

func (body *countingBody) Read(buffer []byte) (int, error) {
	if body.remaining == 0 {
		return 0, io.EOF
	}
	if len(buffer) > body.remaining {
		buffer = buffer[:body.remaining]
	}
	for index := range buffer {
		buffer[index] = 'x'
	}
	body.remaining -= len(buffer)
	body.bytesRead.Add(int64(len(buffer)))
	return len(buffer), nil
}

func (body *countingBody) Close() error {
	body.closed.Store(true)
	return nil
}
