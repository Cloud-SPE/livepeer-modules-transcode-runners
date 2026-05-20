package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	cfg := loadConfig()
	cfg.TempDir = dir
	cfg.PublicHost = "example.com"
	cfg.BrokerToken = "secret"
	cfg.FFmpegBin = writeFakeFFmpeg(t)
	cfg.SessionReadyTimeout = 2 * time.Second
	s, err := newServer(cfg)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return s
}

func writeFakeFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg.py")
	script := `#!/usr/bin/env python3
import socket
import sys
import time

input_url = ""
args = sys.argv[1:]
idx = 0
while idx < len(args):
    if args[idx] == "-i" and idx + 1 < len(args):
        input_url = args[idx + 1]
        idx += 2
        continue
    idx += 1

port = input_url.split("://", 1)[1].split(":", 1)[1].split("/", 1)[0]
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", int(port)))
s.listen(1)
try:
    time.sleep(60)
finally:
    s.close()
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

func TestCreateSessionRequiresAuth(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/live/sessions", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestCreateGetDeleteSession(t *testing.T) {
	s := newTestServer(t)
	body := liveSessionRequest{
		BrokerSessionID: "bsess_1",
		WorkID:          "work_1",
		CapabilityID:    "livepeer:transcode/live-rtmp-hls-abr",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "test"},
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/live/sessions", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out createSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if out.RunnerSessionID == "" || out.Media.Ingest.StreamKey == "" {
		t.Fatalf("unexpected create response: %+v", out)
	}
	if out.Media.Playback.HLSURL != "http://example.com/_hls/"+out.RunnerSessionID+"/master.m3u8" {
		t.Fatalf("unexpected hls url %q", out.Media.Playback.HLSURL)
	}
	ready, err := listenerReady(19350)
	if err != nil {
		t.Fatalf("check listener: %v", err)
	}
	if !ready {
		t.Fatal("rtmp listener not ready")
	}

	getReq := httptest.NewRequest(http.MethodGet, "/v1/video/live/sessions/"+out.RunnerSessionID, nil)
	getReq.SetPathValue("id", out.RunnerSessionID)
	getReq.Header.Set("Authorization", "Bearer secret")
	getRec := httptest.NewRecorder()
	s.routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", getRec.Code, getRec.Body.String())
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/v1/video/live/sessions/"+out.RunnerSessionID, bytes.NewBufferString(`{"reason":"test_end"}`))
	delReq.SetPathValue("id", out.RunnerSessionID)
	delReq.Header.Set("Authorization", "Bearer secret")
	delRec := httptest.NewRecorder()
	s.routes().ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", delRec.Code, delRec.Body.String())
	}
}

func TestCreateSessionUsesForwardedProtoForHLSURL(t *testing.T) {
	s := newTestServer(t)
	body := liveSessionRequest{
		BrokerSessionID: "bsess_2",
		WorkID:          "work_2",
		CapabilityID:    "livepeer:transcode/live-rtmp-hls-abr",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "https"},
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/live/sessions", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out createSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if out.Media.Playback.HLSURL != "https://example.com/_hls/"+out.RunnerSessionID+"/master.m3u8" {
		t.Fatalf("unexpected forwarded hls url %q", out.Media.Playback.HLSURL)
	}
}

func TestCreateSessionUsesConfiguredSchemeForHLSURL(t *testing.T) {
	s := newTestServer(t)
	s.cfg.PublicScheme = "https"
	body := liveSessionRequest{
		BrokerSessionID: "bsess_3",
		WorkID:          "work_3",
		CapabilityID:    "livepeer:transcode/live-rtmp-hls-abr",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "configured"},
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/live/sessions", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out createSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if out.Media.Playback.HLSURL != "https://example.com/_hls/"+out.RunnerSessionID+"/master.m3u8" {
		t.Fatalf("unexpected configured hls url %q", out.Media.Playback.HLSURL)
	}
}

func TestCreateSessionGatewayIngestMode(t *testing.T) {
	s := newTestServer(t)
	body := liveSessionRequest{
		BrokerSessionID: "bsess_gw",
		WorkID:          "work_gw",
		CapabilityID:    "livepeer:transcode/live-gateway-ingest",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "gateway"},
		OutputCredential: &s3OutputCredential{
			Endpoint:        "https://s3-dev.xode.app",
			Region:          "us-east-1",
			Bucket:          "bucket",
			KeyPrefix:       "live-out/a/b/",
			AccessKeyID:     "AKIA_TEST",
			SecretAccessKey: "secret",
			SessionToken:    "token",
			ExpiresAt:       "2026-05-20T22:10:00Z",
		},
		IngestAccept: &liveIngestAcceptance{StreamKey: "gws_1234"},
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/live/sessions", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out createSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if out.PrivateIngestURL != "rtmp://127.0.0.1:19350/live/gws_1234" {
		t.Fatalf("private ingest url=%q", out.PrivateIngestURL)
	}
	if out.Media.Playback.HLSURL != "" {
		t.Fatalf("expected empty hls url, got %q", out.Media.Playback.HLSURL)
	}
	getReq := httptest.NewRequest(http.MethodGet, "/v1/video/live/sessions/"+out.RunnerSessionID, nil)
	getReq.SetPathValue("id", out.RunnerSessionID)
	getReq.Header.Set("Authorization", "Bearer secret")
	getRec := httptest.NewRecorder()
	s.routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var got getSessionResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.Ingest.Mode != modeGatewayIngest {
		t.Fatalf("ingest mode=%q", got.Ingest.Mode)
	}
	if got.Output.Mode != outputModeS3Push {
		t.Fatalf("output mode=%q", got.Output.Mode)
	}
	if got.Output.TargetPrefix != "live-out/a/b/" {
		t.Fatalf("target prefix=%q", got.Output.TargetPrefix)
	}
}

func TestHLSHandlerBlocksTraversal(t *testing.T) {
	dir := t.TempDir()
	h := hlsHandler(dir, "/_hls")
	req := httptest.NewRequest(http.MethodGet, "/_hls/../etc/passwd", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
}

func TestHLSHandlerServesFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sess_a", "master.m3u8")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := hlsHandler(dir, "/_hls")
	req := httptest.NewRequest(http.MethodGet, "/_hls/sess_a/master.m3u8", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHealthIncludesMetrics(t *testing.T) {
	s := newTestServer(t)
	s.metrics.outputCredentialPutSuccess.Add(2)
	s.metrics.outputCredentialPutFailure.Add(1)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Contains(body, []byte(`"output_credential_put_success":2`)) {
		t.Fatalf("body=%s", body)
	}
	if !bytes.Contains(body, []byte(`"output_credential_put_failures":1`)) {
		t.Fatalf("body=%s", body)
	}
}
