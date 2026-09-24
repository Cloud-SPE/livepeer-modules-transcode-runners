package liverunner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMediaRouterClientReadsPinnedPathShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.RequestURI != "/v3/paths/get/ingest%2Frunner_session_001" {
			t.Errorf("router request URI=%q", request.RequestURI)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"name":"ingest/runner_session_001","online":true,"onlineTime":"2026-08-24T12:00:00Z","inboundBytes":42,"outboundBytes":7,"source":{"id":"publisher-id","type":"rtmpConn"},"futureField":"ignored"}`))
	}))
	defer server.Close()
	client, err := NewMediaRouterClientV1(server.URL, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.Path(context.Background(), "ingest/runner_session_001")
	if err != nil || !status.Online || status.Source == nil || status.Source.Type != "rtmpConn" || status.InboundBytes != 42 {
		t.Fatalf("path status=%+v err=%v", status, err)
	}
}

func TestMediaRouterClientWaitsForPublisher(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		switch attempts.Add(1) {
		case 1:
			http.Error(writer, "not found", http.StatusNotFound)
		case 2:
			_, _ = writer.Write([]byte(`{"name":"ingest/runner_session_001","online":false,"source":null}`))
		default:
			_, _ = writer.Write([]byte(`{"name":"ingest/runner_session_001","online":true,"source":{"id":"publisher-id","type":"rtmpsConn"}}`))
		}
	}))
	defer server.Close()
	client, _ := NewMediaRouterClientV1(server.URL, nil, time.Second)
	status, err := client.WaitForRTMPPublisher(context.Background(), "ingest/runner_session_001", time.Millisecond)
	if err != nil || !status.Online || attempts.Load() != 3 {
		t.Fatalf("wait status=%+v attempts=%d err=%v", status, attempts.Load(), err)
	}
}

func TestMediaRouterClientFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
		err  error
	}{
		{"wrong identity", `{"name":"ingest/other","online":true,"source":{"type":"rtmpConn"}}`, http.StatusOK, nil},
		{"wrong source", `{"name":"ingest/runner_session_001","online":true,"source":{"type":"hlsSource"}}`, http.StatusOK, ErrUnexpectedMediaSourceV1},
		{"server error", `{"error":"do-not-echo"}`, http.StatusServiceUnavailable, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.code)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			client, _ := NewMediaRouterClientV1(server.URL, nil, 50*time.Millisecond)
			if test.err == ErrUnexpectedMediaSourceV1 {
				_, err := client.WaitForRTMPPublisher(context.Background(), "ingest/runner_session_001", time.Millisecond)
				if !errors.Is(err, test.err) {
					t.Fatalf("wait error=%v", err)
				}
				return
			}
			_, err := client.Path(context.Background(), "ingest/runner_session_001")
			if err == nil || strings.Contains(err.Error(), "do-not-echo") || strings.Contains(err.Error(), "ingest/other") {
				t.Fatalf("unsafe router error=%v", err)
			}
		})
	}
}

func TestMediaRouterClientRequiresLoopbackAPI(t *testing.T) {
	if _, err := NewMediaRouterClientV1("http://router.internal:9997", nil, time.Second); err == nil {
		t.Fatal("non-loopback MediaMTX API was accepted")
	}
}

func TestMediaRouterClientKicksRTMPAndRTMPSPublishers(t *testing.T) {
	for _, sourceType := range []string{"rtmpConn", "rtmpsConn"} {
		t.Run(sourceType, func(t *testing.T) {
			var kicked atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					_, _ = writer.Write([]byte(`{"name":"ingest/runner_session_001","online":true,"source":{"id":"connection-001","type":"` + sourceType + `"}}`))
					return
				}
				collection := "rtmpconns"
				if sourceType == "rtmpsConn" {
					collection = "rtmpsconns"
				}
				if request.Method != http.MethodPost || request.RequestURI != "/v3/"+collection+"/kick/connection-001" {
					t.Errorf("kick request=%s %s", request.Method, request.RequestURI)
				}
				kicked.Store(true)
				_, _ = writer.Write([]byte(`{"status":"ok"}`))
			}))
			defer server.Close()
			client, _ := NewMediaRouterClientV1(server.URL, nil, time.Second)
			if err := client.KickPublisher(context.Background(), "ingest/runner_session_001"); err != nil || !kicked.Load() {
				t.Fatalf("kick completed=%v err=%v", kicked.Load(), err)
			}
		})
	}
}

func TestMediaRouterClientTreatsAbsentPublisherKickAsComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "not found", http.StatusNotFound)
	}))
	defer server.Close()
	client, _ := NewMediaRouterClientV1(server.URL, nil, time.Second)
	if err := client.KickPublisher(context.Background(), "ingest/runner_session_001"); err != nil {
		t.Fatalf("absent publisher kick=%v", err)
	}
}

func TestMediaRouterWaitHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "not found", http.StatusNotFound)
	}))
	defer server.Close()
	client, _ := NewMediaRouterClientV1(server.URL, nil, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.WaitForRTMPPublisher(ctx, "ingest/runner_session_001", time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait error=%v", err)
	}
}
