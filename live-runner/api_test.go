package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	amf0 "github.com/yutopp/go-amf0"
	flvtag "github.com/yutopp/go-flv/tag"
	rtmp "github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

func newTestServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	cfg := loadConfig()
	cfg.TempDir = dir
	cfg.BrokerToken = "secret"
	cfg.FFmpegBin = writeFakeFFmpeg(t)
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
import os
from pathlib import Path

input_url = ""
output_template = ""
args = sys.argv[1:]
idx = 0
while idx < len(args):
    if args[idx] == "-i" and idx + 1 < len(args):
        input_url = args[idx + 1]
        idx += 2
        continue
    idx += 1

if args:
    output_template = args[-1]

if "://" in input_url:
    port = input_url.split("://", 1)[1].split(":", 1)[1].split("/", 1)[0]
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", int(port)))
    s.listen(1)
    try:
        time.sleep(60)
    finally:
        s.close()
else:
    with open(input_url, "rb", buffering=0) as f:
        try:
            os.read(f.fileno(), 64)
        except Exception:
            pass
        if output_template:
            base_dir = Path(output_template).parent.parent
            variant_dir = base_dir / "v0"
            variant_dir.mkdir(parents=True, exist_ok=True)
            (base_dir / "master.m3u8").write_text("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nv0/playlist.m3u8\n")
            (variant_dir / "playlist.m3u8").write_text("#EXTM3U\n#EXTINF:2.0,\nsegment_000000.ts\n")
            (variant_dir / "segment_000000.ts").write_bytes(b"segment-data")
        time.sleep(60)
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
	s3 := newFakeS3Server()
	defer s3.close()

	s := newTestServer(t)
	body := liveSessionRequest{
		BrokerSessionID: "bsess_1",
		WorkID:          "work_1",
		CapabilityID:    "video:transcode.live",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "test"},
		OutputCredential: &s3OutputCredential{
			Endpoint:        s3.server.URL,
			Region:          "us-east-1",
			Bucket:          "bucket",
			KeyPrefix:       "live-out/a/test/",
			AccessKeyID:     "AKIA_TEST",
			SecretAccessKey: "secret",
			SessionToken:    "token",
			ExpiresAt:       "2026-05-20T22:10:00Z",
		},
		IngestAccept: &liveIngestAcceptance{StreamKey: "gws_test"},
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
	if out.RunnerSessionID == "" || out.PrivateIngestURL == "" {
		t.Fatalf("unexpected create response: %+v", out)
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

func TestCreateSessionRequiresGatewayFields(t *testing.T) {
	s := newTestServer(t)
	body := liveSessionRequest{
		BrokerSessionID: "bsess_2",
		WorkID:          "work_2",
		CapabilityID:    "video:transcode.live",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "missing"},
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/live/sessions", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "output_credential is required") {
		t.Fatalf("unexpected body=%s", rec.Body.String())
	}
}

func TestCreateSessionRejectsExpiredOutputCredential(t *testing.T) {
	s := newTestServer(t)
	body := liveSessionRequest{
		BrokerSessionID: "bsess_expired",
		WorkID:          "work_expired",
		CapabilityID:    "video:transcode.live",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "expired"},
		OutputCredential: &s3OutputCredential{
			Endpoint:        "https://s3-dev.xode.app",
			Region:          "us-east-1",
			Bucket:          "bucket",
			KeyPrefix:       "live-out/a/expired/",
			AccessKeyID:     "AKIA_TEST",
			SecretAccessKey: "secret",
			SessionToken:    "token",
			ExpiresAt:       "2020-01-01T00:00:00Z",
		},
		IngestAccept: &liveIngestAcceptance{StreamKey: "gws_expired"},
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/live/sessions", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "output_credential is already expired") {
		t.Fatalf("unexpected body=%s", rec.Body.String())
	}
}

func TestCreateSessionRejectsMalformedOutputCredentialExpiry(t *testing.T) {
	s := newTestServer(t)
	body := liveSessionRequest{
		BrokerSessionID: "bsess_badexp",
		WorkID:          "work_badexp",
		CapabilityID:    "video:transcode.live",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "badexp"},
		OutputCredential: &s3OutputCredential{
			Endpoint:        "https://s3-dev.xode.app",
			Region:          "us-east-1",
			Bucket:          "bucket",
			KeyPrefix:       "live-out/a/badexp/",
			AccessKeyID:     "AKIA_TEST",
			SecretAccessKey: "secret",
			SessionToken:    "token",
			ExpiresAt:       "not-a-time",
		},
		IngestAccept: &liveIngestAcceptance{StreamKey: "gws_badexp"},
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/live/sessions", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "output_credential.expires_at must be RFC3339") {
		t.Fatalf("unexpected body=%s", rec.Body.String())
	}
}

func TestCreateSessionGatewayIngestMode(t *testing.T) {
	s := newTestServer(t)
	body := liveSessionRequest{
		BrokerSessionID: "bsess_gw",
		WorkID:          "work_gw",
		CapabilityID:    "video:transcode.live",
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
	if out.PrivateIngestURL != "rtmp://127.0.0.1:1935/live/gws_1234" {
		t.Fatalf("private ingest url=%q", out.PrivateIngestURL)
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
	if rt := s.store.get(out.RunnerSessionID); rt != nil {
		rt.stop("test_done")
	}
}

func TestGatewayIngestUploadsAndDeletesSegments(t *testing.T) {
	s3 := newFakeS3Server()
	defer s3.close()

	s := newTestServer(t)
	s.cfg.OutputSyncInterval = 50 * time.Millisecond
	body := liveSessionRequest{
		BrokerSessionID: "bsess_s3",
		WorkID:          "work_s3",
		CapabilityID:    "video:transcode.live",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "gateway"},
		OutputCredential: &s3OutputCredential{
			Endpoint:        s3.server.URL,
			Region:          "us-east-1",
			Bucket:          "bucket",
			KeyPrefix:       "live-out/a/b/",
			AccessKeyID:     "AKIA_TEST",
			SecretAccessKey: "secret",
			SessionToken:    "token",
			ExpiresAt:       "2026-05-20T22:10:00Z",
		},
		IngestAccept: &liveIngestAcceptance{StreamKey: "gws_s3"},
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
	rt := s.store.get(out.RunnerSessionID)
	if rt == nil {
		t.Fatal("session missing from store")
	}

	masterPath := filepath.Join(rt.rec.OutputDir, "master.m3u8")
	segmentPath := filepath.Join(rt.rec.OutputDir, "v0", "segment_000000.ts")
	if err := os.MkdirAll(filepath.Dir(segmentPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(masterPath, []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(segmentPath, []byte("segment"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitForCondition(t, 3*time.Second, func() bool {
		_, ok1 := s3.snapshot()["bucket/live-out/a/b/master.m3u8"]
		_, ok2 := s3.snapshot()["bucket/live-out/a/b/v0/segment_000000.ts"]
		return ok1 && ok2
	})

	if err := os.Remove(segmentPath); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 3*time.Second, func() bool {
		_, ok := s3.snapshot()["bucket/live-out/a/b/v0/segment_000000.ts"]
		return !ok
	})

	got := rt.query()
	if got.Output.PutSuccessCount < 2 {
		t.Fatalf("put success count=%d", got.Output.PutSuccessCount)
	}
	if got.Output.TargetPrefix != "live-out/a/b/" {
		t.Fatalf("target prefix=%q", got.Output.TargetPrefix)
	}
	if rt := s.store.get(out.RunnerSessionID); rt != nil {
		rt.stop("test_done")
	}
}

func TestSharedIngestPublishTransitionsSessionToPublishing(t *testing.T) {
	s3 := newFakeS3Server()
	defer s3.close()

	s := newTestServer(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	shared, err := startSharedIngest(s.store, s.metrics, addr)
	if err != nil {
		t.Fatalf("start shared ingest: %v", err)
	}
	defer shared.Close()
	s.cfg.IngestPublicHost = "127.0.0.1"
	s.cfg.SharedIngestAddr = addr

	body := liveSessionRequest{
		BrokerSessionID: "bsess_publish",
		WorkID:          "work_publish",
		CapabilityID:    "video:transcode.live",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "publish"},
		OutputCredential: &s3OutputCredential{
			Endpoint:        s3.server.URL,
			Region:          "us-east-1",
			Bucket:          "bucket",
			KeyPrefix:       "live-out/a/publish/",
			AccessKeyID:     "AKIA_TEST",
			SecretAccessKey: "secret",
			SessionToken:    "token",
			ExpiresAt:       "2026-05-20T22:10:00Z",
		},
		IngestAccept: &liveIngestAcceptance{StreamKey: "gws_publish"},
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
	rt := s.store.get(out.RunnerSessionID)
	if rt == nil {
		t.Fatal("session missing from store")
	}

	if err := publishViaSharedIngress(addr, rt.rec.StreamKey); err != nil {
		t.Fatalf("publish via shared ingress: %v", err)
	}

	waitForCondition(t, 3*time.Second, func() bool {
		got := rt.query()
		return got.State == statePublishing && got.Ingest.Authenticated && got.Ingest.ConnectedPublisher
	})
	rt.stop("test_done")
}

func TestSharedIngestRejectsUnknownStreamKey(t *testing.T) {
	s3 := newFakeS3Server()
	defer s3.close()

	s := newTestServer(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	shared, err := startSharedIngest(s.store, s.metrics, addr)
	if err != nil {
		t.Fatalf("start shared ingest: %v", err)
	}
	defer shared.Close()
	s.cfg.IngestPublicHost = "127.0.0.1"
	s.cfg.SharedIngestAddr = addr

	body := liveSessionRequest{
		BrokerSessionID: "bsess_reject",
		WorkID:          "work_reject",
		CapabilityID:    "video:transcode.live",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "reject"},
		OutputCredential: &s3OutputCredential{
			Endpoint:        s3.server.URL,
			Region:          "us-east-1",
			Bucket:          "bucket",
			KeyPrefix:       "live-out/a/reject/",
			AccessKeyID:     "AKIA_TEST",
			SecretAccessKey: "secret",
			SessionToken:    "token",
			ExpiresAt:       "2026-05-20T22:10:00Z",
		},
		IngestAccept: &liveIngestAcceptance{StreamKey: "gws_expected"},
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
	rt := s.store.get(out.RunnerSessionID)
	if rt == nil {
		t.Fatal("session missing from store")
	}

	_ = publishViaSharedIngress(addr, "gws_wrong")

	waitForCondition(t, 3*time.Second, func() bool {
		return s.metrics.snapshot().IngestAuthenticationReject >= 1
	})
	got := rt.query()
	if got.State == statePublishing {
		t.Fatalf("unexpected publishing state for rejected stream: %+v", got.Ingest)
	}
	if got.Ingest.Authenticated || got.Ingest.ConnectedPublisher {
		t.Fatalf("unexpected ingest auth state after rejection: %+v", got.Ingest)
	}
	rt.stop("test_done")
}

func TestGatewayIngestEndToEndPublishesAndUploadsHLS(t *testing.T) {
	s3 := newFakeS3Server()
	defer s3.close()

	s := newTestServer(t)
	s.cfg.OutputSyncInterval = 50 * time.Millisecond
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	shared, err := startSharedIngest(s.store, s.metrics, addr)
	if err != nil {
		t.Fatalf("start shared ingest: %v", err)
	}
	defer shared.Close()
	s.cfg.IngestPublicHost = "127.0.0.1"
	s.cfg.SharedIngestAddr = addr

	body := liveSessionRequest{
		BrokerSessionID: "bsess_e2e",
		WorkID:          "work_e2e",
		CapabilityID:    "video:transcode.live",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "e2e"},
		OutputCredential: &s3OutputCredential{
			Endpoint:        s3.server.URL,
			Region:          "us-east-1",
			Bucket:          "bucket",
			KeyPrefix:       "live-out/a/e2e/",
			AccessKeyID:     "AKIA_TEST",
			SecretAccessKey: "secret",
			SessionToken:    "token",
			ExpiresAt:       "2026-05-20T22:10:00Z",
		},
		IngestAccept: &liveIngestAcceptance{StreamKey: "gws_e2e"},
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
	rt := s.store.get(out.RunnerSessionID)
	if rt == nil {
		t.Fatal("session missing from store")
	}

	if err := publishViaSharedIngress(addr, rt.rec.StreamKey); err != nil {
		t.Fatalf("publish via shared ingress: %v", err)
	}

	waitForCondition(t, 3*time.Second, func() bool {
		got := rt.query()
		return got.State == statePublishing && got.Ingest.Authenticated && got.Ingest.ConnectedPublisher
	})
	waitForCondition(t, 3*time.Second, func() bool {
		files := s3.snapshot()
		_, master := files["bucket/live-out/a/e2e/master.m3u8"]
		_, playlist := files["bucket/live-out/a/e2e/v0/playlist.m3u8"]
		_, segment := files["bucket/live-out/a/e2e/v0/segment_000000.ts"]
		return master && playlist && segment
	})

	got := rt.query()
	if got.Output.PutSuccessCount < 3 {
		t.Fatalf("put success count=%d", got.Output.PutSuccessCount)
	}
	if got.Output.LastManifestPutAt == nil {
		t.Fatal("expected manifest upload timestamp")
	}
	if got.Output.LastSegmentPutAt == nil {
		t.Fatal("expected segment upload timestamp")
	}
	rt.stop("test_done")
}

func TestGatewayIngestUploadFailureThresholdAndRecovery(t *testing.T) {
	s3 := newFakeS3Server()
	defer s3.close()

	s := newTestServer(t)
	s.cfg.OutputSyncInterval = 50 * time.Millisecond
	s.cfg.OutputFailureThreshold = 2
	body := liveSessionRequest{
		BrokerSessionID: "bsess_fail",
		WorkID:          "work_fail",
		CapabilityID:    "video:transcode.live",
		OfferingID:      "default",
		SessionParams:   liveSessionParams{Name: "gateway"},
		OutputCredential: &s3OutputCredential{
			Endpoint:        s3.server.URL,
			Region:          "us-east-1",
			Bucket:          "bucket",
			KeyPrefix:       "live-out/a/fail/",
			AccessKeyID:     "AKIA_TEST",
			SecretAccessKey: "secret",
			SessionToken:    "token",
			ExpiresAt:       "2026-05-20T22:10:00Z",
		},
		IngestAccept: &liveIngestAcceptance{StreamKey: "gws_fail"},
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
	rt := s.store.get(out.RunnerSessionID)
	if rt == nil {
		t.Fatal("session missing from store")
	}

	rt.mu.Lock()
	rt.setState(statePublishing, "")
	rt.rec.Ingest.Authenticated = true
	rt.rec.Ingest.ConnectedPublisher = true
	rt.mu.Unlock()

	masterPath := filepath.Join(rt.rec.OutputDir, "master.m3u8")
	if err := os.WriteFile(masterPath, []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s3.failNextPutRequests(2)
	waitForCondition(t, 3*time.Second, func() bool {
		got := rt.query()
		return got.State == stateStalled && got.Output.PutFailureCount >= 2 && got.Output.LastPutError != nil
	})

	if err := os.WriteFile(masterPath, []byte("#EXTM3U\n#EXT-X-VERSION:3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 3*time.Second, func() bool {
		got := rt.query()
		files := s3.snapshot()
		_, uploaded := files["bucket/live-out/a/fail/master.m3u8"]
		return got.State == statePublishing && uploaded && got.Output.LastManifestPutAt != nil
	})

	got := rt.query()
	if got.Output.PutSuccessCount < 1 {
		t.Fatalf("put success count=%d", got.Output.PutSuccessCount)
	}
	if got.Output.PutFailureCount < 2 {
		t.Fatalf("put failure count=%d", got.Output.PutFailureCount)
	}
	rt.stop("test_done")
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

type fakeS3Server struct {
	server       *httptest.Server
	mu           sync.Mutex
	files        map[string][]byte
	failNextPuts int
	putAttempts  int
}

func newFakeS3Server() *fakeS3Server {
	f := &fakeS3Server{files: make(map[string][]byte)}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			f.mu.Lock()
			f.putAttempts++
			if f.failNextPuts > 0 {
				f.failNextPuts--
				f.mu.Unlock()
				http.Error(w, "simulated put failure", http.StatusForbidden)
				return
			}
			f.files[key] = body
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			f.mu.Lock()
			delete(f.files, key)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	return f
}

func (f *fakeS3Server) failNextPutRequests(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNextPuts = n
}

func (f *fakeS3Server) snapshot() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.files))
	for k, v := range f.files {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

func (f *fakeS3Server) close() {
	f.server.Close()
}

func waitForCondition(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func publishViaSharedIngress(addr, streamKey string) error {
	client, err := rtmp.Dial("rtmp", addr, &rtmp.ConnConfig{})
	if err != nil {
		return err
	}
	defer client.Close()

	connect := &rtmpmsg.NetConnectionConnect{
		Command: rtmpmsg.NetConnectionConnectCommand{
			App:           "live",
			TCURL:         "rtmp://" + addr + "/live",
			FlashVer:      "test-publisher",
			Capabilities:  15,
			AudioCodecs:   4071,
			VideoCodecs:   252,
			VideoFunction: 1,
		},
	}
	if err := client.Connect(connect); err != nil {
		return err
	}
	stream, err := client.CreateStream(nil, 128)
	if err != nil {
		return err
	}
	if err := stream.Publish(&rtmpmsg.NetStreamPublish{
		PublishingName: streamKey,
		PublishingType: "live",
	}); err != nil {
		return err
	}

	script := &flvtag.ScriptData{
		Objects: map[string]amf0.ECMAArray{
			"onMetaData": {
				"width":  1280.0,
				"height": 720.0,
			},
		},
	}
	scriptBody := new(bytes.Buffer)
	if err := flvtag.EncodeScriptData(scriptBody, script); err != nil {
		return err
	}
	if err := stream.WriteDataMessage(8, 0, "@setDataFrame", &rtmpmsg.NetStreamSetDataFrame{Payload: scriptBody.Bytes()}); err != nil {
		return err
	}
	if err := stream.Write(5, 1, &rtmpmsg.AudioMessage{Payload: bytes.NewBuffer([]byte{0xAF, 0x00})}); err != nil {
		return err
	}
	if err := stream.Write(6, 2, &rtmpmsg.VideoMessage{Payload: bytes.NewBuffer([]byte{0x17, 0x00, 0x00, 0x00, 0x00})}); err != nil {
		return err
	}
	time.Sleep(250 * time.Millisecond)
	_ = stream.Close()
	return nil
}
