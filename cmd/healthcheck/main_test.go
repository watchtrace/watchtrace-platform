package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (function doerFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestCheckAcceptsHealthyResponse(t *testing.T) {
	err := check(context.Background(), doerFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != "http://127.0.0.1:8080/health/ready" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	}), []string{"http://127.0.0.1:8080/health/ready"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
}

func TestCheckRejectsInvalidInputAndUnhealthyResponse(t *testing.T) {
	if err := check(context.Background(), nil, nil); err == nil || err.Error() != usage {
		t.Fatalf("usage error = %v", err)
	}
	if err := check(context.Background(), doerFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("secret transport detail")
	}), []string{"http://127.0.0.1:8080/health"}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("transport error = %v", err)
	}
	if err := check(context.Background(), doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody}, nil
	}), []string{"http://127.0.0.1:8080/health"}); err == nil || err.Error() != "health check returned HTTP 503" {
		t.Fatalf("status error = %v", err)
	}
}
