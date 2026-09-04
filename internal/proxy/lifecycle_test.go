package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestServiceShutdownWaitsForAcceptedRequests(t *testing.T) {
	idle := make(chan struct{})
	close(idle)
	service := &Service{idle: idle}
	if !service.beginRequest(httptest.NewRecorder()) {
		t.Fatal("initial request was rejected")
	}
	service.BeginShutdown()

	rejected := httptest.NewRecorder()
	if service.beginRequest(rejected) {
		t.Fatal("request was accepted after shutdown began")
	}
	if rejected.Code != http.StatusServiceUnavailable || rejected.Header().Get("Retry-After") == "" {
		t.Fatalf("shutdown response = %d, headers = %#v", rejected.Code, rejected.Header())
	}

	timedCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := service.WaitForIdle(timedCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForIdle() error = %v, want deadline", err)
	}

	service.endRequest()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	defer cancelDrain()
	if err := service.WaitForIdle(drainCtx); err != nil {
		t.Fatalf("WaitForIdle() after completion: %v", err)
	}
}
