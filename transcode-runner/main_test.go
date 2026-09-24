package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckRunnerHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("path = %q, want /healthz", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := checkRunnerHealth(context.Background(), server.Listener.Addr().String()); err != nil {
		t.Fatalf("checkRunnerHealth() error = %v", err)
	}
}

func TestCheckRunnerHealthRejectsUnhealthyStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	if err := checkRunnerHealth(context.Background(), server.Listener.Addr().String()); err == nil {
		t.Fatal("checkRunnerHealth() error = nil, want unhealthy status error")
	}
}

func TestCheckRunnerHealthRejectsInvalidAddress(t *testing.T) {
	if err := checkRunnerHealth(context.Background(), "not-an-address"); err == nil {
		t.Fatal("checkRunnerHealth() error = nil, want address parse error")
	}
}
