package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newTestServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	cfg := loadConfig()
	cfg.TempDir = dir
	cfg.PublicHost = "example.com"
	cfg.BrokerToken = "secret"
	s, err := newServer(cfg)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return s
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
