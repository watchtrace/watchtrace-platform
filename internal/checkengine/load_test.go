package checkengine

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

type timeoutDoer struct{}

func (timeoutDoer) Do(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func TestControlledHundredConcurrentTimeoutsRemainBounded(t *testing.T) {
	const checks = 100
	engine := NewWithClient(timeoutDoer{})
	start := time.Now()
	results := make(chan Result, checks)
	errs := make(chan error, checks)
	var wg sync.WaitGroup
	for i := 0; i < checks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := engine.Execute(context.Background(), Request{JobID: "load-timeout", URL: "https://controlled.invalid", Method: "GET", Timeout: time.Second, ExpectedMin: 200, ExpectedMax: 299, MaxResponseBytes: 1024})
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout storm took %s", elapsed)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("engine returned internal error: %v", err)
		}
	}
	count := 0
	for result := range results {
		count++
		if result.Succeeded || result.ErrorCategory == nil || *result.ErrorCategory != "timeout" {
			t.Fatalf("unexpected timeout result: %+v", result)
		}
	}
	if count != checks {
		t.Fatalf("results=%d", count)
	}
}
