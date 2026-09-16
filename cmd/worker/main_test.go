package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthHandlerSeparatesLivenessAndReadiness(t *testing.T) {
	handler := healthHandler(nil, 0)

	live := httptest.NewRecorder()
	handler.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("liveness status = %d, want %d", live.Code, http.StatusOK)
	}

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d, want %d", ready.Code, http.StatusServiceUnavailable)
	}
}
