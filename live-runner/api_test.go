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
	port := reserveTestPort(t)
	cfg := loadConfig()
	cfg.TempDir = dir
	cfg.PublicHost = "example.com"
	cfg.BrokerToken = "secret"
	cfg.FFmpegBin = writeFakeFFmpeg(t)
	cfg.SessionReadyTimeout = 2 * time.Second
	cfg.RTMPPortStart = port
	cfg.RTMPPortEnd = port
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

input_url = ""
args = sys.argv[1:]
idx = 0
while idx < len(args):
    if args[idx] == "-i" and idx + 1 < len(args):
        input_url = args[idx + 1]
        idx += 2
        continue
    idx += 1

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
        time.sleep(60)
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

func reserveTestPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
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
	ready, err := listenerReady(s.cfg.RTMPPortStart)
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
	if out.PrivateIngestURL != "rtmp://127.0.0.1:1935/live/gws_1234" {
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
		CapabilityID:    "livepeer:transcode/live-gateway-ingest",
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
		CapabilityID:    "livepeer:transcode/live-gateway-ingest",
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
		CapabilityID:    "livepeer:transcode/live-gateway-ingest",
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

type fakeS3Server struct {
	server *httptest.Server
	mu     sync.Mutex
	files  map[string][]byte
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
