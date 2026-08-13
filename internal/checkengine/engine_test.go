package checkengine

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"
)

type doer func(*http.Request) (*http.Response, error)

func (f doer) Do(r *http.Request) (*http.Response, error) { return f(r) }
func TestEngineBoundsBodyAndAddsJobID(t *testing.T) {
	var id string
	e := NewWithClient(doer(func(r *http.Request) (*http.Response, error) {
		id = r.Header.Get("X-WatchTrace-Job-ID")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("secret", 100)))}, nil
	}))
	r, err := e.Execute(context.Background(), Request{JobID: "job-1", URL: "https://example.com", Method: "GET", Timeout: time.Second, ExpectedMin: 200, ExpectedMax: 299, MaxResponseBytes: 32})
	if err != nil || r.ErrorCategory == nil || *r.ErrorCategory != "response_too_large" || id != "job-1" {
		t.Fatalf("result=%+v id=%q err=%v", r, id, err)
	}
}
func TestEngineNeverReturnsBody(t *testing.T) {
	e := NewWithClient(doer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("top-secret"))}, nil
	}))
	r, err := e.Execute(context.Background(), Request{JobID: "job", URL: "https://example.com", Method: "HEAD", Timeout: time.Second, ExpectedMin: 200, ExpectedMax: 299, MaxResponseBytes: 64})
	if err != nil || !r.Succeeded {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}

func TestEngineRecordsAvailableNetworkTimings(t *testing.T) {
	e := NewWithClient(doer(func(r *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(r.Context())
		trace.DNSStart(httptrace.DNSStartInfo{})
		trace.DNSDone(httptrace.DNSDoneInfo{})
		trace.ConnectStart("tcp", "controlled.example.test:443")
		trace.ConnectDone("tcp", "controlled.example.test:443", nil)
		trace.TLSHandshakeStart()
		trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
		trace.GotFirstResponseByte()
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))

	result, err := e.Execute(context.Background(), Request{JobID: "timing-job", URL: "https://controlled.example.test", Method: "HEAD", Timeout: time.Second, ExpectedMin: 200, ExpectedMax: 299, MaxResponseBytes: 64})
	if err != nil || !result.Succeeded || result.DNS == nil || result.Connect == nil || result.TLS == nil || result.FirstByte == nil || result.Total < 0 {
		t.Fatalf("timed result=%+v err=%v", result, err)
	}
}

func TestEngineBoundsOneHundredConcurrentTimeouts(t *testing.T) {
	e := NewWithClient(doer(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))

	const checks = 100
	var wait sync.WaitGroup
	errors := make(chan error, checks)
	results := make(chan Result, checks)
	wait.Add(checks)
	for i := 0; i < checks; i++ {
		go func() {
			defer wait.Done()
			result, err := e.Execute(context.Background(), Request{
				JobID: "timeout-job", URL: "https://controlled.example.test",
				Method: "GET", Timeout: time.Second, ExpectedMin: 200,
				ExpectedMax: 299, MaxResponseBytes: 64,
			})
			errors <- err
			results <- result
		}()
	}
	wait.Wait()
	close(errors)
	close(results)
	for err := range errors {
		if err != nil {
			t.Fatalf("timeout execution returned internal error: %v", err)
		}
	}
	for result := range results {
		if result.ErrorCategory == nil || *result.ErrorCategory != "timeout" || result.Succeeded {
			t.Fatalf("timeout result = %+v", result)
		}
	}
}
